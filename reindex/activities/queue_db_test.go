package activities

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

// queue_db_test.go exercises the postgres-bound reindex activities
// (RewireCorpusDocument + DeleteStaleFigures) for the retry- and
// concurrency-safety the queue.go comments claim but nothing verified.
//
// The headline hazard this pins: a concurrent reindex of the same paper
// can move platform.corpus_documents.document_id first, leaving the
// second rewire to match zero rows. RewireCorpusDocument turns that into
// a *non-retryable* "RewireNoOp" application error (so Temporal doesn't
// burn retries on a swap that can never land), and paper.go maps it to a
// Skipped result. If that classification ever regresses — e.g. someone
// makes the zero-row case a plain (retryable) error, or drops the
// non-retryable flag — these tests fail.
//
// DeleteStaleFigures must be idempotent: re-running it (a retry, or a run
// after the R2R delete cascade already cleared the rows) affects zero
// rows and returns nil.
//
// Gated on Docker via core/pgutil.NewTestDB; skips cleanly without it.

const reindexSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS platform;

CREATE TABLE platform.corpus_documents (
    corpus_id   uuid NOT NULL,
    document_id uuid NOT NULL,
    added_by    text,
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (corpus_id, document_id)
);

CREATE TABLE platform.document_figures (
    id          bigserial PRIMARY KEY,
    document_id uuid NOT NULL,
    label       text,
    order_in_doc int
);
`

func newReindexTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if !dockerAvailable(t) {
		t.Skip("docker not available; skipping reindex DB integration test")
	}
	pool := pgutil.NewTestDB(t)
	if _, err := pool.Exec(context.Background(), reindexSchemaDDL); err != nil {
		t.Fatalf("bootstrap reindex schema: %v", err)
	}
	return pool
}

// seedCorpusRow inserts one platform.corpus_documents row pointing at
// oldDoc and returns (corpusID, oldDoc).
func seedCorpusRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	corpus := uuid.New()
	oldDoc := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.corpus_documents (corpus_id, document_id, added_by)
		VALUES ($1, $2, 'seed')`, corpus, oldDoc); err != nil {
		t.Fatalf("seed corpus_documents: %v", err)
	}
	return corpus, oldDoc
}

func pointerDocID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, corpus uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT document_id FROM platform.corpus_documents WHERE corpus_id = $1`, corpus,
	).Scan(&id); err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	return id
}

func TestRewireCorpusDocument_HappyAndIdempotent_DB(t *testing.T) {
	pool := newReindexTestDB(t)
	ctx := context.Background()
	a := &QueueActivities{Pool: pool}

	corpus, oldDoc := seedCorpusRow(t, ctx, pool)
	newDoc := uuid.New()

	// First rewire moves the pointer.
	if err := a.RewireCorpusDocument(ctx, RewireInput{OldDocID: oldDoc.String(), NewDocID: newDoc.String()}); err != nil {
		t.Fatalf("first rewire: %v", err)
	}
	if got := pointerDocID(t, ctx, pool, corpus); got != newDoc {
		t.Fatalf("pointer = %s, want %s after rewire", got, newDoc)
	}

	// Re-running the SAME rewire now matches zero rows (old pointer is
	// already gone) → RewireNoOp, non-retryable. This is the retry-safety
	// contract: a duplicate delivery doesn't double-apply or wedge.
	err := a.RewireCorpusDocument(ctx, RewireInput{OldDocID: oldDoc.String(), NewDocID: newDoc.String()})
	requireRewireNoOp(t, err)

	// Pointer is unchanged by the no-op rewire.
	if got := pointerDocID(t, ctx, pool, corpus); got != newDoc {
		t.Fatalf("pointer mutated by no-op rewire: %s", got)
	}
}

func TestRewireCorpusDocument_BadInput_DB(t *testing.T) {
	// No DB needed for the validation guard, but keep it in the DB suite
	// for locality. A nil pool is fine because we return before any query.
	a := &QueueActivities{}
	if err := a.RewireCorpusDocument(context.Background(),
		RewireInput{OldDocID: "", NewDocID: uuid.NewString()}); err == nil {
		t.Fatal("expected error for empty old_doc_id, got nil")
	}
}

// TestRewireCorpusDocument_ConcurrentSingleWinner is the core
// concurrency assertion: N goroutines race to rewire the SAME corpus row
// to N distinct new doc ids. Exactly one must succeed; the rest must each
// get a non-retryable RewireNoOp. This models two+ reindex workflows
// picking up the same arxiv paper (the DISTINCT ON dedup in QueryWorkSet
// reduces but does not eliminate this — a paper can still be re-queued
// across continuations before the first rewire lands).
func TestRewireCorpusDocument_ConcurrentSingleWinner_DB(t *testing.T) {
	pool := newReindexTestDB(t)
	ctx := context.Background()
	a := &QueueActivities{Pool: pool}

	corpus, oldDoc := seedCorpusRow(t, ctx, pool)

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		noops    int
		otherErr error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		newDoc := uuid.New()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // line everyone up so the UPDATEs genuinely contend
			err := a.RewireCorpusDocument(ctx, RewireInput{
				OldDocID: oldDoc.String(),
				NewDocID: newDoc.String(),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case isRewireNoOp(err):
				noops++
			default:
				otherErr = err
			}
		}()
	}
	close(start)
	wg.Wait()

	if otherErr != nil {
		t.Fatalf("unexpected error from a racer: %v", otherErr)
	}
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
	if noops != racers-1 {
		t.Fatalf("RewireNoOp count = %d, want %d", noops, racers-1)
	}

	// The surviving pointer must be one of the contenders' new ids, never
	// the old one — i.e. the single winner actually committed.
	if got := pointerDocID(t, ctx, pool, corpus); got == oldDoc {
		t.Fatalf("pointer still old (%s) after a winning rewire", got)
	}
}

func TestDeleteStaleFigures_Idempotent_DB(t *testing.T) {
	pool := newReindexTestDB(t)
	ctx := context.Background()
	a := &QueueActivities{Pool: pool}

	doc := uuid.New()
	for i := 0; i < 3; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO platform.document_figures (document_id, label, order_in_doc) VALUES ($1,$2,$3)`,
			doc, "fig", i); err != nil {
			t.Fatalf("seed figure: %v", err)
		}
	}

	// First delete clears the three rows.
	if err := a.DeleteStaleFigures(ctx, doc.String()); err != nil {
		t.Fatalf("first DeleteStaleFigures: %v", err)
	}
	assertFigureCount(t, ctx, pool, doc, 0)

	// Second delete (a retry, or a run after the R2R cascade already
	// cleared the rows) is a clean no-op — no error, still zero rows.
	if err := a.DeleteStaleFigures(ctx, doc.String()); err != nil {
		t.Fatalf("idempotent DeleteStaleFigures: %v", err)
	}
	assertFigureCount(t, ctx, pool, doc, 0)
}

func TestDeleteStaleFigures_BadInput_DB(t *testing.T) {
	a := &QueueActivities{}
	err := a.DeleteStaleFigures(context.Background(), "")
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || !appErr.NonRetryable() || appErr.Type() != "BadInput" {
		t.Fatalf("DeleteStaleFigures(\"\") = %v, want non-retryable BadInput", err)
	}
}

func assertFigureCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, doc uuid.UUID, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.document_figures WHERE document_id = $1`, doc).Scan(&n); err != nil {
		t.Fatalf("count figures: %v", err)
	}
	if n != want {
		t.Fatalf("figure count = %d, want %d", n, want)
	}
}

func isRewireNoOp(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == "RewireNoOp" && appErr.NonRetryable()
}

func requireRewireNoOp(t *testing.T, err error) {
	t.Helper()
	if !isRewireNoOp(err) {
		t.Fatalf("expected non-retryable RewireNoOp, got %v", err)
	}
}

// dockerAvailable mirrors the store package's probe so the reindex
// integration tests Skip (not Fatal) without a Docker daemon.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return true
	}
	for _, sock := range []string{
		"/var/run/docker.sock",
		os.Getenv("XDG_RUNTIME_DIR") + "/docker.sock",
	} {
		if sock == "/docker.sock" {
			continue
		}
		if _, err := os.Stat(sock); err == nil {
			return true
		}
	}
	return false
}

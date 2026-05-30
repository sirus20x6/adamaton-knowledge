package ingestpipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/pgutil"

	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
)

// pipeline_test.go is the integration coverage for the end-to-end
// parse -> chunk -> embed -> persist flow in Run(). Before this file the
// pipeline -- the single correctness path both HTTP ingest entry points
// funnel through -- had zero exercised branches: a reordered stage, a
// dropped sentinel wrap, or a botched batch loop shipped silently.
//
// The tests split into two tiers by what infra they need:
//
//   - Pure (no Docker): every input-validation and parse/chunk failure
//     branch that Run() can reach BEFORE it touches the embedder or the
//     pool. These run everywhere, including CI without a Docker daemon,
//     and they need a real ztok-backed Chunker (so the cgo build is
//     itself under test -- pkg-config locally, static libztok.a in the
//     image). They pass a nil pool and nil embedder on purpose: Run must
//     fail upstream of ever dereferencing them.
//
//   - DB-backed (Docker): the happy path plus the embedder-error and
//     already-ingested branches, which require InsertDocumentWithChunks
//     to reach a real pgvector-capable Postgres. These stand up an
//     ephemeral paradedb via core/pgutil.NewTestDB and a fake
//     OpenAI-shaped embedding sidecar (httptest), and skip gracefully
//     when Docker is absent.
//
// The fake embedder lets us drive the embed branch deterministically --
// success returns 1024-equivalent vectors (we use a small dim; the vec
// column is an unbounded pgvector so dimensionality is free), and the
// error mode returns a 500 so ErrEmbedFailed is exercised without a live
// BGE-M3 sidecar.

// testLogger returns a logrus logger that discards output so test runs
// stay quiet; Run() logs at Info/Warn unconditionally.
func testLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// realChunker builds the process ztok-backed chunker. A failure here
// means the cgo/pkg-config (or static libztok.a) build is broken, which
// is itself one of the things this card asks us to confirm -- so we
// Fatal rather than skip.
func realChunker(t *testing.T) *chunking.Chunker {
	t.Helper()
	c, err := chunking.New("cl100k_base")
	if err != nil {
		t.Fatalf("chunking.New (ztok cgo build broken?): %v", err)
	}
	return c
}

// --- pure failure branches (no Docker, no embedder, no pool) -----------

func TestRun_NilDocumentID(t *testing.T) {
	c := realChunker(t)
	_, err := Run(context.Background(), nil, nil, c, Input{
		DocumentID:  uuid.Nil,
		ContentType: "text/markdown",
		Body:        []byte("# hello"),
	}, testLogger())
	if err == nil {
		t.Fatal("Run with nil document id should error")
	}
	if !isWrapped(err, ErrPersistFailed) {
		t.Fatalf("nil doc id: err = %v, want wrap of ErrPersistFailed", err)
	}
}

func TestRun_EmptyBody(t *testing.T) {
	c := realChunker(t)
	_, err := Run(context.Background(), nil, nil, c, Input{
		DocumentID:  uuid.New(),
		ContentType: "text/markdown",
		Body:        nil,
	}, testLogger())
	if err == nil {
		t.Fatal("Run with empty body should error")
	}
	if !isWrapped(err, ErrParseFailed) {
		t.Fatalf("empty body: err = %v, want wrap of ErrParseFailed", err)
	}
}

func TestRun_UnsupportedContentType(t *testing.T) {
	c := realChunker(t)
	// A content type that Parse() neither matches nor sniffs to a known
	// type. application/zip isn't in the switch and the bytes don't sniff
	// as PDF or gzip, so Parse returns ErrUnsupportedType, which Run must
	// re-wrap as ErrUnsupportedType (NOT ErrParseFailed) so the route maps
	// it to 415.
	_, err := Run(context.Background(), nil, nil, c, Input{
		DocumentID:  uuid.New(),
		ContentType: "application/zip",
		Body:        []byte("PK\x03\x04 not really a zip but not a known magic"),
	}, testLogger())
	if err == nil {
		t.Fatal("Run with unsupported content type should error")
	}
	if !isWrapped(err, ErrUnsupportedType) {
		t.Fatalf("unsupported type: err = %v, want wrap of ErrUnsupportedType", err)
	}
	// And it must NOT be classified as a generic parse failure -- the two
	// sentinels drive different HTTP statuses (415 vs 500).
	if isWrapped(err, ErrParseFailed) {
		t.Fatalf("unsupported type leaked as ErrParseFailed: %v", err)
	}
}

func TestRun_ZeroChunks_WhitespaceOnly(t *testing.T) {
	c := realChunker(t)
	// Whitespace-only markdown parses fine (pass-through) but the chunker
	// returns zero chunks for blank input, so Run must surface
	// ErrZeroChunks BEFORE attempting to embed (embedder is nil here, so
	// reaching the embed stage would panic -- the nil proves we short
	// circuit).
	_, err := Run(context.Background(), nil, nil, c, Input{
		DocumentID:  uuid.New(),
		ContentType: "text/markdown",
		Body:        []byte("   \n\t  \n  "),
	}, testLogger())
	if err == nil {
		t.Fatal("Run on whitespace-only body should error")
	}
	if !isWrapped(err, ErrZeroChunks) {
		t.Fatalf("whitespace body: err = %v, want ErrZeroChunks", err)
	}
}

func TestRun_OversizedInput_ManyChunks_FailsAtEmbed(t *testing.T) {
	c := realChunker(t)
	// A large body parses and chunks into many windows. With a nil
	// embedder, Run must reach the embed stage and we'd panic -- so to
	// exercise "oversized input" deterministically without a panic we use
	// a fake embedder that errors. This confirms a big multi-batch input
	// flows through chunking and hits embedAll's batch loop, and that a
	// batch failure is wrapped as ErrEmbedFailed.
	srv := newFakeEmbedServer(t, fakeEmbedConfig{failAll: true})
	defer srv.Close()
	embedder := embeddings.NewClient(srv.URL, "bge-m3")

	// ~200k chars of varied text -> well over DefaultChunkSize tokens ->
	// multiple embed batches.
	big := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 5000)
	_, err := Run(context.Background(), nil, embedder, c, Input{
		DocumentID:     uuid.New(),
		ContentType:    "text/plain",
		Body:           []byte(big),
		EmbedBatchSize: 16,
	}, testLogger())
	if err == nil {
		t.Fatal("Run with a failing embedder should error")
	}
	if !isWrapped(err, ErrEmbedFailed) {
		t.Fatalf("oversized + embed failure: err = %v, want ErrEmbedFailed", err)
	}
}

// --- DB-backed branches (Docker required) ------------------------------

func TestRun_HappyPath_DB(t *testing.T) {
	pool := newPipelineTestDB(t)
	ctx := context.Background()

	const dim = 8
	srv := newFakeEmbedServer(t, fakeEmbedConfig{dim: dim})
	defer srv.Close()
	embedder := embeddings.NewClient(srv.URL, "bge-m3")
	c := realChunker(t)

	docID := uuid.New()
	body := []byte("# Title\n\n" + strings.Repeat("Some prose about retrieval-augmented generation. ", 80))
	res, err := Run(ctx, pool, embedder, c, Input{
		DocumentID:  docID,
		Title:       "RAG notes",
		ContentType: "text/markdown",
		Body:        body,
		Metadata:    map[string]any{"source": "test"},
	}, testLogger())
	if err != nil {
		t.Fatalf("Run happy path: %v", err)
	}
	if res.DocumentID != docID {
		t.Fatalf("result doc id = %s, want %s", res.DocumentID, docID)
	}
	if res.Chunks < 1 {
		t.Fatalf("result chunks = %d, want >= 1", res.Chunks)
	}
	if res.DocType != "md" {
		t.Fatalf("result doc type = %q, want md", res.DocType)
	}
	if res.TotalTokens <= 0 {
		t.Fatalf("result total tokens = %d, want > 0", res.TotalTokens)
	}
	if res.SizeBytes != len(body) {
		t.Fatalf("result size = %d, want %d", res.SizeBytes, len(body))
	}
	if res.Figures != 0 {
		t.Fatalf("result figures = %d, want 0 (markdown)", res.Figures)
	}

	// The document row landed with ingestion_status='success'.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT ingestion_status FROM deepresearch.documents WHERE id = $1`, docID).Scan(&status); err != nil {
		t.Fatalf("read document row: %v", err)
	}
	if status != "success" {
		t.Fatalf("ingestion_status = %q, want success", status)
	}

	// One chunk row per reported chunk, all with a vec.
	var chunkCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deepresearch.chunks WHERE document_id = $1`, docID).Scan(&chunkCount); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if chunkCount != res.Chunks {
		t.Fatalf("persisted chunks = %d, result reported %d", chunkCount, res.Chunks)
	}

	// MarkStageIndexed ran post-commit -> stage1_at set.
	var stage1Set bool
	if err := pool.QueryRow(ctx,
		`SELECT stage1_at IS NOT NULL FROM platform.document_stages WHERE document_id = $1`, docID).Scan(&stage1Set); err != nil {
		t.Fatalf("read stage row: %v", err)
	}
	if !stage1Set {
		t.Fatal("stage1_at was not set after a successful ingest")
	}
}

func TestRun_EmbedderError_DB(t *testing.T) {
	pool := newPipelineTestDB(t)
	ctx := context.Background()

	srv := newFakeEmbedServer(t, fakeEmbedConfig{failAll: true})
	defer srv.Close()
	embedder := embeddings.NewClient(srv.URL, "bge-m3")
	c := realChunker(t)

	docID := uuid.New()
	_, err := Run(ctx, pool, embedder, c, Input{
		DocumentID:  docID,
		Title:       "doomed",
		ContentType: "text/markdown",
		Body:        []byte("# heading\n\nbody text that produces at least one chunk"),
	}, testLogger())
	if err == nil {
		t.Fatal("Run with a failing embedder should error")
	}
	if !isWrapped(err, ErrEmbedFailed) {
		t.Fatalf("embedder error: err = %v, want ErrEmbedFailed", err)
	}
	// Embed failed BEFORE persist, so nothing should have been written.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deepresearch.documents WHERE id = $1`, docID).Scan(&n); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if n != 0 {
		t.Fatalf("documents written after embed failure = %d, want 0", n)
	}
}

func TestRun_AlreadyIngested_DB(t *testing.T) {
	pool := newPipelineTestDB(t)
	ctx := context.Background()

	const dim = 8
	srv := newFakeEmbedServer(t, fakeEmbedConfig{dim: dim})
	defer srv.Close()
	embedder := embeddings.NewClient(srv.URL, "bge-m3")
	c := realChunker(t)

	docID := uuid.New()
	in := Input{
		DocumentID:  docID,
		Title:       "dup",
		ContentType: "text/markdown",
		Body:        []byte("# dup\n\n" + strings.Repeat("repeat ingest body. ", 40)),
	}
	if _, err := Run(ctx, pool, embedder, c, in, testLogger()); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	// Second ingest of the same id (now ingestion_status='success') must
	// surface ErrAlreadyIngested.
	_, err := Run(ctx, pool, embedder, c, in, testLogger())
	if err == nil {
		t.Fatal("second ingest of same doc id should error")
	}
	if !isWrapped(err, ErrAlreadyIngested) {
		t.Fatalf("re-ingest: err = %v, want ErrAlreadyIngested", err)
	}
}

func TestRun_EmptyBody_DB_NoWrite(t *testing.T) {
	// Empty body fails at the parse guard BEFORE any DB work; assert the
	// short-circuit with a real pool so we know it isn't the nil-pool that
	// "saves" us in the pure test above.
	pool := newPipelineTestDB(t)
	ctx := context.Background()
	c := realChunker(t)

	docID := uuid.New()
	_, err := Run(ctx, pool, nil, c, Input{
		DocumentID:  docID,
		ContentType: "text/markdown",
		Body:        []byte{},
	}, testLogger())
	if !isWrapped(err, ErrParseFailed) {
		t.Fatalf("empty body (DB): err = %v, want ErrParseFailed", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deepresearch.documents WHERE id = $1`, docID).Scan(&n); err != nil {
		t.Fatalf("count documents: %v", err)
	}
	if n != 0 {
		t.Fatalf("documents written on empty body = %d, want 0", n)
	}
}

// --- helpers -----------------------------------------------------------

// isWrapped is errors.Is in a name that reads at the call site. Run wraps
// every failure in one of the package sentinels via %w, so errors.Is is
// the contract the HTTP routes rely on.
func isWrapped(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		err = unwrap(err)
	}
	return false
}

// unwrap is the std errors.Unwrap behavior inlined to avoid importing
// errors just for the one call; keeps the helper self-contained.
func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}

// fakeEmbedConfig drives newFakeEmbedServer's behavior.
type fakeEmbedConfig struct {
	// dim is the dimensionality of returned vectors on the success path.
	// Zero defaults to 8 -- the vec column is an unbounded pgvector, so
	// any dim round-trips; small keeps the JSON tiny.
	dim int
	// failAll makes every request return HTTP 500 so the embeddings client
	// surfaces an error, exercising Run's ErrEmbedFailed branch.
	failAll bool
}

// newFakeEmbedServer stands up an httptest server speaking the OpenAI
// /v1/embeddings shape the embeddings.Client expects: one {"embedding":
// [...]} object per input, in input order. It records the request count
// so tests can assert batching reached the loop.
func newFakeEmbedServer(t *testing.T, cfg fakeEmbedConfig) *httptest.Server {
	t.Helper()
	dim := cfg.dim
	if dim <= 0 {
		dim = 8
	}
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		if cfg.failAll {
			http.Error(w, `{"error":"sidecar down"}`, http.StatusInternalServerError)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Embedding []float64 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{Data: make([]item, len(req.Input))}
		for i := range req.Input {
			vec := make([]float64, dim)
			for j := range vec {
				vec[j] = 0.01 * float64(j+1)
			}
			out.Data[i] = item{Embedding: vec}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	return srv
}

// pipelineSchemaDDL is the minimal slice of the platform + deepresearch
// schema the ingest persist path writes into. It is a hand-rolled subset
// that pins exactly the columns InsertDocumentWithChunks and
// MarkStageIndexed name -- if those queries grow a column, this DDL must
// grow too (the test fails loudly instead of prod erroring at runtime).
//
// vec is an unbounded `vector` (not vector(1024)) so the fake embedder's
// small dim round-trips; the real schema pins 1024 but dimensionality is
// not what these pipeline tests assert.
const pipelineSchemaDDL = `
CREATE EXTENSION IF NOT EXISTS vector;
CREATE SCHEMA IF NOT EXISTS platform;
CREATE SCHEMA IF NOT EXISTS deepresearch;

CREATE TABLE deepresearch.documents (
    id                uuid PRIMARY KEY,
    collection_ids    uuid[]      NOT NULL DEFAULT '{}',
    owner_id          uuid,
    type              text,
    metadata          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    title             text,
    version           text,
    size_in_bytes     bigint,
    ingestion_status  text,
    extraction_status text,
    total_tokens      int,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deepresearch.chunks (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id    uuid NOT NULL,
    owner_id       uuid,
    collection_ids uuid[] NOT NULL DEFAULT '{}',
    vec            vector,
    text           text,
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE platform.document_stages (
    document_id uuid PRIMARY KEY,
    stage       int,
    stage1_at   timestamptz,
    stage2_at   timestamptz,
    stage3_at   timestamptz,
    stage4_at   timestamptz,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
`

// newPipelineTestDB returns a migrated test pool, or skips when Docker
// isn't available. Mirrors store_db_test.go's probe-then-skip so CI
// without a daemon stays green.
func newPipelineTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if !dockerAvailable() {
		t.Skip("docker not available; skipping ingest pipeline DB integration test")
	}
	pool := pgutil.NewTestDB(t)
	if _, err := pool.Exec(context.Background(), pipelineSchemaDDL); err != nil {
		t.Fatalf("bootstrap pipeline schema: %v", err)
	}
	return pool
}

// dockerAvailable reports whether a Docker daemon looks reachable, so the
// DB-backed tests Skip (not Fatal) on machines/CI without it. Same probe
// shape as the store package's dockerAvailable.
func dockerAvailable() bool {
	if os.Getenv("DOCKER_HOST") != "" {
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

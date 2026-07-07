package store

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

// store_db_test.go holds the containerized integration coverage for the
// r2g store layer. Before this file the package's only DB-touching code
// (corpora/sources/documents/figures CRUD + the DeleteDocument cascade +
// the graphrag rollup queries) had ~zero exercised paths, so a column
// rename or a busted JOIN shipped silently. These tests stand up an
// ephemeral paradedb container via core/pgutil.NewTestDB, create the
// minimal slice of the platform/deepresearch schema each query reads,
// and assert real round-trips.
//
// They skip gracefully when Docker isn't reachable (pgutil.NewTestDB
// fatals on a missing daemon, so we probe first via newStoreTestDB which
// recovers the skip). Run locally with Docker up:
//
//	go test ./store/ -run _DB -v
//
// The schema here is deliberately a hand-rolled subset, NOT a migration
// import: r2g ships no migrations of its own (R2R/platform own the
// canonical DDL during the strangler-fig window), so the tests pin the
// exact columns the store SELECTs/INSERTs name. If a store query starts
// reading a new column, the bootstrap below must grow to match — that
// coupling is the point: the test fails loudly instead of the prod query
// erroring at runtime.

// schemaDDL creates just enough of the platform + deepresearch schema for
// the store package's queries. Tables carry only the columns the store
// touches plus the keys its JOINs/cascades rely on.
const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS platform;
CREATE SCHEMA IF NOT EXISTS deepresearch;

CREATE TABLE deepresearch.documents (
    id                       uuid PRIMARY KEY,
    collection_ids           uuid[]      NOT NULL DEFAULT '{}',
    owner_id                 uuid,
    type                     text,
    metadata                 jsonb       NOT NULL DEFAULT '{}'::jsonb,
    title                    text,
    version                  text,
    size_in_bytes            bigint,
    ingestion_status         text,
    extraction_status        text,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    ingestion_attempt_number int,
    total_tokens             int,
    summary                  text,
    summary_embedding        text
);

CREATE TABLE deepresearch.chunks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id uuid NOT NULL
);

-- documents_relationships backs QueryGraphRelationships (the native
-- GET /v3/graphs/{corpus}/relationships). Columns mirror the R2R DDL
-- slice the store reads/writes (see PersistExtraction).
CREATE TABLE deepresearch.documents_relationships (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id   uuid,
    subject     text NOT NULL,
    predicate   text,
    object      text NOT NULL,
    description text,
    subject_id  uuid,
    object_id   uuid,
    weight      double precision,
    chunk_ids   uuid[],
    metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE platform.corpora (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    scope      text,
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE platform.corpus_documents (
    corpus_id   uuid NOT NULL,
    document_id uuid NOT NULL,
    added_by    text,
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (corpus_id, document_id)
);

CREATE TABLE platform.document_stages (
    document_id uuid PRIMARY KEY,
    stage1_at   timestamptz,
    stage2_at   timestamptz,
    stage3_at   timestamptz,
    stage4_at   timestamptz
);

CREATE TABLE platform.document_figures (
    id            bigserial PRIMARY KEY,
    document_id   uuid NOT NULL,
    label         text,
    section_label text,
    caption_tex   text,
    caption_text  text,
    src_path      text,
    raw_format    text,
    raw_bytes     bytea,
    thumbnail_png bytea,
    order_in_doc  int
);

CREATE TABLE platform.sources (
    id            bigserial PRIMARY KEY,
    name          text NOT NULL,
    kind          text,
    domain        text,
    default_weight double precision,
    peer_reviewed boolean DEFAULT false,
    h5_index      int,
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb
);

-- graphrag_* back the QueryGraphRAGSummary / QueryGraphRAGEntities rollup
-- paths. graphrag_entities stores its attributes in a jsonb payload column
-- (title/type/description/degree/frequency) exactly as the store reads
-- them. The four sibling tables only need a corpus_id column so the
-- summary's per-table COUNT(*) subqueries resolve; they stay empty here.
CREATE TABLE platform.graphrag_entities (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    corpus_id uuid NOT NULL,
    payload   jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE platform.graphrag_relationships (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    corpus_id uuid NOT NULL,
    payload   jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE platform.graphrag_communities (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    corpus_id uuid NOT NULL,
    payload   jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE platform.graphrag_community_reports (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    corpus_id uuid NOT NULL,
    payload   jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE platform.graphrag_text_units (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    corpus_id uuid NOT NULL,
    payload   jsonb NOT NULL DEFAULT '{}'::jsonb
);
`

// newStoreTestDB returns a migrated test pool, or skips the test when
// Docker isn't available. pgutil.NewTestDB calls t.Fatalf on a missing
// daemon; we can't intercept that, so we first probe the shared
// container by attempting a dial and converting the failure into a skip.
func newStoreTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if !dockerAvailable(t) {
		t.Skip("docker not available; skipping store DB integration test")
	}
	pool := pgutil.NewTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, schemaDDL); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return pool
}

// seedDocument inserts a minimal deepresearch.documents row and returns
// its id. Optional column overrides keep call sites terse.
func seedDocument(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO deepresearch.documents (id, type, ingestion_status, title)
		VALUES ($1, 'eprint', 'success', $2)
	`, id, "doc "+id.String()[:8]); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	return id
}

func mustCorpus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name, scope string) *Corpus {
	t.Helper()
	c, err := CreateCorpus(ctx, pool, name, scope, map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("CreateCorpus: %v", err)
	}
	return c
}

// --- corpora CRUD -------------------------------------------------------

func TestCorpusCRUD_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	// Create.
	c := mustCorpus(t, ctx, pool, "  Trimmed Name  ", "research")
	if c.Name != "Trimmed Name" {
		t.Fatalf("CreateCorpus did not trim name: %q", c.Name)
	}
	if c.Scope != "research" {
		t.Fatalf("scope = %q, want research", c.Scope)
	}
	if c.DocumentCount != 0 {
		t.Fatalf("fresh corpus DocumentCount = %d, want 0", c.DocumentCount)
	}

	// Get.
	got, err := GetCorpus(ctx, pool, c.ID)
	if err != nil {
		t.Fatalf("GetCorpus: %v", err)
	}
	if got.ID != c.ID || got.Metadata["k"] != "v" {
		t.Fatalf("GetCorpus mismatch: %+v", got)
	}

	// Update name + merge metadata.
	newName := "Renamed"
	upd, err := UpdateCorpus(ctx, pool, c.ID, CorpusUpdate{
		Name:     &newName,
		Metadata: map[string]any{"extra": true},
	})
	if err != nil {
		t.Fatalf("UpdateCorpus: %v", err)
	}
	if upd.Name != "Renamed" {
		t.Fatalf("update name = %q", upd.Name)
	}
	if upd.Metadata["k"] != "v" || upd.Metadata["extra"] != true {
		t.Fatalf("metadata merge failed: %+v", upd.Metadata)
	}

	// List sees it.
	all, err := ListCorpora(ctx, pool)
	if err != nil {
		t.Fatalf("ListCorpora: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("ListCorpora len = %d, want 1", len(all))
	}

	// Soft-delete hides it from List but Get still returns it.
	if err := SoftDeleteCorpus(ctx, pool, c.ID); err != nil {
		t.Fatalf("SoftDeleteCorpus: %v", err)
	}
	all, _ = ListCorpora(ctx, pool)
	if len(all) != 0 {
		t.Fatalf("soft-deleted corpus still listed: %d", len(all))
	}
	// Update on a soft-deleted row misses.
	if _, err := UpdateCorpus(ctx, pool, c.ID, CorpusUpdate{Name: &newName}); err != ErrNoSuchCorpus {
		t.Fatalf("UpdateCorpus on soft-deleted = %v, want ErrNoSuchCorpus", err)
	}
}

func TestCorpus_NotFound_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()
	if _, err := GetCorpus(ctx, pool, uuid.New()); err != ErrNoSuchCorpus {
		t.Fatalf("GetCorpus(missing) = %v, want ErrNoSuchCorpus", err)
	}
	if err := SoftDeleteCorpus(ctx, pool, uuid.New()); err != ErrNoSuchCorpus {
		t.Fatalf("SoftDeleteCorpus(missing) = %v, want ErrNoSuchCorpus", err)
	}
}

// --- corpus_documents attach/list/detach + stage + count ---------------

func TestAttachListDetach_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()
	c := mustCorpus(t, ctx, pool, "C", "research")

	real := seedDocument(t, ctx, pool)
	ghost := uuid.New() // not in deepresearch.documents

	// Attach one real + one missing → attached=1, missing=1.
	attached, skipped, missing, err := AttachDocuments(ctx, pool, c.ID, []uuid.UUID{real, ghost}, "tester")
	if err != nil {
		t.Fatalf("AttachDocuments: %v", err)
	}
	if attached != 1 || skipped != 0 || missing != 1 {
		t.Fatalf("attach counts = a%d s%d m%d, want a1 s0 m1", attached, skipped, missing)
	}

	// Re-attach is idempotent (ON CONFLICT DO NOTHING) → skipped=1.
	attached, skipped, _, err = AttachDocuments(ctx, pool, c.ID, []uuid.UUID{real}, "tester")
	if err != nil {
		t.Fatalf("AttachDocuments re-run: %v", err)
	}
	if attached != 0 || skipped != 1 {
		t.Fatalf("idempotent attach = a%d s%d, want a0 s1", attached, skipped)
	}

	// GetCorpus count reflects the attach.
	got, _ := GetCorpus(ctx, pool, c.ID)
	if got.DocumentCount != 1 {
		t.Fatalf("DocumentCount = %d, want 1", got.DocumentCount)
	}

	// List joins through to the document title.
	name, total, items, err := ListCorpusDocuments(ctx, pool, c.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListCorpusDocuments: %v", err)
	}
	if name != "C" || total != 1 || len(items) != 1 {
		t.Fatalf("list = name%q total%d items%d", name, total, len(items))
	}
	if items[0].DocumentID != real {
		t.Fatalf("listed doc id = %s, want %s", items[0].DocumentID, real)
	}
	if items[0].Type == nil || *items[0].Type != "eprint" {
		t.Fatalf("joined type missing/wrong: %+v", items[0].Type)
	}

	// Detach is idempotent: first removes, second is a no-op.
	removed, err := DetachDocument(ctx, pool, c.ID, real)
	if err != nil || !removed {
		t.Fatalf("DetachDocument first = (%v,%v), want (true,nil)", removed, err)
	}
	removed, err = DetachDocument(ctx, pool, c.ID, real)
	if err != nil || removed {
		t.Fatalf("DetachDocument second = (%v,%v), want (false,nil)", removed, err)
	}
}

func TestCountStages_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()
	c := mustCorpus(t, ctx, pool, "C", "research")

	d1 := seedDocument(t, ctx, pool)
	d2 := seedDocument(t, ctx, pool)
	if _, _, _, err := AttachDocuments(ctx, pool, c.ID, []uuid.UUID{d1, d2}, "t"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// d1 reached stage2, d2 only stage1.
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.document_stages (document_id, stage1_at, stage2_at)
		VALUES ($1, now(), now())`, d1); err != nil {
		t.Fatalf("stage d1: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.document_stages (document_id, stage1_at)
		VALUES ($1, now())`, d2); err != nil {
		t.Fatalf("stage d2: %v", err)
	}

	sc, err := CountStages(ctx, pool, c.ID)
	if err != nil {
		t.Fatalf("CountStages: %v", err)
	}
	if sc.Total != 2 || sc.Stage1 != 2 || sc.Stage2 != 1 || sc.Stage3 != 0 {
		t.Fatalf("stage counts = %+v, want total2 s1=2 s2=1 s3=0", sc)
	}

	// Missing corpus → ErrNoSuchCorpus.
	if _, err := CountStages(ctx, pool, uuid.New()); err != ErrNoSuchCorpus {
		t.Fatalf("CountStages(missing) = %v, want ErrNoSuchCorpus", err)
	}
}

// --- sources CRUD-ish (list/get/filter) --------------------------------

func TestSources_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	seed := []struct {
		name, kind, domain string
		peer               bool
		weight             float64
	}{
		{"arXiv", "preprint", "arxiv.org", false, 0.5},
		{"IEEE Xplore", "journal", "ieee.org", true, 0.9},
		{"Some Blog", "web", "blog.example", false, 0.1},
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx, `
			INSERT INTO platform.sources (name, kind, domain, peer_reviewed, default_weight)
			VALUES ($1,$2,$3,$4,$5)`, s.name, s.kind, s.domain, s.peer, s.weight); err != nil {
			t.Fatalf("seed source %s: %v", s.name, err)
		}
	}

	// Unfiltered list: total is the full table count, ordered weight DESC.
	all, total, err := ListSources(ctx, pool, SourceFilter{})
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("list all = total%d len%d, want 3/3", total, len(all))
	}
	if all[0].Name != "IEEE Xplore" {
		t.Fatalf("ordering: first = %q, want IEEE Xplore (highest weight)", all[0].Name)
	}

	// Kind filter.
	kind := "preprint"
	filtered, total, err := ListSources(ctx, pool, SourceFilter{Kind: &kind})
	if err != nil {
		t.Fatalf("ListSources kind: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Name != "arXiv" {
		t.Fatalf("kind filter = %d rows (%+v)", len(filtered), filtered)
	}
	if total != 3 {
		t.Fatalf("total must stay UNfiltered = %d, want 3", total)
	}

	// PeerReviewed=true filter.
	pr := true
	prRows, _, err := ListSources(ctx, pool, SourceFilter{PeerReviewed: &pr})
	if err != nil {
		t.Fatalf("ListSources peer: %v", err)
	}
	if len(prRows) != 1 || prRows[0].Name != "IEEE Xplore" {
		t.Fatalf("peer filter = %+v", prRows)
	}

	// QSubstring matches name OR domain, case-insensitively.
	q := "ieee"
	qRows, _, err := ListSources(ctx, pool, SourceFilter{QSubstring: &q})
	if err != nil {
		t.Fatalf("ListSources q: %v", err)
	}
	if len(qRows) != 1 || qRows[0].Name != "IEEE Xplore" {
		t.Fatalf("q filter = %+v", qRows)
	}

	// GetSource round-trip + not-found.
	one, err := GetSource(ctx, pool, all[0].ID)
	if err != nil {
		t.Fatalf("GetSource: %v", err)
	}
	if one.Name != "IEEE Xplore" {
		t.Fatalf("GetSource = %q", one.Name)
	}
	if _, err := GetSource(ctx, pool, 999999); err != ErrNoSuchSource {
		t.Fatalf("GetSource(missing) = %v, want ErrNoSuchSource", err)
	}
}

// --- documents read paths ----------------------------------------------

func TestDocuments_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	id := seedDocument(t, ctx, pool)

	got, err := GetDocument(ctx, pool, id)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if got.ID != id {
		t.Fatalf("GetDocument id = %s, want %s", got.ID, id)
	}
	if got.CollectionIDs == nil {
		t.Fatalf("CollectionIDs must be non-nil (empty slice), got nil")
	}

	if _, err := GetDocument(ctx, pool, uuid.New()); err != ErrNotFound {
		t.Fatalf("GetDocument(missing) = %v, want ErrNotFound", err)
	}

	// List returns the row with a correct total_entries.
	rows, total, err := ListDocuments(ctx, pool, ListDocumentsInput{Limit: 10})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("ListDocuments = total%d len%d, want 1/1", total, len(rows))
	}

	// IngestionStat filter that excludes the row.
	failed := "failed"
	rows, total, err = ListDocuments(ctx, pool, ListDocumentsInput{IngestionStat: &failed})
	if err != nil {
		t.Fatalf("ListDocuments filter: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Fatalf("filtered ListDocuments = total%d len%d, want 0/0", total, len(rows))
	}
}

// --- figures insert + DeleteDocument cascade ---------------------------

func TestFiguresAndDeleteCascade_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	id := seedDocument(t, ctx, pool)

	// A chunk hangs off the doc to exercise the cascade's chunk delete.
	if _, err := pool.Exec(ctx,
		`INSERT INTO deepresearch.chunks (document_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}

	n, err := InsertFigures(ctx, pool, []FigureRow{
		{DocumentID: id, Label: "fig1", RawFormat: "png", OrderInDoc: 0, RawBytes: []byte{1, 2, 3}},
		{DocumentID: id, Label: "", RawFormat: "svg", OrderInDoc: 1}, // empty label → NULL
	})
	if err != nil {
		t.Fatalf("InsertFigures: %v", err)
	}
	if n != 2 {
		t.Fatalf("InsertFigures = %d, want 2", n)
	}
	// Empty-string label persisted as SQL NULL.
	var nullLabels int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM platform.document_figures WHERE label IS NULL`).Scan(&nullLabels); err != nil {
		t.Fatalf("count null labels: %v", err)
	}
	if nullLabels != 1 {
		t.Fatalf("null labels = %d, want 1", nullLabels)
	}

	// Empty insert is a no-op.
	if got, err := InsertFigures(ctx, pool, nil); err != nil || got != 0 {
		t.Fatalf("InsertFigures(nil) = (%d,%v), want (0,nil)", got, err)
	}

	// DeleteDocument removes figures + chunks + the document in one tx.
	affected, err := DeleteDocument(ctx, pool, id)
	if err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if affected != 1 {
		t.Fatalf("DeleteDocument affected = %d, want 1", affected)
	}
	assertRowCount(t, ctx, pool, "platform.document_figures", 0)
	assertRowCount(t, ctx, pool, "deepresearch.chunks", 0)
	assertRowCount(t, ctx, pool, "deepresearch.documents", 0)

	// Deleting a missing doc → 0 affected, no error (handler maps to 404).
	affected, err = DeleteDocument(ctx, pool, uuid.New())
	if err != nil {
		t.Fatalf("DeleteDocument(missing): %v", err)
	}
	if affected != 0 {
		t.Fatalf("DeleteDocument(missing) affected = %d, want 0", affected)
	}
}

// --- graphrag rollup paths ---------------------------------------------

func TestGraphRAGRollup_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	// CorpusExists checks platform.corpora (NOT the graphrag tables), so
	// stand up a real corpus row first.
	c := mustCorpus(t, ctx, pool, "GraphRAG Corpus", "research")
	corpus := c.ID
	other := uuid.New()

	// graphrag_entities attributes live in a jsonb payload; the store
	// reads title/type/description/degree off payload->>...
	for _, e := range []struct {
		corpus     uuid.UUID
		title      string
		entityType string
		degree     int
	}{
		{corpus, "Transformer", "architecture", 9},
		{corpus, "Attention", "mechanism", 4},
		{other, "Noise", "misc", 1}, // different corpus, must not bleed in
	} {
		// degree is stored as a string in the payload — the store reads it
		// via (payload->>'degree')::int guarded by a ^-?[0-9]+$ regex.
		if _, err := pool.Exec(ctx, `
			INSERT INTO platform.graphrag_entities (corpus_id, payload)
			VALUES ($1, jsonb_build_object('title',$2::text,'type',$3::text,'degree',$4::text))`,
			e.corpus, e.title, e.entityType, strconv.Itoa(e.degree)); err != nil {
			t.Fatalf("seed graphrag entity: %v", err)
		}
	}

	ok, err := CorpusExists(ctx, pool, corpus)
	if err != nil {
		t.Fatalf("CorpusExists: %v", err)
	}
	if !ok {
		t.Fatalf("CorpusExists(seeded) = false, want true")
	}
	ok, _ = CorpusExists(ctx, pool, uuid.New())
	if ok {
		t.Fatalf("CorpusExists(unknown) = true, want false")
	}

	// Summary rollup counts only this corpus's entities (the other
	// graphrag_* tables exist but are empty).
	sum, err := QueryGraphRAGSummary(ctx, pool, corpus)
	if err != nil {
		t.Fatalf("QueryGraphRAGSummary: %v", err)
	}
	if sum.Entities != 2 {
		t.Fatalf("summary entities = %d, want 2 (corpus-scoped)", sum.Entities)
	}
	if sum.Relationships != 0 || sum.Communities != 0 {
		t.Fatalf("summary non-entity counts = rel%d comm%d, want 0/0", sum.Relationships, sum.Communities)
	}

	// Entity rollup list ordered by degree DESC; needle filter narrows.
	ents, err := QueryGraphRAGEntities(ctx, pool, corpus, "", 100)
	if err != nil {
		t.Fatalf("QueryGraphRAGEntities: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("entities = %d, want 2", len(ents))
	}
	if ents[0].Title != "Transformer" {
		t.Fatalf("degree ordering: first = %q, want Transformer (degree 9)", ents[0].Title)
	}
	hit, err := QueryGraphRAGEntities(ctx, pool, corpus, "trans", 100)
	if err != nil {
		t.Fatalf("QueryGraphRAGEntities needle: %v", err)
	}
	if len(hit) != 1 || hit[0].Title != "Transformer" {
		t.Fatalf("needle filter = %+v, want one Transformer", hit)
	}
}

// dockerAvailable reports whether a Docker daemon looks reachable, so the
// integration tests can Skip (not Fatal) on machines/CI without it. We
// probe the conventional endpoints rather than shelling out: DOCKER_HOST
// when set, else the default unix socket. This is best-effort — if the
// socket exists but the daemon is wedged, testcontainers will surface the
// real error, which is the right signal.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		// A configured DOCKER_HOST means the operator intends Docker to be
		// used; let testcontainers do the authoritative reachability check.
		return true
	}
	for _, sock := range []string{
		"/var/run/docker.sock",
		os.Getenv("XDG_RUNTIME_DIR") + "/docker.sock",
	} {
		if sock == "/docker.sock" {
			continue // XDG_RUNTIME_DIR unset
		}
		if _, err := os.Stat(sock); err == nil {
			return true
		}
	}
	return false
}

func assertRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if n != want {
		t.Fatalf("count %s = %d, want %d", table, n, want)
	}
}

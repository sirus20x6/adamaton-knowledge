package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/extract"
)

// TestPersistEntityGraph_DB exercises the canonical-entity + bi-temporal
// relation writer against a live, migrated deepresearch schema. Gated on
// R2G_TEST_DSN so it skips in CI (which has no Postgres). Run locally:
//
//	R2G_TEST_DSN="postgres://postgres:postgres@localhost:5432/r2g_migtest?sslmode=disable" \
//	  go test ./store/ -run TestPersistEntityGraph_DB -v
func TestPersistEntityGraph_DB(t *testing.T) {
	dsn := os.Getenv("R2G_TEST_DSN")
	if dsn == "" {
		t.Skip("R2G_TEST_DSN not set; skipping DB integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Clean slate.
	for _, tbl := range []string{
		"deepresearch.entity_relations", "deepresearch.episode_mentions",
		"deepresearch.document_mentions", "deepresearch.entities",
		"deepresearch.documents",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	// Two documents to act as sources.
	docA := uuid.New()
	docB := uuid.New()
	for _, id := range []uuid.UUID{docA, docB} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO deepresearch.documents (id, type, kind) VALUES ($1, 'txt', 'chunk')`, id,
		); err != nil {
			t.Fatalf("seed doc: %v", err)
		}
	}

	res := &extract.Result{
		Entities: []extract.Entity{
			{Name: "pgvector", Category: "library"},
			{Name: "PGVector", Category: "library"}, // case-variant dup
			{Name: "octen", Category: "service"},
		},
		Relationships: []extract.Relationship{
			{Subject: "octen", Predicate: "emits", Object: "pgvector", Weight: 0.9},
		},
	}
	now := time.Now()

	// First persist from docA.
	ents, rels, err := PersistEntityGraph(ctx, pool, KGSource{Kind: SourceDocument, ID: docA}, res, now)
	if err != nil {
		t.Fatalf("persist A: %v", err)
	}
	if ents != 2 || rels != 1 {
		t.Fatalf("persist A counts: ents=%d rels=%d, want 2/1", ents, rels)
	}
	assertCount(t, ctx, pool, "deepresearch.entities", 2)
	assertCount(t, ctx, pool, "deepresearch.entity_relations", 1)
	assertCount(t, ctx, pool, "deepresearch.document_mentions", 2)

	// Re-persist same source: idempotent. No new entities/relations, no
	// duplicate evidence, mentions reset (delete-then-insert).
	if _, _, err := PersistEntityGraph(ctx, pool, KGSource{Kind: SourceDocument, ID: docA}, res, now); err != nil {
		t.Fatalf("persist A again: %v", err)
	}
	assertCount(t, ctx, pool, "deepresearch.entities", 2)
	assertCount(t, ctx, pool, "deepresearch.entity_relations", 1)
	assertCount(t, ctx, pool, "deepresearch.document_mentions", 2)
	assertEvidenceLen(t, ctx, pool, 1) // still one evidence doc

	// Persist same relation from docB: evidence appended, still one edge.
	if _, _, err := PersistEntityGraph(ctx, pool, KGSource{Kind: SourceDocument, ID: docB}, res, now); err != nil {
		t.Fatalf("persist B: %v", err)
	}
	assertCount(t, ctx, pool, "deepresearch.entity_relations", 1)
	assertCount(t, ctx, pool, "deepresearch.document_mentions", 4) // 2 per doc
	assertEvidenceLen(t, ctx, pool, 2)                             // docA + docB

	// predicate_norm normalized.
	var pnorm string
	if err := pool.QueryRow(ctx,
		`SELECT predicate_norm FROM deepresearch.entity_relations LIMIT 1`).Scan(&pnorm); err != nil {
		t.Fatalf("read predicate_norm: %v", err)
	}
	if pnorm != "emits" {
		t.Fatalf("predicate_norm = %q, want emits", pnorm)
	}
}

func assertCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if n != want {
		t.Fatalf("count %s = %d, want %d", table, n, want)
	}
}

func assertEvidenceLen(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT coalesce(array_length(evidence_doc_ids,1),0) FROM deepresearch.entity_relations LIMIT 1`).Scan(&n); err != nil {
		t.Fatalf("evidence len: %v", err)
	}
	if n != want {
		t.Fatalf("evidence_doc_ids length = %d, want %d", n, want)
	}
}

package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRollupStore_DB exercises the rollup persistence layer against a
// live migrated schema. Gated on R2G_TEST_DSN (skips in CI).
func TestRollupStore_DB(t *testing.T) {
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

	if _, err := pool.Exec(ctx, "DELETE FROM deepresearch.documents"); err != nil {
		t.Fatalf("clean: %v", err)
	}

	// Seed three level-0 leaves with embeddings and one without (must be
	// excluded by LoadRollupLeaves).
	emb := "[" + strings.TrimSuffix(strings.Repeat("0.1,", 1024), ",") + "]"
	var leafIDs []uuid.UUID
	for i := 0; i < 3; i++ {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `
			INSERT INTO deepresearch.documents (id, type, kind, level, summary, summary_embedding)
			VALUES ($1, 'txt', 'chunk', 0, $2, $3::vector)
		`, id, fmt.Sprintf("leaf %d", i), emb); err != nil {
			t.Fatalf("seed leaf: %v", err)
		}
		leafIDs = append(leafIDs, id)
	}
	// No-embedding leaf — should not load.
	if _, err := pool.Exec(ctx, `
		INSERT INTO deepresearch.documents (id, type, kind, level, summary)
		VALUES ($1, 'txt', 'chunk', 0, 'no embed')
	`, uuid.New()); err != nil {
		t.Fatalf("seed no-embed leaf: %v", err)
	}

	leaves, err := LoadRollupLeaves(ctx, pool, "chunk")
	if err != nil {
		t.Fatalf("LoadRollupLeaves: %v", err)
	}
	if len(leaves) != 3 {
		t.Fatalf("loaded %d leaves, want 3 (no-embed excluded)", len(leaves))
	}
	for _, l := range leaves {
		if len(l.Embedding) != 1024 {
			t.Fatalf("leaf embedding dim = %d, want 1024", len(l.Embedding))
		}
		if l.Content == "" {
			t.Fatalf("leaf content empty")
		}
	}

	// Insert a level-1 summary over the three leaves and wire parents.
	sumID, err := InsertSummaryDoc(ctx, pool, "chunk", 1, "summary of three leaves", nil, leafIDs)
	if err != nil {
		t.Fatalf("InsertSummaryDoc: %v", err)
	}
	if err := SetDocumentParent(ctx, pool, leafIDs, sumID); err != nil {
		t.Fatalf("SetDocumentParent: %v", err)
	}

	var parented int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM deepresearch.documents WHERE parent_id = $1`, sumID).Scan(&parented); err != nil {
		t.Fatalf("count parented: %v", err)
	}
	if parented != 3 {
		t.Fatalf("parented = %d, want 3", parented)
	}
	// Summary is excluded from a re-load of leaves (level >= 1).
	leaves2, _ := LoadRollupLeaves(ctx, pool, "chunk")
	if len(leaves2) != 3 {
		t.Fatalf("reload leaves = %d, want 3 (summary excluded)", len(leaves2))
	}

	// Idempotency: DeleteRollupSummaries removes the summary, detaches
	// children, preserves leaves.
	if err := DeleteRollupSummaries(ctx, pool, "chunk"); err != nil {
		t.Fatalf("DeleteRollupSummaries: %v", err)
	}
	var summaries, leafCount, stillParented int
	pool.QueryRow(ctx, `SELECT count(*) FROM deepresearch.documents WHERE kind='chunk' AND level>=1`).Scan(&summaries)
	pool.QueryRow(ctx, `SELECT count(*) FROM deepresearch.documents WHERE kind='chunk' AND level=0`).Scan(&leafCount)
	pool.QueryRow(ctx, `SELECT count(*) FROM deepresearch.documents WHERE parent_id IS NOT NULL`).Scan(&stillParented)
	if summaries != 0 {
		t.Fatalf("summaries after delete = %d, want 0", summaries)
	}
	if leafCount != 4 {
		t.Fatalf("leaves after delete = %d, want 4 (preserved)", leafCount)
	}
	if stillParented != 0 {
		t.Fatalf("still-parented after delete = %d, want 0 (detached)", stillParented)
	}
}

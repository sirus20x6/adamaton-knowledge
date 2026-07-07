package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedRelationship inserts one documents_relationships row for parent.
func seedRelationship(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	parent uuid.UUID, subject, predicate, object string, weight float64,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	subjectID := uuid.New()
	objectID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO deepresearch.documents_relationships
		  (id, parent_id, subject, predicate, object, description,
		   subject_id, object_id, weight, chunk_ids, metadata)
		VALUES ($1, $2, $3, $4, $5, 'desc', $6, $7, $8,
		        ARRAY[gen_random_uuid()], '{"src":"test"}'::jsonb)
	`, id, parent, subject, predicate, object, subjectID, objectID, weight); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}
	return id
}

// TestQueryGraphRelationships_DB pins the corpus scoping of the native
// /v3/graphs/{corpus}/relationships read: rows are in-corpus via EITHER
// documents.collection_ids OR platform.corpus_documents, out-of-corpus
// rows never leak, and total_entries reflects the pre-LIMIT count.
func TestQueryGraphRelationships_DB(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	corpus := uuid.New()
	otherCorpus := uuid.New()

	// docA: in corpus via collection_ids (the /v3/documents attach path).
	docA := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO deepresearch.documents (id, type, collection_ids)
		VALUES ($1, 'txt', ARRAY[$2::uuid])
	`, docA, corpus); err != nil {
		t.Fatalf("seed docA: %v", err)
	}
	// docB: in corpus via platform.corpus_documents (the platform attach path).
	docB := seedDocument(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO platform.corpus_documents (corpus_id, document_id)
		VALUES ($1, $2)
	`, corpus, docB); err != nil {
		t.Fatalf("attach docB: %v", err)
	}
	// docC: belongs to a different corpus only.
	docC := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO deepresearch.documents (id, type, collection_ids)
		VALUES ($1, 'txt', ARRAY[$2::uuid])
	`, docC, otherCorpus); err != nil {
		t.Fatalf("seed docC: %v", err)
	}

	seedRelationship(t, ctx, pool, docA, "Booth", "KILLS", "Lincoln", 0.9)
	seedRelationship(t, ctx, pool, docB, "Lincoln", "PRESIDENT_OF", "USA", 0.7)
	seedRelationship(t, ctx, pool, docC, "Foreign", "LEAKS_INTO", "Corpus", 1.0)

	rows, total, err := QueryGraphRelationships(ctx, pool, corpus, 100, 0)
	if err != nil {
		t.Fatalf("QueryGraphRelationships: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	// Ordered weight DESC: Booth first.
	if rows[0].Subject != "Booth" || rows[1].Subject != "Lincoln" {
		t.Errorf("order = %q, %q; want Booth, Lincoln", rows[0].Subject, rows[1].Subject)
	}
	for _, r := range rows {
		if r.Subject == "Foreign" {
			t.Error("out-of-corpus row leaked into results")
		}
		if r.SubjectID == nil || r.ObjectID == nil {
			t.Errorf("row %s missing subject_id/object_id", r.Subject)
		}
		if len(r.ChunkIDs) != 1 {
			t.Errorf("row %s chunk_ids = %v", r.Subject, r.ChunkIDs)
		}
		if r.Metadata["src"] != "test" {
			t.Errorf("row %s metadata = %v", r.Subject, r.Metadata)
		}
		if r.Description != "desc" {
			t.Errorf("row %s description = %q", r.Subject, r.Description)
		}
	}

	// Pagination: limit 1 offset 1 returns the second row, total still 2.
	page, total2, err := QueryGraphRelationships(ctx, pool, corpus, 1, 1)
	if err != nil {
		t.Fatalf("paged query: %v", err)
	}
	if total2 != 2 || len(page) != 1 || page[0].Subject != "Lincoln" {
		t.Errorf("page = %+v total = %d; want [Lincoln] 2", page, total2)
	}

	// Unknown corpus: empty results, zero total, no error.
	none, total3, err := QueryGraphRelationships(ctx, pool, uuid.New(), 100, 0)
	if err != nil {
		t.Fatalf("empty corpus query: %v", err)
	}
	if len(none) != 0 || total3 != 0 {
		t.Errorf("expected empty result for unknown corpus, got %d/%d", len(none), total3)
	}
}

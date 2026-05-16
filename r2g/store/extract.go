package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/extract"
)

// PersistExtraction wipes any prior entities + relationships for the
// document and inserts the new sets. Runs in one transaction so the
// document_entities / _relationships pair stays consistent if the
// transaction aborts.
//
// Empty-result guard: if both res.Entities and res.Relationships are
// empty, this function is a NO-OP (returns 0, 0, nil without touching
// either table). That keeps previously-extracted graph data intact when
// the LLM hiccups and returns `{"entities": []}` for a doc it had
// previously produced entities for. Callers should treat such empty
// results as a soft failure upstream (see internal/extract.Run's
// suspect-empty handling), but this is defense-in-depth: even if a
// future caller bypasses that check, real data won't get wiped.
//
// Also flips the document's extraction_status to 'success' so the
// dashboard's "graphed?" filter (`extraction_status='success' AND
// has entities`) lights up.
//
// The description_embedding column is left NULL in this phase. Adding
// per-entity embeddings is a future optimisation -- it triples the
// LLM-call cost (one embed per entity) and the dashboard's current
// graph view doesn't use it.
func PersistExtraction(
	ctx context.Context,
	pool *pgxpool.Pool,
	docID uuid.UUID,
	chunkIDs []uuid.UUID,
	res *extract.Result,
) (int, int, error) {
	// Empty-result no-op: preserve prior extraction. See doc comment.
	if len(res.Entities) == 0 && len(res.Relationships) == 0 {
		return 0, 0, nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Wipe existing graph data for this doc -- both tables CASCADE on
	// documents.id so DELETE FROM documents is the usual cleanup path,
	// but we don't want to delete the document itself here. Manual
	// scoped delete is safe because re-extraction is idempotent: every
	// call replaces the prior entities/relationships wholesale.
	if _, err := tx.Exec(ctx,
		`DELETE FROM deepresearch.documents_entities WHERE parent_id = $1`, docID); err != nil {
		return 0, 0, fmt.Errorf("clear entities: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM deepresearch.documents_relationships WHERE parent_id = $1`, docID); err != nil {
		return 0, 0, fmt.Errorf("clear relationships: %w", err)
	}

	// Insert entities. Map name -> id so relationship subject_id /
	// object_id can be filled in.
	entityIDs := map[string]uuid.UUID{}
	for _, e := range res.Entities {
		eid := uuid.New()
		entityIDs[normalizeName(e.Name)] = eid
		if _, err := tx.Exec(ctx, `
			INSERT INTO deepresearch.documents_entities
			  (id, parent_id, name, category, description, chunk_ids, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb, now(), now())
		`, eid, docID, e.Name, nullIfEmpty(e.Category), nullIfEmpty(e.Description), chunkIDs); err != nil {
			return 0, 0, fmt.Errorf("insert entity %q: %w", e.Name, err)
		}
	}

	// Insert relationships. subject_id / object_id link back to
	// entities by lowercased name when present; left NULL otherwise
	// (the dashboard's graph view tolerates either).
	for _, r := range res.Relationships {
		var subjectID, objectID *uuid.UUID
		if id, ok := entityIDs[normalizeName(r.Subject)]; ok {
			subjectID = &id
		}
		if id, ok := entityIDs[normalizeName(r.Object)]; ok {
			objectID = &id
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO deepresearch.documents_relationships
			  (id, parent_id, subject, predicate, object, description, subject_id, object_id, weight, chunk_ids, metadata, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, '{}'::jsonb, now(), now())
		`, uuid.New(), docID,
			r.Subject, r.Predicate, r.Object,
			nullIfEmpty(r.Description), subjectID, objectID, r.Weight, chunkIDs,
		); err != nil {
			return 0, 0, fmt.Errorf("insert relationship %s-[%s]->%s: %w",
				r.Subject, r.Predicate, r.Object, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE deepresearch.documents SET extraction_status = 'success', updated_at = now() WHERE id = $1`,
		docID,
	); err != nil {
		return 0, 0, fmt.Errorf("update extraction_status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit extract tx: %w", err)
	}
	return len(res.Entities), len(res.Relationships), nil
}

// MarkExtractionFailed updates extraction_status to 'failed' so the
// dashboard knows the doc has been attempted, not just pending.
// Failure reason is best-effort logged to metadata.
func MarkExtractionFailed(ctx context.Context, pool *pgxpool.Pool, docID uuid.UUID, reason string) error {
	_, err := pool.Exec(ctx, `
		UPDATE deepresearch.documents
		SET extraction_status = 'failed',
		    metadata = jsonb_set(coalesce(metadata, '{}'::jsonb), '{extraction_error}', to_jsonb($2::text)),
		    updated_at = now()
		WHERE id = $1
	`, docID, reason)
	return err
}

// LoadChunksForExtraction fetches the texts (and ids) of all chunks
// belonging to a document, ordered by chunk_order so the extractor
// sees the doc in reading order. Returns ids parallel to texts.
func LoadChunksForExtraction(ctx context.Context, pool *pgxpool.Pool, docID uuid.UUID) ([]uuid.UUID, []string, error) {
	const q = `
		SELECT id, text
		FROM deepresearch.chunks
		WHERE document_id = $1
		ORDER BY COALESCE((metadata->>'chunk_order')::int, 0)
	`
	rows, err := pool.Query(ctx, q, docID)
	if err != nil {
		return nil, nil, fmt.Errorf("load chunks: %w", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	var texts []string
	for rows.Next() {
		var id uuid.UUID
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, nil, fmt.Errorf("scan chunk: %w", err)
		}
		ids = append(ids, id)
		texts = append(texts, text)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate chunks: %w", err)
	}
	return ids, texts, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func normalizeName(s string) string {
	// match the extract package's lower-case dedup key
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		out = append(out, c)
	}
	return string(out)
}

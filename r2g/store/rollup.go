package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RollupLeafRow is a level-0 node fed to the rollup clusterer: its id,
// the text to summarize, and the embedding to cluster on. The
// documents-backed source reads summary (falling back to title) + the
// summary_embedding vector.
type RollupLeafRow struct {
	ID        uuid.UUID
	Content   string
	Embedding []float32
}

// LoadRollupLeaves returns the level-0 documents of a kind that have an
// embedding to cluster on. Summaries (level >= 1) are excluded so a
// re-run clusters only true leaves, not prior rollup output.
func LoadRollupLeaves(ctx context.Context, pool *pgxpool.Pool, kind string) ([]RollupLeafRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, COALESCE(NULLIF(summary, ''), COALESCE(title, '')), summary_embedding::text
		  FROM deepresearch.documents
		 WHERE kind = $1 AND level = 0 AND summary_embedding IS NOT NULL
	`, kind)
	if err != nil {
		return nil, fmt.Errorf("query rollup leaves: %w", err)
	}
	defer rows.Close()

	var out []RollupLeafRow
	for rows.Next() {
		var r RollupLeafRow
		var vecText *string
		if err := rows.Scan(&r.ID, &r.Content, &vecText); err != nil {
			return nil, fmt.Errorf("scan rollup leaf: %w", err)
		}
		r.Embedding = parsePgvector(vecText)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rollup leaves: %w", err)
	}
	return out, nil
}

// InsertSummaryDoc writes one rollup summary node as a documents row at
// the given level/kind. memberLeafIDs are recorded in metadata for
// traceability; the load-bearing tree structure is the parent_id chain
// (see SetDocumentParent). embedding may be nil (retrieval over summaries
// then falls back to lexical until a re-embed pass runs).
func InsertSummaryDoc(
	ctx context.Context,
	pool *pgxpool.Pool,
	kind string,
	level int,
	content string,
	embedding []float32,
	memberLeafIDs []uuid.UUID,
) (uuid.UUID, error) {
	meta, err := json.Marshal(map[string]any{
		"rollup":          true,
		"source_leaf_ids": memberLeafIDs,
		"member_count":    len(memberLeafIDs),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal summary metadata: %w", err)
	}

	var embed *string
	if len(embedding) > 0 {
		s := vectorParam(embedding)
		embed = &s
	}

	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO deepresearch.documents
		  (id, type, kind, level, summary, summary_embedding, metadata,
		   ingestion_status, extraction_status, created_at, updated_at)
		VALUES ($1, 'rollup_summary', $2, $3, $4, $5::vector, $6::jsonb,
		        'success', 'skipped', now(), now())
	`, id, kind, level, content, embed, string(meta)); err != nil {
		return uuid.Nil, fmt.Errorf("insert summary doc: %w", err)
	}
	return id, nil
}

// SetDocumentParent points each child document at parentID. Used to wire
// leaf documents to their leaf-most summary and summaries to their parent
// summary.
func SetDocumentParent(ctx context.Context, pool *pgxpool.Pool, childIDs []uuid.UUID, parentID uuid.UUID) error {
	if len(childIDs) == 0 {
		return nil
	}
	if _, err := pool.Exec(ctx, `
		UPDATE deepresearch.documents SET parent_id = $1, updated_at = now()
		 WHERE id = ANY($2)
	`, parentID, childIDs); err != nil {
		return fmt.Errorf("set document parent: %w", err)
	}
	return nil
}

// DeleteRollupSummaries removes prior rollup output for a kind so a
// re-run is idempotent: clear summary nodes (level >= 1) and detach their
// children. Leaf documents (level 0) are preserved.
func DeleteRollupSummaries(ctx context.Context, pool *pgxpool.Pool, kind string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Detach everything parented to a summary of this kind, then drop the
	// summaries. parent_id FK is ON DELETE SET NULL, but doing it
	// explicitly keeps the intent clear and the updated_at fresh.
	if _, err := tx.Exec(ctx, `
		UPDATE deepresearch.documents SET parent_id = NULL, updated_at = now()
		 WHERE parent_id IN (
		   SELECT id FROM deepresearch.documents WHERE kind = $1 AND level >= 1
		 )
	`, kind); err != nil {
		return fmt.Errorf("detach children: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM deepresearch.documents WHERE kind = $1 AND level >= 1`, kind,
	); err != nil {
		return fmt.Errorf("delete summaries: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete summaries: %w", err)
	}
	return nil
}

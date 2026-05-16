package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MarkStageIndexed bumps platform.document_stages.stage1_at for a
// document, mirroring the row the Python platform's stage orchestrator
// used to write. Idempotent. Best-effort: callers run it after the
// main ingest transaction commits, so a stage-write failure logs but
// does not roll back the ingest.
//
// Called from the ingest handler so the dashboard's "indexed" counter
// tracks reality without needing a separate Python writer.
func MarkStageIndexed(ctx context.Context, pool *pgxpool.Pool, docID uuid.UUID) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO platform.document_stages
			(document_id, stage, stage1_at, updated_at)
		VALUES ($1, 1, now(), now())
		ON CONFLICT (document_id) DO UPDATE
			SET stage = GREATEST(platform.document_stages.stage, 1),
			    stage1_at = COALESCE(platform.document_stages.stage1_at, now()),
			    updated_at = now()
	`, docID)
	if err != nil {
		return fmt.Errorf("mark stage indexed: %w", err)
	}
	return nil
}

// MarkStageGraphed marks stage2_at when extract produced entities, or
// clears it when extract came back empty AND the deepresearch.documents_entities
// table also has no rows for this doc.
//
// Why the second check: an in-flight extract that returns zero entities
// is NOT a reliable signal that the doc has no graph. We saw this on
// 2026-05-15 when vLLM started returning valid-but-empty `{"entities":[]}`
// responses while degraded, and the unconditional clear was decrementing
// the dashboard counter on docs whose entities table was still populated
// (PersistExtraction is now itself a no-op on empty results, but this
// guard is defense-in-depth in case some future caller bypasses it).
//
// Authoritative source: deepresearch.documents_entities.parent_id. If
// that table has rows for the doc, the doc IS graphed regardless of
// what the latest extract attempt produced.
func MarkStageGraphed(ctx context.Context, pool *pgxpool.Pool, docID uuid.UUID, hasEntities bool) error {
	if hasEntities {
		_, err := pool.Exec(ctx, `
			INSERT INTO platform.document_stages
				(document_id, stage, stage1_at, stage2_at, updated_at)
			VALUES ($1, 2, now(), now(), now())
			ON CONFLICT (document_id) DO UPDATE
				SET stage = GREATEST(platform.document_stages.stage, 2),
				    stage1_at = COALESCE(platform.document_stages.stage1_at, now()),
				    stage2_at = COALESCE(platform.document_stages.stage2_at, now()),
				    updated_at = now()
		`, docID)
		if err != nil {
			return fmt.Errorf("mark stage graphed: %w", err)
		}
		return nil
	}
	// hasEntities=false: only clear stage2_at if the entities table
	// genuinely has no rows for this doc. Empty LLM responses on a
	// previously-graphed doc must NOT decrement the counter.
	var entityCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM deepresearch.documents_entities
		WHERE parent_id = $1
	`, docID).Scan(&entityCount); err != nil {
		return fmt.Errorf("count entities for stage guard: %w", err)
	}
	if entityCount > 0 {
		// Entities exist; the counter is correct, don't touch it.
		return nil
	}
	_, err := pool.Exec(ctx, `
		UPDATE platform.document_stages
		SET stage2_at = NULL,
		    stage = LEAST(stage, 1),
		    updated_at = now()
		WHERE document_id = $1 AND stage2_at IS NOT NULL
	`, docID)
	if err != nil {
		return fmt.Errorf("clear stage graphed: %w", err)
	}
	return nil
}

// ClearStagesForDocument removes the document_stages row when a doc is
// deleted. Without this the next reindex (which writes a new UUID for
// the same arxiv id) would leave an orphaned stage row behind, which
// is what made the dashboard "indexed" counter drift downward over
// time before this fix.
func ClearStagesForDocument(ctx context.Context, pool *pgxpool.Pool, docID uuid.UUID) error {
	_, err := pool.Exec(ctx, `
		DELETE FROM platform.document_stages WHERE document_id = $1
	`, docID)
	if err != nil {
		return fmt.Errorf("clear stages: %w", err)
	}
	return nil
}

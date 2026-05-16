// Package activities — skipped.go: tracks per-arxiv-id reindex skip
// decisions so the corpus workflow's QueryWorkSet can exclude papers
// we've already determined to have no usable LaTeX source.
//
// Without this table the corpus workflow's ContinueAsNew loop would
// re-fetch the same thousands of pre-2010 / withdrawn / pdf-only papers
// on every continuation, costing real HTTP round-trips to the platform
// ingest endpoint for no progress.
package activities

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// SkippedActivities owns the postgres pool used by MarkArxivSkipped.
// Separate struct from QueueActivities so the activity registry stays
// shallow — each activity-struct is one logical concern.
type SkippedActivities struct {
	Pool   *pgxpool.Pool
	Logger *logrus.Logger
}

// MarkSkippedInput is the activity payload. ArxivID is the paper id
// (new-style "YYMM.NNNNN" or old-style "cat/NNNNNNN"); Reason is a
// short tag — typically one of "invalid_arxiv_id", "ingest_not_ok",
// "not_latex" — recorded for observability.
type MarkSkippedInput struct {
	ArxivID string `json:"arxiv_id"`
	Reason  string `json:"reason"`
}

// MarkArxivSkipped upserts the skip record. The ON CONFLICT branch
// refreshes last_skipped_at and bumps skip_count so we can spot
// papers that keep getting re-skipped — which would mean either the
// 30-day TTL in QueryWorkSet's LEFT JOIN expired and the paper got
// re-attempted, or a manual reset cleared the table.
func (a *SkippedActivities) MarkArxivSkipped(ctx context.Context, in MarkSkippedInput) error {
	if in.ArxivID == "" {
		return fmt.Errorf("MarkArxivSkipped: arxiv_id required")
	}
	const q = `
		INSERT INTO evo.reindex_skipped (arxiv_id, reason)
		VALUES ($1, $2)
		ON CONFLICT (arxiv_id) DO UPDATE
		SET reason = EXCLUDED.reason,
		    last_skipped_at = NOW(),
		    skip_count = evo.reindex_skipped.skip_count + 1
	`
	if _, err := a.Pool.Exec(ctx, q, in.ArxivID, in.Reason); err != nil {
		return fmt.Errorf("mark arxiv skipped: %w", err)
	}
	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"arxiv":  in.ArxivID,
			"reason": in.Reason,
		}).Debug("MarkArxivSkipped: recorded")
	}
	return nil
}

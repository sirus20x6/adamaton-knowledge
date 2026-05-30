// Package activities holds the Temporal activity implementations for
// the arxiv reindex subsystem. Two activity structs live here:
//
//	QueueActivities  — postgres-bound (deepresearch + platform tables)
//	IngestActivities — HTTP-bound (platform /platform/sources/ingest + v3)
//
// They're separated so the postgres pool and the HTTP client can be
// injected independently at worker startup, mirroring the same shape
// the skills worker uses (R2RActivities + PersistActivities).
package activities

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamaton-knowledge/reindex/workflows"
)

// QueueActivities groups the two postgres-bound activities used by the
// arxiv reindex workflows.
//
//   - QueryWorkSet runs once at the start of the parent workflow to
//     produce the per-paper work list (replaces the bash script's psql
//     pipe-to-QUEUE_FILE on lines 42-56).
//   - RewireCorpusDocument runs once per paper after a successful latex
//     re-ingest (replaces the bash UPDATE on line 138).
//
// Dependencies are captured on the receiver so each call reuses the
// same pool instead of redialing.
type QueueActivities struct {
	Pool   *pgxpool.Pool
	Logger *logrus.Logger
}

// RewireInput is the per-paper rewire payload. Named struct (not bare
// args) so adding fields later — e.g. a corpus id, or a copy of the
// figure_count for an audit log — doesn't break activity signatures.
type RewireInput struct {
	OldDocID string `json:"old_doc_id"`
	NewDocID string `json:"new_doc_id"`
}

// QueryWorkSet returns the list of arxiv papers whose deepresearch row
// either still has source_tier != 'latex' or carries an empty summary.
// Mirrors the SQL on lines 42-56 of the bash script verbatim, minus the
// $POISON_FILTER (Temporal handles "in-flight" tracking via the child
// workflow execution history — the bash script's inflight file is no
// longer needed).
//
// When limit > 0 we append a LIMIT clause so dry runs and pilots can
// cap the queue without touching the SQL filter.
func (a *QueueActivities) QueryWorkSet(ctx context.Context, limit int) ([]workflows.ArxivPaper, error) {
	log := a.logger()

	// DISTINCT ON (arxiv_id) so a paper that has multiple orphan docs
	// in deepresearch.documents (e.g. a previous reindex attempt where
	// RewireCorpusDocument succeeded but DeleteR2RDocument(old) didn't)
	// only appears in the work-set once. Without dedup, the corpus
	// workflow tries to start two children with the same workflow id
	// ("reindex-paper-{arxiv_id}") and the second hits
	// "child workflow execution already started". We pick the most
	// recent row (ORDER BY arxiv_id, created_at DESC) on the assumption
	// it's the one most likely to be the live corpus_documents target.
	// Performance: requires deepresearch.documents_arxiv_id_created_idx — see reindex/sql/001_arxiv_id_index.sql
	//
	// LEFT JOIN against evo.reindex_skipped excludes papers we've
	// recently determined have no usable LaTeX source. Without this
	// filter the corpus workflow's ContinueAsNew loop would re-fetch
	// the same thousands of skipped papers on every continuation. The
	// 30-day cutoff is deliberately short — long enough that a single
	// full-corpus pass doesn't re-attempt yesterday's skips, short
	// enough that a freshly LaTeX-ified paper (rare but real: arxiv
	// occasionally re-uploads source for old papers) gets another
	// chance within a month without manual table-truncation.
	q := `
		SELECT DISTINCT ON (d.metadata->>'arxiv_id')
		       d.id::text AS old_doc_id,
		       d.metadata->>'arxiv_id' AS arxiv_id,
		       COALESCE(d.title, d.metadata->>'arxiv_id', '?') AS title
		FROM deepresearch.documents d
		LEFT JOIN evo.reindex_skipped s
		  ON s.arxiv_id = d.metadata->>'arxiv_id'
		 AND s.last_skipped_at > now() - interval '30 days'
		WHERE d.metadata ? 'arxiv_id'
		  AND d.metadata->>'arxiv_id' IS NOT NULL
		  AND d.metadata->>'arxiv_id' <> ''
		  AND s.arxiv_id IS NULL  -- not recently skipped
		  AND (
		    COALESCE(d.metadata->>'source_tier', '') <> 'latex'
		    OR d.summary IS NULL OR d.summary = ''
		  )
		ORDER BY d.metadata->>'arxiv_id', d.created_at DESC
	`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := a.Pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query arxiv work set: %w", err)
	}
	defer rows.Close()

	var out []workflows.ArxivPaper
	for rows.Next() {
		var p workflows.ArxivPaper
		if err := rows.Scan(&p.OldDocID, &p.ArxivID, &p.Title); err != nil {
			return nil, fmt.Errorf("scan arxiv row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate arxiv rows: %w", err)
	}

	log.WithField("count", len(out)).Info("QueryWorkSet: produced arxiv work set")
	return out, nil
}

// RewireCorpusDocument swaps the platform-side document pointer from
// the old (non-latex) R2R doc to the freshly-ingested latex doc.
// Mirrors line 138 of the bash script. The DELETE of stale
// document_figures rows from the bash version (line 139) is NOT folded
// into this UPDATE — it lives in its own DeleteStaleFigures activity
// (below) so the figure cleanup has a visible, independently-retried
// failure mode instead of being swallowed (the bash version masked it
// with 2>&1 >/dev/null). The paper workflow runs DeleteStaleFigures
// right after this rewire succeeds; see reindex/workflows/paper.go.
func (a *QueueActivities) RewireCorpusDocument(ctx context.Context, in RewireInput) error {
	log := a.logger()
	if in.OldDocID == "" || in.NewDocID == "" {
		return fmt.Errorf("rewire: old_doc_id and new_doc_id required (got %q / %q)", in.OldDocID, in.NewDocID)
	}
	const q = `UPDATE platform.corpus_documents SET document_id = $1 WHERE document_id = $2`
	tag, err := a.Pool.Exec(ctx, q, in.NewDocID, in.OldDocID)
	if err != nil {
		return fmt.Errorf("rewire corpus_documents: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Non-retryable: the corpus row this paper was supposed to swap doesn't exist.
		// Most common cause: a concurrent reindex moved the corpus pointer first.
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("rewire matched zero rows (old=%s new=%s)", in.OldDocID, in.NewDocID),
			"RewireNoOp", nil)
	}
	log.WithFields(logrus.Fields{
		"old": in.OldDocID,
		"new": in.NewDocID,
	}).Debug("RewireCorpusDocument: swapped pointer")
	return nil
}

// DeleteStaleFigures removes the orphaned platform.document_figures rows
// that belonged to the OLD (pre-reindex) document. It ports line 139 of
// the bash script (DELETE FROM platform.document_figures WHERE
// document_id='$old_id') as a standalone activity.
//
// Why a dedicated activity rather than relying on the step-6 R2R delete?
// The corpus rewire flow's final step issues DeleteR2RDocument(old),
// which hits r2g's DELETE /v3/documents/{id} and DOES cascade the
// figure rows away (r2g/store/search.go DeleteDocument deletes from
// platform.document_figures inside its transaction). But that delete is
// best-effort: paper.go logs a Warn and still returns the paper as Done
// if every retry fails. When that happens the old doc *and* its figure
// rows are orphaned with nothing to ever reclaim them — exactly the
// "deferred stale document_figures cleanup" the queue.go TODO flagged.
//
// Running this explicit DELETE right after the rewire succeeds (and
// before the R2R old-doc delete) guarantees the figure rows are gone
// even if the downstream cascade never runs. It is idempotent: a second
// run, or a run after the cascade already cleared the rows, simply
// affects zero rows. document_figures has no FK to documents (see the
// "manual" note in r2g/store/search.go), so deleting figures first
// never violates a constraint regardless of delete ordering.
//
// Performance: a document_figures(document_id) index keeps this a single
// point lookup — see reindex/sql/002_document_figures_doc_id_index.sql.
func (a *QueueActivities) DeleteStaleFigures(ctx context.Context, oldDocID string) error {
	log := a.logger()
	if oldDocID == "" {
		return temporal.NewNonRetryableApplicationError(
			"delete-stale-figures: old_doc_id required", "BadInput", nil)
	}
	const q = `DELETE FROM platform.document_figures WHERE document_id = $1`
	tag, err := a.Pool.Exec(ctx, q, oldDocID)
	if err != nil {
		return fmt.Errorf("delete stale document_figures (old=%s): %w", oldDocID, err)
	}
	log.WithFields(logrus.Fields{
		"old":     oldDocID,
		"deleted": tag.RowsAffected(),
	}).Debug("DeleteStaleFigures: cleared old document_figures")
	return nil
}

// QueryReadyInput names the args for the QueryReadyForExtraction
// activity so adding fields later (additional filters, ordering hints)
// doesn't break the activity signature. Mirrors workflows.queryReadyInput.
type QueryReadyInput struct {
	CorpusID string `json:"corpus_id"`
	Limit    int    `json:"limit"`
}

// QueryReadyForExtraction returns doc IDs that are ready for graph
// extraction: ingest is fully done (chunks + embeddings written), but
// extraction hasn't landed yet. Filter is type='eprint' so we don't
// waste extraction work on legacy md docs that are about to be replaced
// by the reindex pipeline.
//
// The order is FIFO by created_at so older docs drain first — this
// matters for the existing backlog (601+ docs already at
// extraction_status='pending') since those are the most-stale and
// most likely to have stable content vs the still-incoming reindex
// output.
func (a *QueueActivities) QueryReadyForExtraction(ctx context.Context, in QueryReadyInput) ([]string, error) {
	log := a.logger()
	if in.CorpusID == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"query-ready: corpus_id required", "BadInput", nil)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}

	// EXISTS-chunks guard added 2026-05-15: some docs end up with
	// ingestion_status='success' but zero rows in deepresearch.chunks
	// (likely a partial-commit / interrupted-ingest mode we still need
	// to root-cause). Without this filter the picker returns the same
	// phantom batch on every iteration, every extract attempt fails 409
	// "no chunks found", and Temporal's non-retryable error wedges the
	// whole queue -- healthier docs later in the ORDER never get pulled.
	const q = `
		SELECT d.id::text
		FROM deepresearch.documents d
		JOIN platform.corpus_documents cd ON cd.document_id = d.id
		WHERE cd.corpus_id = $1
		  AND d.type = 'eprint'
		  AND d.ingestion_status = 'success'
		  AND d.extraction_status IN ('pending', 'failed')
		  AND EXISTS (
		    SELECT 1 FROM deepresearch.chunks c WHERE c.document_id = d.id
		  )
		ORDER BY d.created_at ASC
		LIMIT $2
	`
	rows, err := a.Pool.Query(ctx, q, in.CorpusID, limit)
	if err != nil {
		return nil, fmt.Errorf("query ready-for-extraction: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan ready row: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready rows: %w", err)
	}

	log.WithFields(logrus.Fields{
		"corpus": in.CorpusID,
		"count":  len(out),
		"limit":  limit,
	}).Info("QueryReadyForExtraction: produced batch")
	return out, nil
}

func (a *QueueActivities) logger() *logrus.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return logrus.New()
}

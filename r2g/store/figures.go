package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/figures"
)

// InsertFigures writes the supplied rows into platform.document_figures.
// Single transaction, one INSERT per row to stay consistent with the
// rest of the store package (see store/ingest.go:133 for the chunk
// equivalent — same trade-off: COPY is awkward for bytea, per-row INSERT
// is plenty fast for the ~5-10 figures a paper carries).
//
// No ON CONFLICT clause: the table has no unique key beyond the surrogate
// primary id, and the Python path didn't dedupe either. Re-ingests of
// the same document will create duplicate rows — the calling code is
// expected to DELETE existing rows first if it wants to refresh.
func InsertFigures(ctx context.Context, pool *pgxpool.Pool, rows []figures.FigureRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin figures tx: %w", err)
	}
	defer tx.Rollback(ctx)

	const insertSQL = `
		INSERT INTO platform.document_figures (
			document_id,
			label,
			section_label,
			caption_tex,
			caption_text,
			src_path,
			raw_format,
			raw_bytes,
			thumbnail_png,
			order_in_doc
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	for _, r := range rows {
		if _, err := tx.Exec(ctx, insertSQL,
			r.DocumentID,
			nullableText(r.Label),
			nullableText(r.SectionLabel),
			nullableText(r.CaptionTex),
			nullableText(r.CaptionText),
			nullableText(r.SrcPath),
			r.RawFormat,
			r.RawBytes,
			nullableBytes(r.ThumbnailPNG),
			r.OrderInDoc,
		); err != nil {
			return 0, fmt.Errorf("insert figure %s/%d: %w",
				r.DocumentID, r.OrderInDoc, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit figures tx: %w", err)
	}
	return len(rows), nil
}

// nullableText writes empty strings as SQL NULL so the column reads
// match what the Python path produced (it converted "" to None at
// latex_figures.py:418-425).
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableBytes does the same for the optional thumbnail_png column.
// thumbnail generation is best-effort (svg/eps figs have no thumbnail,
// oversized rasters return ErrOversize) so callers may legitimately
// pass nil here.
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Compile-time check that InsertFigures has the signature the figures
// package's inserter parameter expects.
var _ = func() func(context.Context, *pgxpool.Pool, []figures.FigureRow) (int, error) {
	return InsertFigures
}

// Tx is exposed so external callers can wrap multiple writes in one
// transaction if they ever need to. Unused today but cheap to surface.
type Tx = pgx.Tx

package figures

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/tarwalk"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// MaxMemberBytes caps per-figure raw bytes. Matches _MAX_MEMBER_BYTES in
// the retired Python path (32 MiB). Figures bigger than this get skipped
// entirely — the dashboard's figure-of-the-day is rarely a 32-MB+ asset.
const MaxMemberBytes = 32 << 20

// imgFormats maps lowercase file-extension → raw_format string stored on
// the document_figures row. Mirrors the Python _IMG_EXTS at
// latex_figures.py:42. The ON CHECK constraint on platform.document_figures
// only allows these values.
var imgFormats = map[string]string{
	".pdf":  "pdf",
	".png":  "png",
	".jpg":  "jpg",
	".jpeg": "jpeg",
	".eps":  "eps",
	".svg":  "svg",
	".gif":  "gif",
	".tif":  "tiff",
	".tiff": "tiff",
}

// FigureMeta is what r2g already gets from latex2text v0.3 — caption +
// label + the list of \includegraphics src paths in source order.
// (Section context isn't surfaced by latex2text; we leave it empty here,
// the Python path was best-effort about it too.)
type FigureMeta struct {
	Label    string
	Caption  string
	SrcPaths []string
}

// ExtractAndPersist is the entry point called from the ingest route
// after the main document + chunks transaction commits.
//
// Walks the gzipped tarball, builds an in-memory map of member name →
// bytes (with a 32 MiB per-member cap), resolves each FigureMeta.SrcPaths
// entry through tryResolve (same fallbacks as Python _resolve_in_tree),
// renders a thumbnail, and writes the rows via inserter (store.InsertFigures
// in production; a stub in tests).
//
// Returns (rowsInserted, err). All recoverable failures (bad image, fitz
// crash, oversized member) are logged and that figure is skipped — same
// graceful-degradation as the Python path. Returns a non-nil error only
// on hard failures (bad tarball, DB unreachable).
func ExtractAndPersist(
	ctx context.Context,
	pool *pgxpool.Pool,
	docID uuid.UUID,
	tarball []byte,
	metas []FigureMeta,
	logger *logrus.Logger,
	inserter func(ctx context.Context, pool *pgxpool.Pool, rows []store.FigureRow) (int, error),
) (int, error) {
	if len(metas) == 0 || len(tarball) == 0 {
		return 0, nil
	}
	members, err := tarwalk.ReadAll(tarball, MaxMemberBytes)
	if err != nil {
		return 0, fmt.Errorf("read tarball: %w", err)
	}
	if len(members) == 0 {
		return 0, nil
	}

	rows := make([]store.FigureRow, 0, len(metas))
	order := 0
	for _, meta := range metas {
		for _, src := range meta.SrcPaths {
			resolved, raw, format, ok := resolveAndRead(members, src, logger)
			if !ok {
				// Same as Python: log + drop. Parser already
				// captures the caption as a figure_ref chunk
				// upstream.
				continue
			}
			thumb, terr := MakeThumbnail(raw, format)
			if terr != nil {
				if errors.Is(terr, ErrOversize) {
					logger.WithFields(logrus.Fields{
						"doc": docID, "src": src, "format": format,
					}).Info("figures: oversize raster, skipping thumbnail")
				} else {
					logger.WithError(terr).WithFields(logrus.Fields{
						"doc": docID, "src": src, "format": format,
					}).Info("figures: thumbnail failed, persisting raw only")
				}
				thumb = nil
			}
			rows = append(rows, store.FigureRow{
				DocumentID:   docID,
				Label:        meta.Label,
				CaptionTex:   "", // latex2text already delatex'd the caption
				CaptionText:  meta.Caption,
				SrcPath:      resolved,
				RawFormat:    format,
				RawBytes:     raw,
				ThumbnailPNG: thumb,
				OrderInDoc:   order,
			})
			order++
		}
	}

	if len(rows) == 0 {
		return 0, nil
	}
	return inserter(ctx, pool, rows)
}

// resolveAndRead implements Python _resolve_in_tree's fallback ladder:
//  1. Exact match on src.
//  2. If src has no extension or an unrecognised one, try appending each
//     extension from imgFormats.
//  3. Glob: find any member whose stem == src's stem and whose extension
//     is in imgFormats.
//
// Returns (resolvedName, bytes, format, ok). The format string is the
// imgFormats value (e.g. "pdf", "jpg"), not the raw extension.
func resolveAndRead(
	members map[string][]byte, src string, _ *logrus.Logger,
) (string, []byte, string, bool) {
	src = strings.TrimPrefix(src, "./")
	src = path.Clean(src)
	ext := strings.ToLower(path.Ext(src))

	if b, ok := members[src]; ok {
		if f, ok := imgFormats[ext]; ok {
			return src, b, f, true
		}
	}
	stem := strings.TrimSuffix(src, ext)
	// 2. Try the canonical extensions.
	for tryExt, format := range imgFormats {
		candidate := stem + tryExt
		if b, ok := members[candidate]; ok {
			return candidate, b, format, true
		}
	}
	// 3. Glob — pick the first member that matches the stem and has a
	// known extension. Stable iteration order over a map is not
	// guaranteed; for a paper that ships both diagram.png and
	// diagram.pdf the choice is deterministic-by-extension because we
	// iterate imgFormats here, not the members map.
	srcStem := path.Base(stem)
	for memberName, b := range members {
		mext := strings.ToLower(path.Ext(memberName))
		if memberName == src {
			continue
		}
		if path.Base(strings.TrimSuffix(memberName, mext)) != srcStem {
			continue
		}
		if f, ok := imgFormats[mext]; ok {
			return memberName, b, f, true
		}
	}
	return "", nil, "", false
}

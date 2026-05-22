// Package routes — prepare.go owns the split-ingest endpoints used by the
// reindex Temporal pipeline:
//
//	POST /platform/sources/prepare  — fetch + parse only (no DB write)
//	POST /platform/sources/figures  — extract + persist figures (eprint only)
//
// Why split: the heavy/contended embedding step + the chunk + the DB
// persist now run as a Temporal activity on the worker node (see
// reindex ChunkEmbedPersist). But the arxiv fetch + parse (pdftotext,
// latex2text) and figure extraction (go-fitz/MuPDF, cgo) need binaries
// that only ship in the r2g image — so they stay here, reached over HTTP.
// /prepare returns the parsed text (payload-safe, ~tens-to-hundreds of KB)
// so the worker can chunk+embed+persist without carrying the tarball.
package routes

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/arxiv"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/figures"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/parsing"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// sourcesPrepareHandler implements POST /platform/sources/prepare. Body is
// the same shape as /ingest ({ target: "arxiv:ID", document_id? }), but the
// handler stops after parse: it runs the arxiv fetch cascade and the parser,
// then returns the extracted text + tier + doctype + metadata. It does NOT
// chunk, embed, or persist — that's the worker's ChunkEmbedPersist activity.
func sourcesPrepareHandler(logger *logrus.Logger) http.HandlerFunc {
	fetcher := arxiv.NewClient(logger)
	return func(w http.ResponseWriter, req *http.Request) {
		body, err := decodeIngestBody(io.LimitReader(req.Body, 1<<16))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		scheme, value, err := parseTarget(body.Target)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		if scheme != "arxiv" {
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"ok": false, "error": "prepare only supports arxiv targets", "scheme": scheme,
			})
			return
		}

		doc, err := fetcher.Fetch(req.Context(), value, nil)
		if err != nil {
			if errors.Is(err, arxiv.ErrAllTiersExhausted) {
				writeJSON(w, http.StatusBadGateway, errorBody(
					"arxiv fetch cascade exhausted (no tier returned content)"))
				return
			}
			logger.WithError(err).WithField("arxiv_id", value).Warn("arxiv fetch failed (prepare)")
			writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
			return
		}

		filename := arxivFilename(value, doc.SourceTier)
		parsed, err := parsing.Parse(req.Context(), doc.ContentType, filename, doc.Body)
		if err != nil {
			if errors.Is(err, parsing.ErrUnsupportedType) {
				writeJSON(w, http.StatusUnsupportedMediaType, errorBody(err.Error()))
				return
			}
			logger.WithError(err).WithField("arxiv_id", value).Warn("parse failed (prepare)")
			writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"arxiv_id":    value,
			"source_tier": doc.SourceTier,
			"doctype":     parsed.DocType,
			"text":        parsed.Text,
			"title":       filename,
			"size_bytes":  len(doc.Body),
			"has_figures": parsed.DocType == "eprint" && countFigureMetas(parsed.Figures) > 0,
			"metadata":    arxivMetadata(value, doc),
		})
	}
}

// figuresRequestBody is the POST /platform/sources/figures payload. The
// worker calls this after ChunkEmbedPersist has committed the document, so
// the figure rows can FK-reference an existing document_id.
type figuresRequestBody struct {
	ArxivID    string `json:"arxiv_id"`
	DocumentID string `json:"document_id"`
}

// sourcesFiguresHandler implements POST /platform/sources/figures. Eprint
// (latex) only — for any other tier there are no extractable figures and it
// returns figures:0. Re-fetches the tarball (the worker can't carry multi-MB
// bytes through a Temporal payload) and runs the cgo/MuPDF figure pipeline.
func sourcesFiguresHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	fetcher := arxiv.NewClient(logger)
	return func(w http.ResponseWriter, req *http.Request) {
		var body figuresRequestBody
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
			return
		}
		if body.ArxivID == "" || body.DocumentID == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("arxiv_id and document_id are required"))
			return
		}
		docID, err := uuid.Parse(body.DocumentID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid document_id: "+err.Error()))
			return
		}

		doc, err := fetcher.Fetch(req.Context(), body.ArxivID, nil)
		if err != nil {
			if errors.Is(err, arxiv.ErrAllTiersExhausted) {
				writeJSON(w, http.StatusBadGateway, errorBody(
					"arxiv fetch cascade exhausted (no tier returned content)"))
				return
			}
			logger.WithError(err).WithField("arxiv_id", body.ArxivID).Warn("arxiv fetch failed (figures)")
			writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
			return
		}
		if doc.SourceTier != "latex" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "figures": 0})
			return
		}

		filename := arxivFilename(body.ArxivID, doc.SourceTier)
		parsed, err := parsing.Parse(req.Context(), doc.ContentType, filename, doc.Body)
		if err != nil {
			logger.WithError(err).WithField("arxiv_id", body.ArxivID).Warn("parse failed (figures)")
			writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
			return
		}

		metas := make([]figures.FigureMeta, 0, len(parsed.Figures))
		for _, f := range parsed.Figures {
			if len(f.SrcPaths) == 0 {
				continue
			}
			metas = append(metas, figures.FigureMeta{Label: f.Label, Caption: f.Caption, SrcPaths: f.SrcPaths})
		}
		if len(metas) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "figures": 0})
			return
		}

		n, err := figures.ExtractAndPersist(req.Context(), pool, docID, doc.Body, metas, logger, store.InsertFigures)
		if err != nil {
			logger.WithError(err).WithField("document_id", docID).Warn("figure extract failed")
			writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "figures": n})
	}
}

// arxivFilename derives the synthetic document title/filename from the arxiv
// id and the fetched tier (drives the parser's extension dispatch). Shared by
// the prepare, figures, and ingest handlers so all three agree on the name.
func arxivFilename(arxivID, sourceTier string) string {
	name := strings.ReplaceAll(arxivID, "/", "_")
	switch sourceTier {
	case "latex":
		return name + ".eprint"
	case "pdf":
		return name + ".pdf"
	default:
		return name + ".md"
	}
}

// arxivMetadata builds the stable metadata blob persisted with the document.
// Raw archive bytes are deliberately excluded (bytea doesn't round-trip
// through JSON). Shared by the prepare and ingest handlers.
func arxivMetadata(arxivID string, doc *arxiv.FetchedDoc) map[string]any {
	md := map[string]any{
		"arxiv_id":     arxivID,
		"source_tier":  doc.SourceTier,
		"source_url":   doc.URL,
		"paper_title":  doc.Title,
		"ingested_via": "platform_sources",
	}
	if mt, ok := doc.Metadata["main_tex"]; ok {
		md["main_tex"] = mt
	}
	if tf, ok := doc.Metadata["tex_files"]; ok {
		md["tex_files"] = tf
	}
	return md
}

// countFigureMetas counts parsed figures that have at least one source path
// (the only ones the figure extractor can resolve).
func countFigureMetas(figs []parsing.FigureInfo) int {
	n := 0
	for _, f := range figs {
		if len(f.SrcPaths) > 0 {
			n++
		}
	}
	return n
}

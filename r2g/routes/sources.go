// Package routes — sources.go owns /platform/sources/*.
//
// Lifted from platform/backend/app/api/sources.py on 2026-05-15 as part
// of the "minimize Python" arc. The Python file's last responsibility was
// shuffling an arxiv-fetcher cascade plus an internal HTTP POST to
// r2g:7272/v3/documents -- both now live in this process.
package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/arxiv"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/chunking"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/embeddings"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/ingestpipeline"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/upload"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/store"
)

// RegisterSources wires the /platform/sources/* endpoints. Route order
// matters: /kinds and /upload come before /{id} so the numeric-id route
// doesn't shadow the static paths. gorilla/mux respects registration
// order on overlapping path patterns.
func RegisterSources(
	r *mux.Router,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	logger *logrus.Logger,
) {
	r.HandleFunc("/platform/sources/kinds", sourcesKindsHandler()).Methods(http.MethodGet)
	r.HandleFunc("/platform/sources/upload", sourcesUploadHandler(logger)).Methods(http.MethodPost)
	r.HandleFunc("/platform/sources/ingest",
		sourcesIngestHandler(pool, embedder, chunker, logger)).Methods(http.MethodPost)
	// Numeric-id route -- the regex constraint forbids "kinds" etc. from
	// matching even if registration order were different.
	r.HandleFunc("/platform/sources/{id:[0-9]+}", sourcesGetHandler(pool, logger)).Methods(http.MethodGet)
	// List route -- both with and without trailing slash. FastAPI mounted
	// these as separate endpoints, so the Caddy reroute will see both
	// shapes from existing callers.
	r.HandleFunc("/platform/sources/", sourcesListHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/sources", sourcesListHandler(pool, logger)).Methods(http.MethodGet)
}

// sourcesKindsHandler returns the static list of allowed source kinds.
// The DB CHECK constraint is the source of truth; we keep it duplicated
// in store.AllowedSourceKinds and expose it here so the frontend's
// filter chip dropdown doesn't have to scrape information_schema.
func sourcesKindsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"kinds": store.AllowedSourceKinds,
		})
	}
}

// sourcesListHandler implements GET /platform/sources/ with filters that
// compose with AND: kind, peer_reviewed, q (substring over name OR
// domain), limit (1-500), offset.
func sourcesListHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		filter := store.SourceFilter{}
		if v := q.Get("kind"); v != "" {
			if !slicesContains(store.AllowedSourceKinds, v) {
				writeJSON(w, http.StatusBadRequest,
					errorBody(fmt.Sprintf("unknown kind: %q; expected one of %v",
						v, store.AllowedSourceKinds)))
				return
			}
			filter.Kind = &v
		}
		if v := q.Get("peer_reviewed"); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest,
					errorBody("peer_reviewed must be a bool: "+err.Error()))
				return
			}
			filter.PeerReviewed = &b
		}
		if v := q.Get("q"); v != "" {
			filter.QSubstring = &v
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				writeJSON(w, http.StatusBadRequest,
					errorBody("limit must be an integer 1..500"))
				return
			}
			filter.Limit = n
		} else {
			filter.Limit = 50
		}
		if v := q.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest,
					errorBody("offset must be a non-negative integer"))
				return
			}
			filter.Offset = n
		}

		rows, total, err := store.ListSources(req.Context(), pool, filter)
		if err != nil {
			logger.WithError(err).Error("list sources failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		items := make([]map[string]any, 0, len(rows))
		for _, s := range rows {
			items = append(items, sourcePayload(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":  items,
			"count":  len(items),
			"total":  total,
			"limit":  filter.Limit,
			"offset": filter.Offset,
		})
	}
}

// sourcesGetHandler implements GET /platform/sources/{id}. 404 if missing.
func sourcesGetHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, err := strconv.ParseInt(mux.Vars(req)["id"], 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid id"))
			return
		}
		s, err := store.GetSource(req.Context(), pool, id)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchSource) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("no source with id %d", id)))
				return
			}
			logger.WithError(err).Error("get source failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, sourcePayload(*s))
	}
}

// sourcesUploadHandler implements POST /platform/sources/upload --
// stash multipart bytes to /var/lib/dr-uploads and return the path the
// caller should pass to POST /ingest with target=file:<path>.
func sourcesUploadHandler(logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// 1 MiB header budget; the rest streams via io.Reader.
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest,
				errorBody("parse multipart: "+err.Error()))
			return
		}
		file, hdr, err := req.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest,
				errorBody("file field missing: "+err.Error()))
			return
		}
		defer file.Close()

		res, err := upload.Save(req.Context(), upload.Config{}, file, hdr.Filename)
		if err != nil {
			switch {
			case errors.Is(err, upload.ErrUnsupportedExt):
				writeJSON(w, http.StatusUnsupportedMediaType, errorBody(err.Error()))
			case errors.Is(err, upload.ErrTooLarge):
				writeJSON(w, http.StatusRequestEntityTooLarge, errorBody(err.Error()))
			case errors.Is(err, upload.ErrEmpty):
				writeJSON(w, http.StatusBadRequest, errorBody("empty file"))
			default:
				logger.WithError(err).Error("upload save failed")
				writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			}
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"path":         res.Path,
			"size":         res.Size,
			"filename":     res.Filename,
			"content_type": hdr.Header.Get("Content-Type"),
		})
	}
}

// sourcesIngestHandler implements POST /platform/sources/ingest. Body:
// { target: "arxiv:1706.03762", corpus_id?: uuid, document_id?: uuid }.
// Other target schemes (doi/url/file) currently 501 -- matches the
// Python behaviour. The arxiv path composes arxiv.Fetch with
// ingestpipeline.Run -- no internal HTTP self-call; the bytes flow
// straight through the pipeline.
func sourcesIngestHandler(
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	logger *logrus.Logger,
) http.HandlerFunc {
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
		switch scheme {
		case "arxiv":
			ingestArxiv(req, w, pool, embedder, chunker, fetcher, logger,
				value, body.CorpusID, body.DocumentID)
		case "doi", "url", "file":
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"ok":     false,
				"error":  "not yet implemented",
				"scheme": scheme,
			})
		default:
			writeJSON(w, http.StatusBadRequest,
				errorBody(fmt.Sprintf("unknown target scheme: %s", scheme)))
		}
	}
}

// ingestArxiv runs the cascade, builds a pipeline Input, persists. The
// response shape mirrors sources.py:463-476 verbatim (including
// figure_count=0 -- preserved for old callers parsing the field).
func ingestArxiv(
	req *http.Request,
	w http.ResponseWriter,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	fetcher *arxiv.Client,
	logger *logrus.Logger,
	arxivID, corpusID, documentID string,
) {
	doc, err := fetcher.Fetch(req.Context(), arxivID, nil)
	if err != nil {
		if errors.Is(err, arxiv.ErrAllTiersExhausted) {
			writeJSON(w, http.StatusBadGateway, errorBody(
				"arxiv fetch cascade exhausted (no tier returned content)"))
			return
		}
		logger.WithError(err).WithField("arxiv_id", arxivID).Warn("arxiv fetch failed")
		writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
		return
	}

	var docID uuid.UUID
	if documentID != "" {
		docID, _ = uuid.Parse(documentID) // already validated in decodeIngestBody
	} else {
		docID = uuid.New()
	}

	safeID := strings.ReplaceAll(arxivID, "/", "_")
	filename := safeID
	switch doc.SourceTier {
	case "latex":
		filename += ".eprint"
	case "pdf":
		filename += ".pdf"
	default:
		filename += ".md"
	}

	// Build a stable metadata blob -- don't pass through raw_archive
	// bytes (the Python wrapper filtered them out at sources.py:423
	// for the same reason: bytea in JSON doesn't round-trip).
	metadata := map[string]any{
		"arxiv_id":      arxivID,
		"source_tier":   doc.SourceTier,
		"source_url":    doc.URL,
		"paper_title":   doc.Title,
		"ingested_via":  "platform_sources",
	}
	if mt, ok := doc.Metadata["main_tex"]; ok {
		metadata["main_tex"] = mt
	}
	if tf, ok := doc.Metadata["tex_files"]; ok {
		metadata["tex_files"] = tf
	}

	var collectionIDs []uuid.UUID
	if corpusID != "" {
		if cid, err := uuid.Parse(corpusID); err == nil {
			collectionIDs = []uuid.UUID{cid}
		} else {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid corpus_id: "+err.Error()))
			return
		}
	}

	// Inherit the default owner if env-configured (matches the existing
	// /v3/documents handler -- otherwise the row would have NULL owner_id
	// and downstream pydantic validation in R2R's read endpoints would
	// reject it).
	var ownerID *uuid.UUID
	if v := os.Getenv("R2G_DEFAULT_OWNER_ID"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			ownerID = &id
		}
	}

	res, err := ingestpipeline.Run(req.Context(), pool, embedder, chunker, ingestpipeline.Input{
		DocumentID:    docID,
		Title:         filename,
		ContentType:   doc.ContentType,
		Body:          doc.Body,
		Metadata:      metadata,
		CollectionIDs: collectionIDs,
		OwnerID:       ownerID,
	}, logger)
	if err != nil {
		writePipelineError(w, logger, docID, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"arxiv_id":     arxivID,
		"document_id":  docID.String(),
		"source_tier":  doc.SourceTier,
		"filename":     filename,
		"size":         len(doc.Body),
		"chunks":       res.Chunks,
		"total_tokens": res.TotalTokens,
		// Preserved for backwards compat with callers that parse the
		// legacy response (Zotero bulk-ingest, dashboard polling). The
		// real count is in res.Figures but the old endpoint reported 0
		// because figure extract ran asynchronously after the response.
		"figure_count": res.Figures,
		// r2r_status was the HTTP code from the in-network self-POST.
		// Now in-process; report 200 if we got here.
		"r2r_status": http.StatusOK,
	})
}

// parseTarget splits "scheme:value" strictly. Mirrors Python _parse_target.
func parseTarget(target string) (string, string, error) {
	idx := strings.Index(target, ":")
	if idx < 0 {
		return "", "", errors.New("target must be in the form scheme:value")
	}
	scheme := strings.ToLower(strings.TrimSpace(target[:idx]))
	value := strings.TrimSpace(target[idx+1:])
	if scheme == "" || value == "" {
		return "", "", errors.New("target must be in the form scheme:value")
	}
	return scheme, value, nil
}

// sourcePayload coerces a store.Source into the JSON shape the legacy
// /platform/sources endpoint emitted. Field order doesn't matter on the
// wire (JSON), but the names must match: id, name, kind, domain,
// default_weight, peer_reviewed, h5_index, metadata.
func sourcePayload(s store.Source) map[string]any {
	out := map[string]any{
		"id":            s.ID,
		"name":          s.Name,
		"kind":          s.Kind,
		"domain":        s.Domain,
		"peer_reviewed": s.PeerReviewed,
		"metadata":      s.Metadata,
	}
	if s.DefaultWeight != nil {
		out["default_weight"] = *s.DefaultWeight
	} else {
		out["default_weight"] = nil
	}
	if s.H5Index != nil {
		out["h5_index"] = *s.H5Index
	} else {
		out["h5_index"] = nil
	}
	return out
}

// slicesContains avoids pulling slices.Contains in from generics-land
// for a one-call use. Inline string equality, no reflection.
func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// parseIngestBody pulls the JSON body of POST /platform/sources/ingest
// into its target/corpus_id/document_id fields. Used by the ingest
// handler in routes/ingest_sources.go. Defined here so both files agree
// on the shape.
type ingestRequestBody struct {
	Target     string `json:"target"`
	CorpusID   string `json:"corpus_id"`
	DocumentID string `json:"document_id"`
}

func decodeIngestBody(r io.Reader) (*ingestRequestBody, error) {
	var body ingestRequestBody
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	if body.Target == "" {
		return nil, errors.New("target is required")
	}
	if body.DocumentID != "" {
		if _, err := uuid.Parse(body.DocumentID); err != nil {
			return nil, fmt.Errorf("invalid document_id: %w", err)
		}
	}
	return &body, nil
}

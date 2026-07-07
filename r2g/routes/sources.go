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
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/blobstore"
	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/arxiv"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/ingestpipeline"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/upload"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/webfetch"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
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
	blobs blobstore.Backend,
	logger *logrus.Logger,
) {
	r.HandleFunc("/platform/sources/kinds", sourcesKindsHandler()).Methods(http.MethodGet)
	r.HandleFunc("/platform/sources/upload", sourcesUploadHandler(blobs, logger)).Methods(http.MethodPost)
	r.HandleFunc("/platform/sources/ingest",
		sourcesIngestHandler(pool, embedder, chunker, blobs, logger)).Methods(http.MethodPost)
	// Split-ingest endpoints for the reindex Temporal pipeline (see
	// prepare.go): /prepare fetches+parses without persisting, /figures
	// runs the cgo figure extractor after the worker has persisted the doc.
	r.HandleFunc("/platform/sources/prepare",
		sourcesPrepareHandler(logger)).Methods(http.MethodPost)
	r.HandleFunc("/platform/sources/figures",
		sourcesFiguresHandler(pool, logger)).Methods(http.MethodPost)
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
// stash multipart bytes in the dr-uploads blob store (object key
// "uploads/<uuid>[ext]") and return that key for the caller to pass to
// POST /ingest with target=file:<key> (or s3://dr-uploads/<key>).
//
// The response carries the object key under "key"; "path" is set to the
// same value as a compat alias for the SPA, whose IngestPanel reads
// uploadResp.path and builds target=file://<path> (see
// frontend/src/api/library.ts UploadResponse + IngestPanel.tsx).
func sourcesUploadHandler(blobs blobstore.Backend, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if blobs == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				errorBody("blob store not configured (set BLOBSTORE_* env)"))
			return
		}
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

		res, err := upload.Save(req.Context(), upload.Config{}, blobs, file, hdr.Filename)
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
			"key":          res.Key,
			"path":         res.Key, // compat alias for the SPA (target=file://<path>)
			"size":         res.Size,
			"filename":     res.Filename,
			"content_type": hdr.Header.Get("Content-Type"),
		})
	}
}

// sourcesIngestHandler implements POST /platform/sources/ingest. Body:
// { target: "arxiv:1706.03762", corpus_id?: uuid, document_id?: uuid }.
// The arxiv path composes arxiv.Fetch with ingestpipeline.Run -- no
// internal HTTP self-call; the bytes flow straight through the pipeline.
// The file/s3 schemes Get a previously-uploaded object out of the
// dr-uploads blob store and run it through the same pipeline. The doi
// scheme resolves through doi.org content negotiation (PDF preferred,
// falling back to citation_pdf_url and then readability extraction of
// the landing page); the url scheme fetches directly with the same
// HTML-to-markdown extraction. Both feed the shared pipeline.
func sourcesIngestHandler(
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	blobs blobstore.Backend,
	logger *logrus.Logger,
) http.HandlerFunc {
	fetcher := arxiv.NewClient(logger)
	web := webfetch.NewClient(logger)
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
		case "file", "s3":
			ingestBlob(req, w, pool, embedder, chunker, blobs, logger,
				body.Target, body.CorpusID, body.DocumentID)
		case "doi", "url":
			ingestWeb(req, w, pool, embedder, chunker, web, logger,
				scheme, value, body.CorpusID, body.DocumentID)
		default:
			writeJSON(w, http.StatusBadRequest,
				errorBody(fmt.Sprintf("unknown target scheme: %s", scheme)))
		}
	}
}

// blobKeyFromTarget extracts the dr-uploads object key from an ingest
// target. It accepts every shape the upload path can hand back:
//
//	s3://dr-uploads/uploads/<uuid>.pdf   (canonical URI form)
//	file://uploads/<uuid>.pdf            (what the SPA builds: file://<path>)
//	file:uploads/<uuid>.pdf              (bare scheme:value)
//	uploads/<uuid>.pdf                   (bare key)
//
// The leading "s3"/"file" scheme, any "//" authority slashes, and a
// leading "dr-uploads/" bucket segment are all stripped so the result is
// the plain object key. Returns an error for an empty or absolute-path key
// (a "/"-prefixed value is a legacy filesystem path, not a blob key).
func blobKeyFromTarget(target string) (string, error) {
	key := strings.TrimSpace(target)
	if i := strings.Index(key, ":"); i >= 0 {
		// Strip a leading scheme ("s3:", "file:") if present.
		switch strings.ToLower(strings.TrimSpace(key[:i])) {
		case "s3", "file":
			key = key[i+1:]
		}
	}
	// Strip a "//authority" prefix: for s3:// the authority is the bucket
	// (dr-uploads); for file:// the SPA emits file://<key> with no real
	// authority, so the segment after "//" is already the key.
	key = strings.TrimPrefix(key, "//")
	key = strings.TrimPrefix(key, "dr-uploads/")
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("empty blob key in target")
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("absolute filesystem paths are no longer supported; "+
			"expected an s3://dr-uploads/<key> reference, got %q", target)
	}
	return key, nil
}

// ingestBlob pulls a previously-uploaded object out of the dr-uploads blob
// store and funnels it through the same pipeline the arxiv path uses. The
// target is one of the forms accepted by blobKeyFromTarget; the bytes are
// Get'd from the store and handed to ingestpipeline.Run as Input.Body.
func ingestBlob(
	req *http.Request,
	w http.ResponseWriter,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	blobs blobstore.Backend,
	logger *logrus.Logger,
	target, corpusID, documentID string,
) {
	if blobs == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errorBody("blob store not configured (set BLOBSTORE_* env)"))
		return
	}

	key, err := blobKeyFromTarget(target)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		return
	}

	rc, err := blobs.Get(req.Context(), key)
	if err != nil {
		if errors.Is(err, blobstore.ErrNotFound) {
			writeJSON(w, http.StatusNotFound,
				errorBody(fmt.Sprintf("no uploaded object at key %q", key)))
			return
		}
		logger.WithError(err).WithField("key", key).Error("blob get failed")
		writeJSON(w, http.StatusBadGateway, errorBody("blob store read failed: "+err.Error()))
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		logger.WithError(err).WithField("key", key).Error("blob read failed")
		writeJSON(w, http.StatusBadGateway, errorBody("blob read failed: "+err.Error()))
		return
	}
	if len(data) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody(fmt.Sprintf("object %q is empty", key)))
		return
	}

	var docID uuid.UUID
	if documentID != "" {
		docID, _ = uuid.Parse(documentID) // already validated in decodeIngestBody
	} else {
		docID = uuid.New()
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

	var ownerID *uuid.UUID
	if v := os.Getenv("R2G_DEFAULT_OWNER_ID"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			ownerID = &id
		}
	}

	// Title is the object key's base name (e.g. "<uuid>.pdf"); the parser
	// dispatches on ContentType inferred from the extension.
	title := path.Base(key)
	contentType := contentTypeForKey(key)

	res, err := ingestpipeline.Run(req.Context(), pool, embedder, chunker, ingestpipeline.Input{
		DocumentID:  docID,
		Title:       title,
		ContentType: contentType,
		Body:        data,
		Metadata: map[string]any{
			"source":      "upload",
			"blob_key":    key,
			"blob_bucket": "dr-uploads",
		},
		CollectionIDs: collectionIDs,
		OwnerID:       ownerID,
	}, logger)
	if err != nil {
		writePipelineError(w, logger, docID, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"key":          key,
		"document_id":  docID.String(),
		"filename":     title,
		"size":         len(data),
		"chunks":       res.Chunks,
		"total_tokens": res.TotalTokens,
		"figure_count": res.Figures,
		"r2r_status":   http.StatusOK,
	})
}

// ingestWeb handles the doi: and url: target schemes. It fetches the
// document via internal/webfetch (DOI resolution or direct URL fetch —
// both normalize to pipeline-parseable bytes) and funnels it through
// the same pipeline the arxiv/blob paths use.
//
// Status mapping: invalid DOI/URL or unsupported upstream content type
// -> 400; any other fetch failure (network, upstream 4xx/5xx, size cap)
// -> 502; pipeline errors -> writePipelineError's mapping.
func ingestWeb(
	req *http.Request,
	w http.ResponseWriter,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	web *webfetch.Client,
	logger *logrus.Logger,
	scheme, value, corpusID, documentID string,
) {
	var (
		doc *webfetch.Doc
		err error
	)
	switch scheme {
	case "doi":
		doc, err = web.FetchDOI(req.Context(), value)
	default:
		doc, err = web.FetchURL(req.Context(), value)
	}
	if err != nil {
		switch {
		case errors.Is(err, webfetch.ErrInvalidDOI),
			errors.Is(err, webfetch.ErrInvalidURL),
			errors.Is(err, webfetch.ErrUnsupportedContent):
			writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
		default:
			logger.WithError(err).WithFields(logrus.Fields{
				"scheme": scheme, "value": value,
			}).Warn("web fetch failed")
			writeJSON(w, http.StatusBadGateway, errorBody(err.Error()))
		}
		return
	}

	var docID uuid.UUID
	if documentID != "" {
		docID, _ = uuid.Parse(documentID) // already validated in decodeIngestBody
	} else {
		docID = uuid.New()
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

	var ownerID *uuid.UUID
	if v := os.Getenv("R2G_DEFAULT_OWNER_ID"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			ownerID = &id
		}
	}

	res, err := ingestpipeline.Run(req.Context(), pool, embedder, chunker, ingestpipeline.Input{
		DocumentID:    docID,
		Title:         doc.Title,
		ContentType:   doc.ContentType,
		Body:          doc.Body,
		Metadata:      doc.Metadata,
		CollectionIDs: collectionIDs,
		OwnerID:       ownerID,
	}, logger)
	if err != nil {
		writePipelineError(w, logger, docID, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"target":       scheme + ":" + value,
		"document_id":  docID.String(),
		"title":        doc.Title,
		"source_url":   doc.URL,
		"source_kind":  doc.SourceKind,
		"size":         len(doc.Body),
		"chunks":       res.Chunks,
		"total_tokens": res.TotalTokens,
		"figure_count": res.Figures,
		"r2r_status":   http.StatusOK,
	})
}

// contentTypeForKey maps an object key's extension to the content-type the
// ingest pipeline's parser dispatches on. Unknown extensions fall through
// to "" so parsing.Parse can sniff the bytes.
func contentTypeForKey(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".pdf":
		return "application/pdf"
	case ".epub":
		return "application/epub+zip"
	case ".md", ".markdown":
		return "text/markdown"
	case ".tex":
		return "text/x-tex"
	case ".html", ".htm":
		return "text/html"
	case ".txt":
		return "text/plain"
	default:
		return ""
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

	filename := arxivFilename(arxivID, doc.SourceTier)

	// Stable metadata blob (shared with /prepare). Raw archive bytes are
	// excluded -- bytea doesn't round-trip through JSON.
	metadata := arxivMetadata(arxivID, doc)

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

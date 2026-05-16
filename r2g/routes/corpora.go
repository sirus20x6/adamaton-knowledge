// Package routes — corpora.go owns /platform/corpora/*.
//
// Lifted from platform/backend/app/api/corpora.py on 2026-05-15 as
// part of the "minimize Python" arc. The /stages handler is the hot
// path the Library dashboard polls; the rest is CRUD around it.
package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/store"
)

// RegisterCorpora wires the /platform/corpora/* endpoints. Route order
// matters: the {corpus_id}/documents/{document_id} and /stages routes
// register before bare {corpus_id} so they don't get shadowed.
func RegisterCorpora(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	// List + create. FastAPI mounted both slash variants as distinct
	// endpoints; Caddy will see both shapes from existing callers.
	r.HandleFunc("/platform/corpora/", corporaListHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/corpora", corporaListHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/corpora/", corporaCreateHandler(pool, logger)).Methods(http.MethodPost)
	r.HandleFunc("/platform/corpora", corporaCreateHandler(pool, logger)).Methods(http.MethodPost)

	// Document subroutes — registered before the bare {corpus_id} so
	// the /documents and /stages segments take precedence.
	r.HandleFunc("/platform/corpora/{corpus_id}/documents/{document_id}",
		corporaDetachDocumentHandler(pool, logger)).Methods(http.MethodDelete)
	r.HandleFunc("/platform/corpora/{corpus_id}/documents",
		corporaAttachDocumentsHandler(pool, logger)).Methods(http.MethodPost)
	r.HandleFunc("/platform/corpora/{corpus_id}/documents",
		corporaListDocumentsHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/corpora/{corpus_id}/stages",
		corporaStagesHandler(pool, logger)).Methods(http.MethodGet)

	// Single-corpus routes.
	r.HandleFunc("/platform/corpora/{corpus_id}",
		corporaGetHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/corpora/{corpus_id}",
		corporaPatchHandler(pool, logger)).Methods(http.MethodPatch)
	r.HandleFunc("/platform/corpora/{corpus_id}",
		corporaDeleteHandler(pool, logger)).Methods(http.MethodDelete)
}

// coerceCorpusScope mirrors the Python _coerce_scope helper. Returns
// (db_scope, metadata_overlay, err). "experiments" is virtual: stored
// as "mixed" with metadata.experiment=true.
func coerceCorpusScope(scope string) (string, map[string]any, error) {
	normalized := strings.ToLower(strings.TrimSpace(scope))
	if !slicesContains(store.AllowedAPICorpusScopes, normalized) {
		return "", nil, fmt.Errorf("scope must be one of %v", store.AllowedAPICorpusScopes)
	}
	if normalized == "experiments" {
		return "mixed", map[string]any{"experiment": true}, nil
	}
	return normalized, map[string]any{}, nil
}

func corpusPayload(c store.Corpus) map[string]any {
	return map[string]any{
		"id":             c.ID.String(),
		"name":           c.Name,
		"scope":          c.Scope,
		"created_at":     c.CreatedAt.Format("2006-01-02T15:04:05.999999999"),
		"document_count": c.DocumentCount,
		"metadata":       c.Metadata,
	}
}

func corporaListHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rows, err := store.ListCorpora(req.Context(), pool)
		if err != nil {
			logger.WithError(err).Error("list corpora failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		items := make([]map[string]any, 0, len(rows))
		for _, c := range rows {
			items = append(items, corpusPayload(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"corpora": items})
	}
}

type createCorpusBody struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

func corporaCreateHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body createCorpusBody
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
			return
		}
		name := strings.TrimSpace(body.Name)
		// Match Pydantic's min_length=1 / max_length=200 validators.
		if name == "" || len(name) > 200 {
			writeJSON(w, http.StatusBadRequest,
				errorBody("name must be 1..200 chars"))
			return
		}
		scope := body.Scope
		if scope == "" {
			scope = "research"
		}
		dbScope, overlay, err := coerceCorpusScope(scope)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		c, err := store.CreateCorpus(req.Context(), pool, name, dbScope, overlay)
		if err != nil {
			logger.WithError(err).Error("create corpus failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, corpusPayload(*c))
	}
}

func corporaGetHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, ok := parseCorpusID(w, req)
		if !ok {
			return
		}
		c, err := store.GetCorpus(req.Context(), pool, id)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchCorpus) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("corpus %s not found", id)))
				return
			}
			logger.WithError(err).WithField("corpus_id", id).Error("get corpus failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, corpusPayload(*c))
	}
}

type patchCorpusBody struct {
	Name     *string        `json:"name"`
	Scope    *string        `json:"scope"`
	Metadata map[string]any `json:"metadata"`
}

func corporaPatchHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, ok := parseCorpusID(w, req)
		if !ok {
			return
		}
		var body patchCorpusBody
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
			return
		}

		upd := store.CorpusUpdate{Metadata: map[string]any{}}
		for k, v := range body.Metadata {
			upd.Metadata[k] = v
		}

		if body.Scope != nil {
			dbScope, overlay, err := coerceCorpusScope(*body.Scope)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
				return
			}
			upd.Scope = &dbScope
			for k, v := range overlay {
				upd.Metadata[k] = v
			}
			// Clearing the experiment flag when scope flips back to a
			// non-experiments value keeps the Library tab honest -- explicit
			// false so `Boolean(meta.experiment)` still drops the row.
			if strings.ToLower(strings.TrimSpace(*body.Scope)) != "experiments" {
				if _, ok := upd.Metadata["experiment"]; !ok {
					upd.Metadata["experiment"] = false
				}
			}
		}
		if body.Name != nil {
			trimmed := strings.TrimSpace(*body.Name)
			if trimmed == "" {
				writeJSON(w, http.StatusBadRequest, errorBody("name must not be empty"))
				return
			}
			if len(trimmed) > 200 {
				writeJSON(w, http.StatusBadRequest, errorBody("name must be 1..200 chars"))
				return
			}
			upd.Name = &trimmed
		}

		c, err := store.UpdateCorpus(req.Context(), pool, id, upd)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchCorpus) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("corpus %s not found", id)))
				return
			}
			logger.WithError(err).WithField("corpus_id", id).Error("update corpus failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, corpusPayload(*c))
	}
}

type attachDocumentsBody struct {
	DocumentIDs []string `json:"document_ids"`
	AddedBy     string   `json:"added_by"`
}

func corporaAttachDocumentsHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, ok := parseCorpusID(w, req)
		if !ok {
			return
		}
		var body attachDocumentsBody
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
			return
		}
		if len(body.DocumentIDs) == 0 {
			writeJSON(w, http.StatusBadRequest,
				errorBody("document_ids must be a non-empty list"))
			return
		}
		docIDs := make([]uuid.UUID, 0, len(body.DocumentIDs))
		for _, s := range body.DocumentIDs {
			d, err := uuid.Parse(s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest,
					errorBody("invalid uuid in document_ids: "+err.Error()))
				return
			}
			docIDs = append(docIDs, d)
		}
		addedBy := body.AddedBy
		if addedBy == "" {
			addedBy = "manual"
		}
		attached, skipped, missing, err := store.AttachDocuments(
			req.Context(), pool, id, docIDs, addedBy)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchCorpus) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("corpus %s not found", id)))
				return
			}
			logger.WithError(err).WithField("corpus_id", id).Error("attach documents failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"corpus_id": id.String(),
			"attached":  attached,
			"skipped":   skipped,
			"missing":   missing,
		})
	}
}

func corporaListDocumentsHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, ok := parseCorpusID(w, req)
		if !ok {
			return
		}
		q := req.URL.Query()
		limit := 100
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				writeJSON(w, http.StatusBadRequest, errorBody("limit must be 1..500"))
				return
			}
			limit = n
		}
		offset := 0
		if v := q.Get("offset"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, errorBody("offset must be >= 0"))
				return
			}
			offset = n
		}
		name, total, docs, err := store.ListCorpusDocuments(req.Context(), pool, id, limit, offset)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchCorpus) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("corpus %s not found", id)))
				return
			}
			logger.WithError(err).WithField("corpus_id", id).Error("list corpus documents failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		out := make([]map[string]any, 0, len(docs))
		for _, d := range docs {
			row := map[string]any{
				"document_id":         d.DocumentID.String(),
				"title":               d.Title,
				"type":                d.Type,
				"metadata":            d.Metadata,
				"size_in_bytes":       d.SizeInBytes,
				"ingestion_status":    d.IngestionStatus,
				"added_at":            d.AddedAt.Format("2006-01-02T15:04:05.999999999"),
				"added_by":            d.AddedBy,
				"document_created_at": nil,
			}
			if d.DocumentCreatedAt != nil {
				row["document_created_at"] = d.DocumentCreatedAt.Format("2006-01-02T15:04:05.999999999")
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"corpus_id":   id.String(),
			"corpus_name": name,
			"total":       total,
			"limit":       limit,
			"offset":      offset,
			"documents":   out,
		})
	}
}

func corporaDetachDocumentHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		vars := mux.Vars(req)
		corpusID, err := uuid.Parse(vars["corpus_id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid corpus_id"))
			return
		}
		docID, err := uuid.Parse(vars["document_id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid document_id"))
			return
		}
		removed, err := store.DetachDocument(req.Context(), pool, corpusID, docID)
		if err != nil {
			logger.WithError(err).Error("detach document failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"corpus_id":   corpusID.String(),
			"document_id": docID.String(),
			"removed":     removed,
		})
	}
}

func corporaDeleteHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, ok := parseCorpusID(w, req)
		if !ok {
			return
		}
		err := store.SoftDeleteCorpus(req.Context(), pool, id)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchCorpus) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("corpus %s not found", id)))
				return
			}
			logger.WithError(err).WithField("corpus_id", id).Error("delete corpus failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id.String()})
	}
}

// corporaStagesHandler is the hot path the dashboard polls. On any DB
// error past the corpus-exists check, return zeroed counters so the
// gauge keeps rendering (matches the Python _ZERO_STAGES fallback at
// corpora.py:561).
func corporaStagesHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id, ok := parseCorpusID(w, req)
		if !ok {
			return
		}
		counts, err := store.CountStages(req.Context(), pool, id)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchCorpus) {
				writeJSON(w, http.StatusNotFound,
					errorBody(fmt.Sprintf("corpus %s not found", id)))
				return
			}
			logger.WithError(err).WithField("corpus_id", id).
				Warn("stages: corpus_documents join failed")
			writeJSON(w, http.StatusOK, store.StageCounts{})
			return
		}
		writeJSON(w, http.StatusOK, counts)
	}
}

func parseCorpusID(w http.ResponseWriter, req *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(mux.Vars(req)["corpus_id"])
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid corpus_id"))
		return uuid.Nil, false
	}
	return id, true
}

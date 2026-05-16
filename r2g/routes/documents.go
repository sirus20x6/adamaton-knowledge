package routes

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// RegisterDocuments wires the two read-only document endpoints.
// Caddy's path routing decides whether r2g or the legacy R2R serves
// these; this binary owns whatever paths land here.
func RegisterDocuments(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	r.HandleFunc("/v3/documents/{id}", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(mux.Vars(req)["id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid document id"))
			return
		}
		doc, err := store.GetDocument(req.Context(), pool, id)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorBody("document not found"))
			return
		}
		if err != nil {
			logger.WithError(err).WithField("id", id).Error("GetDocument failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": doc})
	}).Methods(http.MethodGet)

	r.HandleFunc("/v3/documents/{id}", func(w http.ResponseWriter, req *http.Request) {
		id, err := uuid.Parse(mux.Vars(req)["id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid document id"))
			return
		}
		n, err := store.DeleteDocument(req.Context(), pool, id)
		if err != nil {
			logger.WithError(err).WithField("id", id).Error("DeleteDocument failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		// 404 mirrors R2R's behaviour for "document not found". The
		// reindex worker treats 404 as success in its DELETE path so
		// the corpus rewire flow doesn't need a special-case here.
		if n == 0 {
			writeJSON(w, http.StatusNotFound, errorBody("document not found"))
			return
		}
		// Clear the stage row so the dashboard counters drop the doc
		// immediately. Skipping this is what caused "indexed"/"graphed"
		// to drift downward over many reindex cycles -- old stage rows
		// outlived their docs while the new docs (new UUIDs) had no
		// stages rows yet.
		if err := store.ClearStagesForDocument(req.Context(), pool, id); err != nil {
			logger.WithError(err).WithField("id", id).
				Warn("clear stages failed (document still deleted)")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results": map[string]any{"success": true},
		})
	}).Methods(http.MethodDelete)

	r.HandleFunc("/v3/documents", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		in := store.ListDocumentsInput{
			Limit:  parseIntQuery(q, "limit", 100),
			Offset: parseIntQuery(q, "offset", 0),
		}
		if cid := q.Get("collection_id"); cid != "" {
			if parsed, err := uuid.Parse(cid); err == nil {
				in.CollectionID = &parsed
			} else {
				writeJSON(w, http.StatusBadRequest, errorBody("invalid collection_id"))
				return
			}
		}
		if s := q.Get("ingestion_status"); s != "" {
			in.IngestionStat = &s
		}
		docs, total, err := store.ListDocuments(req.Context(), pool, in)
		if err != nil {
			logger.WithError(err).Error("ListDocuments failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		if docs == nil {
			docs = []store.Document{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results":       docs,
			"total_entries": total,
		})
	}).Methods(http.MethodGet)
}

func parseIntQuery(q map[string][]string, key string, def int) int {
	v, ok := q[key]
	if !ok || len(v) == 0 || v[0] == "" {
		return def
	}
	n, err := strconv.Atoi(v[0])
	if err != nil {
		return def
	}
	return n
}

func errorBody(msg string) map[string]any {
	return map[string]any{"detail": msg}
}

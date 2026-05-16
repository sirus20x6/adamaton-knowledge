package routes

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// RegisterChunks wires GET /v3/chunks. Typical query is by
// document_id (the document-reader UI hits this with pagination);
// without document_id it returns global pagination over the table.
func RegisterChunks(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	r.HandleFunc("/v3/chunks", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		in := store.ListChunksInput{
			Limit:  parseIntQuery(q, "limit", 100),
			Offset: parseIntQuery(q, "offset", 0),
		}
		if did := q.Get("document_id"); did != "" {
			parsed, err := uuid.Parse(did)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody("invalid document_id"))
				return
			}
			in.DocumentID = &parsed
		}
		chunks, total, err := store.ListChunks(req.Context(), pool, in)
		if err != nil {
			logger.WithError(err).Error("ListChunks failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		if chunks == nil {
			chunks = []store.Chunk{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results":       chunks,
			"total_entries": total,
		})
	}).Methods(http.MethodGet)
}

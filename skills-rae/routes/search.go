package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/embeddings"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/reranker"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/retrieval"
)

// RegisterSearch wires POST /v1/rae/search. Returns the raw retrieval
// output (K, H, R) without the compiled context block — Phase E's
// /v1/rae/compile builds the block on top of this.
//
// Request:
//
//	{ "query": "rename a function",
//	  "top_k": 5,
//	  "top_down_n": 2,
//	  "bottom_up_n": 50,
//	  "weights": {"alpha":0.5,"beta":0.3,"gamma":0.2,"lambda":0.4} }
//
// Response: see internal/retrieval.Response.
func RegisterSearch(r *mux.Router, pool *pgxpool.Pool, embedder *embeddings.Client, rerankClient *reranker.Client, logger *logrus.Logger) {
	ret := &retrieval.Retriever{Pool: pool, Embedder: embedder, Reranker: rerankClient}
	r.HandleFunc("/v1/rae/search", func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		var body retrieval.Request
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		if body.Query == "" {
			writeErr(w, http.StatusBadRequest, "query is required")
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		out, err := ret.Retrieve(ctx, body)
		if err != nil {
			logger.WithError(err).Error("retrieve failed")
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}).Methods(http.MethodPost)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

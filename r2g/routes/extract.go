package routes

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/extract"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/llm"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// RegisterExtract wires POST /v3/documents/{id}/extract. r2g runs the
// extraction synchronously inside the request -- a single call
// completes in 5-20 seconds for a typical paper. The handler is the
// only caller; orchestration / queueing is left to upstream (Caddy
// rate-limit, ExtractCorpusWorkflow, etc.).
//
// R2R's wire shape:
//
//	{ "results": { "message": "...", "task_id": null } }
//
// Existing callers (reindex/activities/ingest.go:ExtractEntities,
// platform/backend/app/workers/hi_worker.py) treat a 2xx with this
// envelope as success.
func RegisterExtract(
	r *mux.Router,
	pool *pgxpool.Pool,
	llmClient *llm.Client,
	chunker *chunking.Chunker,
	logger *logrus.Logger,
) {
	r.HandleFunc("/v3/documents/{id}/extract", func(w http.ResponseWriter, req *http.Request) {
		docID, err := uuid.Parse(mux.Vars(req)["id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid document id"))
			return
		}

		chunkIDs, texts, err := store.LoadChunksForExtraction(req.Context(), pool, docID)
		if err != nil {
			logger.WithError(err).WithField("doc", docID).Error("LoadChunks failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		if len(texts) == 0 {
			// R2R's behaviour on extract-without-chunks is to log
			// "No chunks found for document X" and return an error.
			// We mirror that so the caller can wait for chunks to
			// land before retrying.
			writeJSON(w, http.StatusConflict,
				errorBody("no chunks found for document; ingest must complete first"))
			return
		}

		res, err := extract.Run(req.Context(), llmClient, texts, chunker.CountTokens,
			extract.DefaultMaxBatchTokens)
		if err != nil {
			logger.WithError(err).WithField("doc", docID).Error("extract.Run failed")
			_ = store.MarkExtractionFailed(req.Context(), pool, docID, err.Error())
			writeJSON(w, http.StatusBadGateway, errorBody("extraction failed: "+err.Error()))
			return
		}

		nEnt, nRel, err := store.PersistExtraction(req.Context(), pool, docID, chunkIDs, res)
		if err != nil {
			logger.WithError(err).WithField("doc", docID).Error("PersistExtraction failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("persist error"))
			return
		}

		// Best-effort: keep the dashboard "graphed" counter honest.
		// If extract produced entities we mark stage2_at; if not (e.g.
		// LLM truncated mid-JSON or the doc had no extractable
		// content) we clear any stale stage2_at on the doc.
		if err := store.MarkStageGraphed(req.Context(), pool, docID, nEnt > 0); err != nil {
			logger.WithError(err).WithField("doc", docID).
				Warn("stage2 mark failed (extract still persisted)")
		}

		logger.WithFields(logrus.Fields{
			"doc":           docID,
			"entities":      nEnt,
			"relationships": nRel,
			"llm_calls":     res.LLMCalls,
			"input_tokens":  res.InputTokens,
		}).Info("extraction complete")

		writeJSON(w, http.StatusOK, map[string]any{
			"results": map[string]any{
				"message": "Graph extraction completed successfully.",
				"task_id": nil,
			},
		})
	}).Methods(http.MethodPost)
}

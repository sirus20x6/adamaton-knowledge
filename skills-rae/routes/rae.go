package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/compile"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/reranker"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/retrieval"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/store"
)

// CompileRequest is the public POST /v1/rae/compile shape. Mirrors the
// retrieval.Request fields used by the inner pipeline plus
// compilation-only knobs (output_contract, budget).
type CompileRequest struct {
	Query           string             `json:"query"`
	TaskID          string             `json:"task_id,omitempty"`
	TopK            int                `json:"top_k,omitempty"`
	TopDownN        int                `json:"top_down_n,omitempty"`
	BottomUpN       int                `json:"bottom_up_n,omitempty"`
	L1N             int                `json:"l1_n,omitempty"`
	BudgetTokens    int                `json:"budget_tokens,omitempty"`
	OutputContract  string             `json:"output_contract,omitempty"`
	Weights         retrieval.Weights  `json:"weights,omitempty"`
	AffWeights      compile.AffWeights `json:"aff_weights,omitempty"`
}

// CompileResponse is the public shape. We bubble the retrieval
// diagnostics up so callers can audit which communities matched and
// where the time went per call.
type CompileResponse struct {
	Context        string                          `json:"context"`
	SelectedSkills []retrieval.SelectedSkill       `json:"selected_skills"`
	RescueAttached []compile.AttachedCue           `json:"rescue_attached"`
	Skipped        []compile.SkippedCue            `json:"skipped_cues"`
	Diagnostics    retrieval.Diagnostics           `json:"diagnostics"`
}

// RegisterCompile wires POST /v1/rae/compile. The whole pipeline:
// retrieval -> compile -> rendered context block. Returns the
// compiled block plus the structured metadata the delegator can
// store in evo.skill_rae_traces.
func RegisterCompile(r *mux.Router, pool *pgxpool.Pool, embedder *embeddings.Client, rerankClient *reranker.Client, logger *logrus.Logger) {
	// Compile needs the query + subunit vectors so the Aff features and
	// rescue-dedup can use real bge-m3 cosine instead of the Jaccard
	// token fallback. /v1/rae/search keeps ReturnVectors=false because
	// its callers don't compile and don't want 1024 floats per subunit
	// shipped over the wire.
	ret := &retrieval.Retriever{Pool: pool, Embedder: embedder, Reranker: rerankClient, ReturnVectors: true}
	r.HandleFunc("/v1/rae/compile", func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		var body CompileRequest
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

		// Stage 1 -- retrieval.
		retIn := retrieval.Request{
			Query:    body.Query,
			TopK:     body.TopK,
			TopDownN: body.TopDownN,
			BottomUpN: body.BottomUpN,
			L1N:      body.L1N,
			Weights:  body.Weights,
		}
		retResp, err := ret.Retrieve(ctx, retIn)
		if err != nil {
			logger.WithError(err).Error("retrieve failed in /v1/rae/compile")
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Stage 2 -- compilation.
		c := compile.Compile(compile.Request{
			Query:          body.Query,
			OutputContract: body.OutputContract,
			BudgetTokens:   body.BudgetTokens,
			Affinities:     body.AffWeights,
			RetrieveResp:   retResp,
		})

		// Stage 3 -- telemetry (best-effort). Sum stage timings as a
		// rough elapsed. Fire-and-forget in a background goroutine so
		// the response isn't held up by an unhealthy DB.
		go writeTrace(pool, body, c, retResp, logger)

		writeJSON(w, http.StatusOK, CompileResponse{
			Context:        c.Context,
			SelectedSkills: c.Selected,
			RescueAttached: c.RescueAttached,
			Skipped:        c.Skipped,
			Diagnostics:    retResp.Diagnostics,
		})
	}).Methods(http.MethodPost)
}

func writeTrace(pool *pgxpool.Pool, req CompileRequest, c *compile.Compiled, retResp *retrieval.Response, logger *logrus.Logger) {
	if pool == nil || c == nil || retResp == nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(c.Selected))
	for _, s := range c.Selected {
		ids = append(ids, s.SkillID)
	}
	var elapsed int
	for _, ms := range retResp.Diagnostics.StageMs {
		elapsed += int(ms)
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.InsertTrace(bg, pool, req.TaskID, req.Query, "rae", ids, len(c.RescueAttached), elapsed); err != nil {
		logger.WithError(err).Debug("trace insert failed")
	}
}

package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/observe"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/reranker"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// hybridEnabled toggles the BM25+vector RRF fusion path in store.
// Default ON; flip to "0", "false", "off" to fall back to pure
// pgvector kNN (the v1 behaviour). Read once at request time so an
// operator can toggle without restarting -- the cost is negligible
// next to the search itself.
func hybridEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("R2G_HYBRID_ENABLED")))
	switch v {
	case "", "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// graphVoteEnabled toggles the TGS-RAG Graph-to-Text seed-vote path.
// Default ON; only "0" or "false" disable. Read per-request so an
// operator can flip it live without restarting r2g.
func graphVoteEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("R2G_GRAPH_VOTE_ENABLED")))
	switch v {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// graphVoteMaxSeeds reads R2G_GRAPH_VOTE_MAX_SEEDS (default 10). An
// unparseable or non-positive value falls back to the default rather
// than failing the request.
func graphVoteMaxSeeds() int {
	const def = 10
	v := strings.TrimSpace(os.Getenv("R2G_GRAPH_VOTE_MAX_SEEDS"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// SearchRequest mirrors what every existing caller actually POSTs.
// We accept R2R's "search_mode" / "search_settings" envelope plus a
// bare-minimum top-level `query`. Modes other than "custom" use
// reasonable defaults; "custom" passes search_settings through.
type SearchRequest struct {
	Query          string                 `json:"query"`
	SearchMode     string                 `json:"search_mode,omitempty"`
	SearchSettings map[string]interface{} `json:"search_settings,omitempty"`
}

// RegisterSearch wires POST /v3/retrieval/search. r2g implements
// semantic-only search in v1; full-text + RRF fusion lands once we
// have profile data to know whether it's worth the second query.
//
// The response shape matches R2R's WrappedSearchResponse:
//
//	{ "results": { "chunk_search_results": [ {...}, ... ] } }
//
// existing callers (dashboard skills_search_graph parser, the SPA
// research view) deserialize against that exact shape.
func RegisterSearch(r *mux.Router, pool *pgxpool.Pool, embedder *embeddings.Client, rerankerClient *reranker.Client, metrics *observe.Metrics, logger *logrus.Logger) {
	r.HandleFunc("/v3/retrieval/search", func(w http.ResponseWriter, req *http.Request) {
		var body SearchRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid request body"))
			return
		}
		if body.Query == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("query is required"))
			return
		}

		in := store.SearchInput{Limit: 10}
		applyMode(&in, body.SearchMode)
		if err := applySearchSettings(&in, body.SearchSettings); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		gvOverride := extractGraphVoteOverride(body.SearchSettings)
		effectiveGraphVote := graphVoteEnabled()
		if gvOverride != nil {
			effectiveGraphVote = *gvOverride
		}

		vec, err := embedder.EmbedOne(req.Context(), body.Query)
		if err != nil {
			logger.WithError(err).Error("embed query failed")
			writeJSON(w, http.StatusBadGateway, errorBody("embedder unavailable"))
			return
		}
		in.QueryVec = vec
		in.QueryText = body.Query

		var (
			hits     []store.SearchHit
			warnings map[string]string
			seedIDs  []uuid.UUID
		)
		if hybridEnabled() {
			// Stage-1: pull a wider candidate set when we plan to rerank,
			// so the cross-encoder has room to promote hits buried in
			// the middle of the RRF list. ``deepLimit`` overrides the
			// caller's limit only for the store call; we still trim to
			// the requested limit after reranking.
			deepLimit := in.Limit
			if rerankerClient.Enabled() && deepLimit < 20 {
				deepLimit = 20
			}
			deepIn := in
			deepIn.Limit = deepLimit

			// TGS-RAG Channel 1: derive seed entities from the query and
			// hand them to HybridSearch so it can run the graph-vote
			// goroutine alongside semantic + BM25. Every degrade path
			// here (env off, extraction error, no matches) leaves the
			// response intact and falls back to the prior 2-channel RRF.
			if effectiveGraphVote {
				// Multi-corpus search shouldn't constrain seeds to one
				// corpus; pass "" so ExtractQuerySeedEntities is unscoped.
				var corpusIDStr string
				switch len(in.CollectionIDs) {
				case 1:
					corpusIDStr = in.CollectionIDs[0].String()
				default:
					corpusIDStr = ""
				}
				seeds, seedErr := store.ExtractQuerySeedEntities(req.Context(), pool, body.Query, corpusIDStr, graphVoteMaxSeeds())
				switch {
				case seedErr != nil:
					logger.WithError(seedErr).Warn("graph vote seed extraction failed; continuing without seeds")
					metrics.GraphVoteOutcome("error")
				case len(seeds) == 0:
					metrics.GraphVoteOutcome("empty")
				default:
					seedIDs = make([]uuid.UUID, 0, len(seeds))
					for _, s := range seeds {
						seedIDs = append(seedIDs, s.ID)
					}
					deepIn.SeedEntities = seedIDs
					metrics.GraphVoteOutcome("ok")
				}
			}

			hits, warnings, err = store.HybridSearch(req.Context(), pool, deepIn)
		} else {
			hits, err = store.SemanticSearch(req.Context(), pool, in)
		}
		if err != nil {
			logger.WithError(err).Error("search failed")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		if hits == nil {
			hits = []store.SearchHit{}
		}
		if len(warnings) > 0 {
			logger.WithField("warnings", warnings).Warn("hybrid search partial failure")
		}

		// Stage-2: cross-encoder rerank on the candidate set.
		// Best-effort: any sidecar failure logs and falls back to the
		// pre-rerank order so a wedged sidecar doesn't kill /search.
		if rerankerClient.Enabled() && len(hits) > 1 {
			// 30s budget per call. With ms-marco-MiniLM-L-6-v2 on Pi 5
			// CPU a 20-passage batch lands in ~3s; the headroom guards
			// against a cold-start tokenizer load or a transient sidecar
			// pause. Far above this and the user is better off seeing
			// the RRF order than waiting -- the fallback path kicks in
			// on deadline-exceeded and logs a warning.
			rerankCtx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
			passages := make([]string, len(hits))
			for i, h := range hits {
				passages[i] = h.Text
			}
			ranked, rerankErr := rerankerClient.Rerank(rerankCtx, body.Query, passages, 0)
			cancel()
			if rerankErr != nil && !errors.Is(rerankErr, reranker.ErrDisabled) {
				logger.WithError(rerankErr).Warn("reranker failed; falling back to RRF order")
			} else if len(ranked) > 0 {
				// Replace .Score with the reranker score so downstream
				// callers see a calibrated value, and re-order hits to
				// match. We trust the reranker's order over RRF when
				// it answers -- that's the whole point of stage 2.
				out := make([]store.SearchHit, 0, len(ranked))
				for _, r := range ranked {
					if r.Index < 0 || r.Index >= len(hits) {
						continue
					}
					h := hits[r.Index]
					h.Score = r.Score
					out = append(out, h)
				}
				hits = out
			}
		}

		// Trim to the caller's requested limit AFTER stage-2 so the
		// reranker had the full deep candidate set to choose from.
		if in.Limit > 0 && len(hits) > in.Limit {
			hits = hits[:in.Limit]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results": map[string]any{
				"chunk_search_results": hits,
				// R2R's response includes empty graph_search_results /
				// web_search_results slots when those backends aren't
				// engaged. Mirror that so callers don't blow up on a
				// missing key.
				"graph_search_results":     []any{},
				"web_search_results":       []any{},
				"document_search_results":  []any{},
				"context_document_results": []any{},
			},
		})

		logger.WithFields(logrus.Fields{
			"hits":                 len(hits),
			"graph_vote_seeds":     len(seedIDs),
			"graph_vote_effective": effectiveGraphVote,
		}).Info("search completed")
	}).Methods(http.MethodPost)
}

// applyMode maps R2R's preset modes to our search input. "basic" and
// "advanced" set sane defaults; "custom" expects the caller to set
// limit / filters via search_settings.
func applyMode(in *store.SearchInput, mode string) {
	switch mode {
	case "basic":
		in.Limit = 10
	case "advanced":
		// In R2R, advanced enables hybrid (semantic + BM25). r2g v1 is
		// semantic-only -- the limit bump is the only observable
		// difference. Returning fewer-but-higher-quality hits via a
		// hybrid retrieval is what Phase 2.5 brings.
		in.Limit = 20
	case "", "custom":
		// stay at defaults set by caller
	}
}

// applySearchSettings extracts the bits of R2R's SearchSettings
// envelope that r2g actually uses: limit and a collection_ids /
// document_id filter. Anything else is silently ignored -- we don't
// reject unknown keys so the upgrade path is non-breaking.
func applySearchSettings(in *store.SearchInput, s map[string]interface{}) error {
	if s == nil {
		return nil
	}
	if v, ok := s["limit"]; ok {
		if n, ok := toInt(v); ok {
			in.Limit = n
		}
	}
	filters, _ := s["filters"].(map[string]interface{})
	if filters == nil {
		return nil
	}

	// collection_ids: {"$overlap": ["uuid", ...]} -- the shape every
	// existing caller passes today (see
	// dashboard/apiserver/skills_search_graph.go).
	if cf, ok := filters["collection_ids"].(map[string]interface{}); ok {
		if arr, ok := cf["$overlap"].([]interface{}); ok {
			for _, raw := range arr {
				s, ok := raw.(string)
				if !ok {
					continue
				}
				id, err := uuid.Parse(s)
				if err != nil {
					return errorWrap("filters.collection_ids.$overlap[]", err)
				}
				in.CollectionIDs = append(in.CollectionIDs, id)
			}
		}
	}

	// document_id: {"$eq": "uuid"} -- R2R's filter shape; rare but
	// supported.
	if df, ok := filters["document_id"].(map[string]interface{}); ok {
		if eq, ok := df["$eq"].(string); ok {
			id, err := uuid.Parse(eq)
			if err != nil {
				return errorWrap("filters.document_id.$eq", err)
			}
			in.DocumentID = &id
		}
	}
	return nil
}

// extractGraphVoteOverride reads search_settings.graph_vote as a *bool.
// Returns nil when the key is absent or not a boolean, signalling
// "follow R2G_GRAPH_VOTE_ENABLED." Lets the bench A/B both states in
// a single process without operator-coordinated r2g restarts.
func extractGraphVoteOverride(s map[string]interface{}) *bool {
	if s == nil {
		return nil
	}
	v, ok := s["graph_vote"]
	if !ok {
		return nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func errorWrap(field string, err error) error {
	return &fieldError{field: field, err: err}
}

type fieldError struct {
	field string
	err   error
}

func (e *fieldError) Error() string {
	return e.field + ": " + e.err.Error()
}

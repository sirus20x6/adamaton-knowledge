package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// maxEpisodeBytes caps a single episode body. Episodes are short raw
// captures (a chat turn, a skill observation, a claim), not documents;
// 1 MB is generous headroom.
const maxEpisodeBytes = 1 << 20

type episodeIn struct {
	Kind           string         `json:"kind"`
	Content        string         `json:"content"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	OccurredAt     *time.Time     `json:"occurred_at,omitempty"`
	ForesightUntil *time.Time     `json:"foresight_until,omitempty"`
}

// RegisterEpisodes wires the H-Mem episodic-capture endpoints:
//
//	POST /v3/episodes              write one raw episode
//	GET  /v3/episodes?kind=&since=&foresight=active   read recent episodes
//
// Embedding is computed inline when an embedder is configured; absent
// one, the row stores NULL and the consolidator worker embeds it later.
// These endpoints are r2g extensions, not part of upstream R2R.
func RegisterEpisodes(
	r *mux.Router,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	logger *logrus.Logger,
) {
	r.HandleFunc("/v3/episodes", func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxEpisodeBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("read body"))
			return
		}
		var in episodeIn
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid json"))
			return
		}
		if in.Kind == "" || in.Content == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("kind and content are required"))
			return
		}

		var embedding []float32
		if embedder != nil {
			vecs, err := embedder.Embed(req.Context(), []string{in.Content})
			if err != nil {
				logger.WithError(err).Warn("episode embed failed; storing without vector")
			} else if len(vecs) == 1 {
				embedding = vecs[0]
			}
		}

		id, err := store.InsertEpisode(req.Context(), pool, store.InsertEpisodeInput{
			Kind:           in.Kind,
			Content:        in.Content,
			Metadata:       in.Metadata,
			Embedding:      embedding,
			OccurredAt:     in.OccurredAt,
			ForesightUntil: in.ForesightUntil,
		})
		if err != nil {
			logger.WithError(err).Error("insert episode")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"results": map[string]any{"id": id}})
	}).Methods(http.MethodPost)

	r.HandleFunc("/v3/episodes", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		f := store.ListEpisodesFilter{
			Kind:            q.Get("kind"),
			ForesightActive: q.Get("foresight") == "active",
		}
		if s := q.Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody("since must be RFC3339"))
				return
			}
			f.Since = &t
		}
		if l := q.Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil {
				f.Limit = n
			}
		}

		eps, err := store.ListEpisodes(req.Context(), pool, f)
		if err != nil {
			logger.WithError(err).Error("list episodes")
			writeJSON(w, http.StatusInternalServerError, errorBody("internal error"))
			return
		}
		if eps == nil {
			eps = []store.Episode{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": eps})
	}).Methods(http.MethodGet)
}

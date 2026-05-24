// Command r2g is the Go re-implementation of R2R's /v3/* HTTP surface
// for the deepresearch + evo stack. It runs as a sibling of the
// Python R2R container during the strangler-fig migration: Caddy
// path-routes the endpoints r2g has implemented to this binary, and
// everything else still hits the Python service.
//
// Configuration is read from environment variables:
//
//	R2G_PORT            — listen port (default 7373; adjacent to R2R's 7272)
//	R2G_DATABASE_URL    — postgres DSN to the shared deepresearch DB
//	R2G_LOG_LEVEL       — logrus level (default "info")
//	R2G_LOG_FORMAT      — "text" or "json" (default "text")
//	BGE_EMBED_URL       — OpenAI-compatible embeddings sidecar (preferred;
//	                      points at the native llama-server bge-embed)
//	BGE_SIDECAR_URL     — legacy fallback for the same; deprecated in
//	                      favour of BGE_EMBED_URL
//	R2G_EMBED_MODEL     — model name to send to the sidecar (default "bge-m3")
//	OPENAI_API_BASE     — OpenAI-compatible chat-completions endpoint (vLLM)
//	OPENAI_API_KEY      — auth token for the LLM endpoint (often a placeholder)
//	R2G_LLM_MODEL       — model name to send (default "qwen3.6-27b-nvfp8")
//	R2G_PPROF_PORT      — pprof + /metrics side listener (default 7374, "" to disable)
//	BLOBSTORE_ENDPOINT  — S3 / Garage endpoint for document uploads (bucket
//	                      "dr-uploads"); with BLOBSTORE_ACCESS_KEY /
//	                      BLOBSTORE_SECRET_KEY / BLOBSTORE_REGION. Absent ->
//	                      /platform/sources/upload + file:/s3: ingest return 503.
//
// The binary is single-process, single-purpose, and stateless except
// for postgres. Restart-safe; graceful shutdown drains in-flight
// requests on SIGTERM/SIGINT.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof" // GET /debug/pprof/* on the side port
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/blobstore"
	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/llm"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/observe"
	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/reranker"
	"github.com/sirus20x6/adamaton-knowledge/r2g/routes"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "r2g: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger()

	port := envOr("R2G_PORT", "7373")
	dsn := os.Getenv("R2G_DATABASE_URL")
	if dsn == "" {
		return errors.New("R2G_DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.DialPostgres(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.WithField("port", port).Info("postgres pool ready")

	// BGE_EMBED_URL is the post-Python-elim variable; BGE_SIDECAR_URL is
	// the legacy name kept as a fallback so callers that haven't been
	// re-deployed continue to work against the old sidecar.
	embedURL := os.Getenv("BGE_EMBED_URL")
	if embedURL == "" {
		embedURL = os.Getenv("BGE_SIDECAR_URL")
	}
	embedModel := envOr("R2G_EMBED_MODEL", "bge-m3")
	var embedder *embeddings.Client
	if embedURL != "" {
		embedder = embeddings.NewClient(embedURL, embedModel)
		logger.WithFields(map[string]any{
			"url":   embedURL,
			"model": embedModel,
		}).Info("embeddings client ready")
	} else {
		logger.Warn("neither BGE_EMBED_URL nor BGE_SIDECAR_URL set; /v3/retrieval/search will return 503")
	}

	// Stage-2 reranker sidecar. Absent ``R2G_RERANKER_URL`` -> search
	// returns the RRF order unchanged. We instantiate unconditionally
	// so the route handler can branch on ``Enabled()`` cheaply.
	rerankerURL := os.Getenv("R2G_RERANKER_URL")
	rerankerClient := reranker.NewClient(rerankerURL)
	if rerankerClient.Enabled() {
		logger.WithField("url", rerankerURL).Info("reranker client ready")
	} else {
		logger.Info("R2G_RERANKER_URL not set; reranking disabled (RRF order preserved)")
	}

	chunker, err := chunking.Default()
	if err != nil {
		return fmt.Errorf("init chunker: %w", err)
	}

	// Document-blob store for /platform/sources/upload + the file:/s3:
	// ingest schemes. Bucket "dr-uploads" is shared with the platform
	// plugin-host. Credentials/endpoint come from the canonical BLOBSTORE_*
	// env (see core/blobstore.ConfigFromEnv). Absent BLOBSTORE_ENDPOINT ->
	// nil store; the handlers return 503 rather than failing startup, so
	// search/ingest of non-upload sources keeps working.
	var blobs blobstore.Backend
	blobCfg := blobstore.ConfigFromEnv("dr-uploads")
	if blobCfg.Endpoint != "" {
		s3, err := blobstore.NewS3(ctx, blobCfg)
		if err != nil {
			return fmt.Errorf("init blob store: %w", err)
		}
		if err := s3.EnsureBucket(ctx); err != nil {
			return fmt.Errorf("ensure blob bucket %q: %w", blobCfg.Bucket, err)
		}
		blobs = s3
		logger.WithFields(logrus.Fields{
			"endpoint": blobCfg.Endpoint,
			"bucket":   blobCfg.Bucket,
		}).Info("blob store ready (dr-uploads)")
	} else {
		logger.Warn("BLOBSTORE_ENDPOINT not set; /platform/sources/upload + file:/s3: ingest will return 503")
	}

	llmURL := os.Getenv("OPENAI_API_BASE")
	llmKey := os.Getenv("OPENAI_API_KEY")
	llmModel := envOr("R2G_LLM_MODEL", "qwen3.6-27b-nvfp8")
	var llmClient *llm.Client
	if llmURL != "" {
		llmClient = llm.NewClient(llmURL, llmKey, llmModel)
		logger.WithFields(logrus.Fields{
			"url":   llmURL,
			"model": llmModel,
		}).Info("llm client ready")
	} else {
		logger.Warn("OPENAI_API_BASE not set; /v3/documents/{id}/extract will return 503")
	}

	graphVoteOn := os.Getenv("R2G_GRAPH_VOTE_ENABLED") != "false" && os.Getenv("R2G_GRAPH_VOTE_ENABLED") != "0"
	logger.WithField("enabled", graphVoteOn).Info("graph_vote (TGS-RAG Graph-to-Text) ready")

	metricsReg := prometheus.NewRegistry()
	metrics := observe.NewMetrics(metricsReg)

	r := mux.NewRouter()
	r.Use(observe.Middleware(metrics, logger))
	routes.RegisterHealth(r, pool, routes.HealthDeps{
		BGEURL: embedURL,
		LLMURL: llmURL,
	}, logger)
	routes.RegisterDocuments(r, pool, logger)
	routes.RegisterChunks(r, pool, logger)
	routes.RegisterFigures(r, pool, logger)
	routes.RegisterCorpora(r, pool, logger)
	routes.RegisterGraph(r, pool, logger)
	if embedder != nil {
		routes.RegisterSearch(r, pool, embedder, rerankerClient, metrics, logger)
		routes.RegisterIngest(r, pool, embedder, chunker, logger)
		routes.RegisterSources(r, pool, embedder, chunker, blobs, logger)
	}
	if llmClient != nil {
		routes.RegisterExtract(r, pool, llmClient, chunker, logger)
	}

	// pprof + /metrics on a side port so they aren't exposed by the
	// public service. R2G_PPROF_PORT="" disables.
	pprofPort := envOr("R2G_PPROF_PORT", "7374")
	var pprofSrv *http.Server
	if pprofPort != "" {
		mp := http.NewServeMux()
		mp.Handle("/metrics", promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{Registry: metricsReg}))
		// net/http/pprof registers its handlers on DefaultServeMux on
		// import. Mount that on /debug/pprof/ so we don't accidentally
		// share routes with the main listener.
		mp.Handle("/debug/pprof/", http.DefaultServeMux)
		pprofSrv = &http.Server{Addr: ":" + pprofPort, Handler: mp, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			logger.WithField("addr", pprofSrv.Addr).Info("pprof + /metrics listening")
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.WithError(err).Warn("pprof listener failed")
			}
		}()
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// WriteTimeout left unset because some endpoints (search, ingest)
		// take longer than any sensible global ceiling; the per-handler
		// context deadline is the right knob.
		IdleTimeout: 120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.WithField("addr", srv.Addr).Info("r2g listening")
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutCancel()
		if pprofSrv != nil {
			_ = pprofSrv.Shutdown(shutCtx)
		}
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.WithError(err).Warn("graceful shutdown timed out")
			return fmt.Errorf("shutdown: %w", err)
		}
		logger.Info("r2g stopped")
		return nil
	}
}

func newLogger() *logrus.Logger {
	lg := logrus.New()
	if lvl, err := logrus.ParseLevel(envOr("R2G_LOG_LEVEL", "info")); err == nil {
		lg.SetLevel(lvl)
	}
	if envOr("R2G_LOG_FORMAT", "text") == "json" {
		lg.SetFormatter(&logrus.JSONFormatter{})
	}
	return lg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

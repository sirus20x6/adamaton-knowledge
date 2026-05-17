// Command skills-rae is the SkillRAE retrieval + context-compilation
// service. It owns the multi-level skill graph (communities ↔ skills ↔
// subunits) and serves /v1/rae/compile and /v1/rae/search to the
// delegator. The offline indexer that populates the graph runs in the
// sibling “skills-rae-worker“ binary against the Temporal queue
// “skills-rae“.
//
// Configuration is read from environment variables:
//
//	SKILLS_RAE_PORT          — listen port (default 7376)
//	SKILLS_RAE_DSN           — postgres DSN (shared evo DB)
//	SKILLS_RAE_LOG_LEVEL     — logrus level (default "info")
//	SKILLS_RAE_LOG_FORMAT    — "text" or "json" (default "text")
//	BGE_EMBED_URL            — OpenAI-compatible embeddings sidecar
//	                           (preferred; points at the native llama-server
//	                           bge-embed)
//	BGE_SIDECAR_URL          — legacy fallback for the same; deprecated
//	SKILLS_RAE_EMBED_MODEL   — model name (default "bge-m3")
//	SKILLS_RAE_RERANKER_URL  — optional cross-encoder rerank sidecar
//	SKILLS_RAE_PPROF_PORT    — pprof + /metrics side listener (default 7377, "" disables)
//
// Stateless except for postgres; restart-safe; graceful shutdown drains
// in-flight requests on SIGTERM/SIGINT. Mirrors the r2g binary's shape
// so ops conventions (pprof side port, Prometheus on the same port,
// /healthz on the public port) stay consistent across the fleet.
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

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/observe"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/reranker"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/routes"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "skills-rae: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger()

	port := envOr("SKILLS_RAE_PORT", "7376")
	dsn := os.Getenv("SKILLS_RAE_DSN")
	if dsn == "" {
		return errors.New("SKILLS_RAE_DSN is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.DialPostgres(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.WithField("port", port).Info("postgres pool ready")

	// BGE_EMBED_URL is the post-Python-elim variable (points at the new
	// llama-server bge-embed catalog service); BGE_SIDECAR_URL is the
	// legacy name kept as a fallback during the migration.
	embedURL := os.Getenv("BGE_EMBED_URL")
	if embedURL == "" {
		embedURL = os.Getenv("BGE_SIDECAR_URL")
	}
	embedModel := envOr("SKILLS_RAE_EMBED_MODEL", "bge-m3")
	embedder := embeddings.NewClient(embedURL, embedModel)
	if embedder.Enabled() {
		logger.WithFields(map[string]any{"url": embedURL, "model": embedModel}).Info("embedder ready")
	} else {
		logger.Warn("neither BGE_EMBED_URL nor BGE_SIDECAR_URL set; /v1/rae/* will return 500")
	}

	// Optional cross-encoder reranker. Off by default — the cost on
	// Pi CPU (~3s per call) is significant; the env knob lets ops
	// opt in once the speed/quality tradeoff is acceptable.
	rerankURL := os.Getenv("SKILLS_RAE_RERANKER_URL")
	rerankClient := reranker.NewClient(rerankURL)
	if rerankClient.Enabled() {
		logger.WithField("url", rerankURL).Info("reranker client ready")
	} else {
		logger.Info("SKILLS_RAE_RERANKER_URL not set; bi-encoder ordering used")
	}

	metricsReg := prometheus.NewRegistry()
	metrics := observe.NewMetrics(metricsReg)

	r := mux.NewRouter()
	r.Use(observe.Middleware(metrics, logger))
	routes.RegisterHealth(r, pool, logger)
	routes.RegisterSearch(r, pool, embedder, rerankClient, logger)
	routes.RegisterCompile(r, pool, embedder, rerankClient, logger)
	routes.RegisterDebug(r, pool, logger)

	pprofPort := envOr("SKILLS_RAE_PPROF_PORT", "7377")
	var pprofSrv *http.Server
	if pprofPort != "" {
		mp := http.NewServeMux()
		mp.Handle("/metrics", promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{Registry: metricsReg}))
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
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.WithField("addr", srv.Addr).Info("skills-rae listening")
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
		logger.Info("skills-rae stopped")
		return nil
	}
}

func newLogger() *logrus.Logger {
	lg := logrus.New()
	if lvl, err := logrus.ParseLevel(envOr("SKILLS_RAE_LOG_LEVEL", "info")); err == nil {
		lg.SetLevel(lvl)
	}
	if envOr("SKILLS_RAE_LOG_FORMAT", "text") == "json" {
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

// reindex-worker is the Temporal worker for the arxiv reindex
// subsystem. Replaces the bash retry loop at
// dashboard/reindex-arxiv-latex/reindex-arxiv-latex.sh — runs the same
// per-paper ingest flow but with concurrent fan-out, durable retries,
// and queryable progress.
//
// Deployed on the Pi via systemd/reindex-worker.service. Connects to
// the deepresearch postgres (deepresearch.documents +
// platform.corpus_documents) and the platform's /platform/sources/ingest
// endpoint.
package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	"github.com/sirus20x6/adamaton-knowledge/reindex/workerentry"
	reindexworkflows "github.com/sirus20x6/adamaton-knowledge/reindex/workflows"
)

const (
	// The reindex worker reads/writes the deepresearch DB (not the evo
	// one) — its DSN is distinct from evo-worker's. On the Pi this
	// usually points at the docker-compose service "deepresearch-postgres-1".
	defaultDSN       = "postgres://postgres@localhost:5433/deepresearch?sslmode=disable"
	defaultTemporal  = "localhost:7233"
	defaultNamespace = "default"
	defaultPlatform  = "https://deepresearch.local"

	// Per-queue concurrency defaults. Ingest at 4 to stay inside what
	// the CPU-only BGE-M3 sidecar can sustain (8 concurrent ingests
	// each firing batches of up to 32 embeds saturated BGE's OpenMP
	// pool and triggered embed-failed retry storms). Extract at 4
	// matches vLLM's effective concurrent throughput for the qwen3.6
	// model at our prompt sizes. Override via
	// REINDEX_INGEST_CONCURRENCY / REINDEX_EXTRACT_CONCURRENCY.
	defaultIngestConcurrency  = 4
	defaultExtractConcurrency = 4
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	dsn := envOr("REINDEX_POSTGRES_DSN", envOr("DEEPRESEARCH_POSTGRES_DSN", defaultDSN))
	temporalAddr := envOr("TEMPORAL_ADDRESS", defaultTemporal)
	namespace := envOr("TEMPORAL_NAMESPACE", defaultNamespace)
	platformURL := envOr("PLATFORM_BASE_URL", defaultPlatform)
	r2gURL := os.Getenv("R2G_URL")
	// ARXIV_SKIP_URL: optional. When set, the worker preflights every
	// paper against the arxiv-skip sidecar (tools/arxiv-skip) and
	// short-circuits papers with no LaTeX source before the heavy
	// deepresearch ingest pipeline runs. Empty disables the preflight
	// (fail-open: the slow path still produces the right answer).
	arxivSkipURL := os.Getenv("ARXIV_SKIP_URL")
	// Embedding sidecar for the worker-side ChunkEmbedPersist activity.
	// Prefer the native llama-server endpoint (BGE_EMBED_URL); fall back to
	// the legacy Python sidecar (BGE_SIDECAR_URL) during the migration.
	embedURL := envOr("BGE_EMBED_URL", os.Getenv("BGE_SIDECAR_URL"))
	embedModel := envOr("REINDEX_EMBED_MODEL", "bge-m3")

	ingestConcurrency := envInt("REINDEX_INGEST_CONCURRENCY", defaultIngestConcurrency)
	extractConcurrency := envInt("REINDEX_EXTRACT_CONCURRENCY", defaultExtractConcurrency)

	logger.WithFields(logrus.Fields{
		"temporal":            temporalAddr,
		"namespace":           namespace,
		"ingest_queue":        reindexworkflows.TaskQueueIngest,
		"extract_queue":       reindexworkflows.TaskQueueExtract,
		"ingest_concurrency":  ingestConcurrency,
		"extract_concurrency": extractConcurrency,
		"platform":            platformURL,
		"r2g":                 r2gURL,
		"arxiv_skip":          arxivSkipURL,
		"dsn":                 redactDSN(dsn),
	}).Info("reindex-worker starting")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("open deepresearch postgres pool: %v", err)
	}
	defer pool.Close()

	c, err := dialTemporalWithRetry(ctx, temporalAddr, namespace, logger)
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	// HTTP client used by activities. The 20-minute ceiling is sized
	// for the longest call (ExtractEntities, synchronous LLM-driven
	// graph extraction inside R2R under orchestration.provider="simple").
	// Shorter activities (Ingest=240s, Delete=30s) get their effective
	// timeout from Temporal's per-activity StartToCloseTimeout, which
	// cancels the request context long before this ceiling matters.
	// Explicit Transport so connection pooling is bounded and idle
	// conns get reaped instead of accumulating per-request.
	httpClient := &http.Client{
		Timeout: 20 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	// Two workers in the same process, one per task queue. Each has
	// its own activity-execution pool so an extract retry-storm
	// (vLLM down) can't starve ingest slots and vice versa. They
	// share the Temporal client and the pgxpool.
	ingestW := worker.New(c, reindexworkflows.TaskQueueIngest, worker.Options{
		Identity:                               "reindex-worker:ingest",
		MaxConcurrentActivityExecutionSize:     ingestConcurrency,
		MaxConcurrentWorkflowTaskExecutionSize: ingestConcurrency,
	})
	extractW := worker.New(c, reindexworkflows.TaskQueueExtract, worker.Options{
		Identity:                               "reindex-worker:extract",
		MaxConcurrentActivityExecutionSize:     extractConcurrency,
		MaxConcurrentWorkflowTaskExecutionSize: extractConcurrency,
	})

	// Registration helpers live in reindex/workerentry so the
	// unified adamaton-worker can register the same workflows + activities
	// from its own main; here we host both queues in one process.
	deps := workerentry.Deps{
		Pool:         pool,
		HTTPClient:   httpClient,
		Logger:       logger,
		PlatformURL:  platformURL,
		R2GURL:       r2gURL,
		ArxivSkipURL: arxivSkipURL,
		EmbedURL:     embedURL,
		EmbedModel:   embedModel,
	}
	workerentry.RegisterIngest(ingestW, deps)
	workerentry.RegisterExtract(extractW, deps)

	if sess, err := workerregistry.Register(ctx, pool, workerregistry.RegisterOptions{
		Identity: "reindex-worker",
		DeclaredQueues: []string{
			reindexworkflows.TaskQueueIngest,
			reindexworkflows.TaskQueueExtract,
		},
		Logger: logger,
	}); err != nil {
		logger.WithError(err).Warn("worker self-registration failed; worker continues without a workers row")
	} else {
		defer sess.Close(context.Background())
	}

	// Start both workers, then block on a single InterruptCh.
	// worker.Start is non-blocking; the workers run in their own
	// goroutines and we wait here for SIGTERM.
	if err := ingestW.Start(); err != nil {
		log.Fatalf("ingest worker.Start: %v", err)
	}
	defer ingestW.Stop()
	if err := extractW.Start(); err != nil {
		log.Fatalf("extract worker.Start: %v", err)
	}
	defer extractW.Stop()

	logger.Info("reindex-worker ready (ingest + extract)")
	<-worker.InterruptCh()
	logger.Info("reindex-worker stopping")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envInt reads an integer-valued env var with a fallback. A non-integer
// value is treated as unset (the fallback wins) so a misconfigured env
// can't silently floor concurrency to zero.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "[invalid-dsn]"
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}

func dialTemporalWithRetry(ctx context.Context, addr, ns string, logger *logrus.Logger) (client.Client, error) {
	const (
		base    = 5 * time.Second
		maxStep = 6
	)
	var attempts atomic.Int32
	for {
		c, err := client.Dial(client.Options{
			HostPort:  addr,
			Namespace: ns,
			Identity:  "reindex-worker",
		})
		if err == nil {
			if a := attempts.Load(); a > 0 {
				logger.WithField("attempts", a).Info("temporal connection established after retries")
			}
			return c, nil
		}
		a := attempts.Add(1)
		step := int32(a)
		if step > maxStep {
			step = maxStep
		}
		wait := base * time.Duration(step)
		logger.WithError(err).Warnf("temporal dial failed; retrying in %s", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

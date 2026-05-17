// skills-worker is the Temporal worker for the skills subsystem. It
// polls the "skills" task queue (see workflows.TaskQueue) and runs the
// R2R sync, GitHub importer, and check-source workflows in place of the
// fire-and-forget goroutines that previously lived in
// dashboard/apiserver/skills_r2r_sync.go.
//
// The workflow / activity / schedule registration logic lives in the
// importable skills/workerentry package so the umbrella's consolidated
// adamaton-worker binary can install the same set. This main.go owns
// only the per-process bootstrap (logger, pgxpool, Temporal client,
// signal handling, self-registration into evo.workers).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	"github.com/sirus20x6/adamaton-knowledge/skills/workerentry"
	skillsworkflows "github.com/sirus20x6/adamaton-knowledge/skills/workflows"
)

const (
	defaultDSN       = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	defaultTemporal  = "localhost:7233"
	defaultNamespace = "default"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	dsn := envOr("EVO_POSTGRES_DSN", envOr("POSTGRES_DSN", defaultDSN))
	temporalAddr := envOr("TEMPORAL_ADDRESS", defaultTemporal)
	namespace := envOr("TEMPORAL_NAMESPACE", defaultNamespace)
	taskQueue := envOr("SKILLS_TASK_QUEUE", skillsworkflows.TaskQueue)

	logger.WithFields(logrus.Fields{
		"temporal":  temporalAddr,
		"namespace": namespace,
		"queue":     taskQueue,
		"dsn":       redactDSN(dsn),
	}).Info("skills-worker starting")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("open evo postgres pool: %v", err)
	}
	defer pool.Close()

	c, err := dialTemporalWithRetry(ctx, temporalAddr, namespace, logger)
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	// HTTP client used by activities that hit the platform R2R surface
	// + GitHub. Tuned for the longest call we make (R2R ingest at 90s);
	// per-activity contexts may impose a tighter deadline. The custom
	// Transport caps the keep-alive pool — without it, the worker uses
	// http.DefaultTransport's MaxIdleConnsPerHost=2, which starves the
	// connection cache when activities fan out across api.github.com,
	// raw.githubusercontent.com, and the R2R host concurrently.
	httpClient := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	w := worker.New(c, taskQueue, worker.Options{
		Identity:                               "skills-worker",
		MaxConcurrentActivityExecutionSize:     8,
		MaxConcurrentWorkflowTaskExecutionSize: 8,
	})

	workerentry.Register(ctx, w, workerentry.Deps{
		Pool:       pool,
		HTTPClient: httpClient,
		Logger:     logger,
		Temporal:   c,
		TaskQueue:  taskQueue,
	})

	if sess, err := workerregistry.Register(ctx, pool, workerregistry.RegisterOptions{
		Identity:       "skills-worker",
		DeclaredQueues: []string{taskQueue},
		Logger:         logger,
	}); err != nil {
		logger.WithError(err).Warn("worker self-registration failed; worker continues without a workers row")
	} else {
		defer sess.Close(context.Background())
	}

	logger.Info("skills-worker ready")
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker.Run: %v", err)
	}
	logger.Info("skills-worker stopped")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// redactDSN strips the password segment from a Postgres DSN before
// logging it. Mirrors a tiny utility most workers reinvent inline.
func redactDSN(dsn string) string {
	at := -1
	colon := -1
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			at = i
			break
		}
		if dsn[i] == ':' && i > 11 { // skip "postgres://"
			colon = i
		}
	}
	if at < 0 || colon < 0 || colon >= at {
		return dsn
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}

// dialTemporalWithRetry mirrors evolve/cmd/evo-worker/main.go's helper.
// Linear backoff capped at 30s, honours ctx cancellation. On the Pi the
// systemd unit has Restart=on-failure so even a fatal exit recovers;
// retrying inside the process avoids restart-storm logspam during boot
// while the Temporal server is still warming up.
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
			Identity:  "skills-worker",
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

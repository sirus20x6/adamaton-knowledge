// Command skills-rae-worker is the Temporal worker for the SkillRAE
// offline indexer + community recomputation. It watches the
// "skills-rae" task queue and dispatches IndexSkillWorkflow,
// ReindexAllSkillsWorkflow, and RecomputeCommunitiesWorkflow plus
// their activities.
//
// Configuration:
//
//	SKILLS_RAE_DSN          — postgres DSN (shared evo DB)
//	BGE_SIDECAR_URL         — bge-m3 embeddings sidecar
//	CLUSTER_SIDECAR_URL     — UMAP+HDBSCAN sidecar (/v1/cluster + /v1/assign)
//	SKILLS_RAE_EMBED_MODEL  — model name (default "bge-m3")
//	TEMPORAL_ADDRESS        — temporal frontend (default localhost:7233)
//	TEMPORAL_NAMESPACE      — namespace (default default)
//	SKILLS_RAE_LOG_LEVEL    — logrus level (default info)
//
// Stateless; restart-safe. Activities use the dependency-injection
// pattern (suite structs with Pool + Embedder) so unit tests can
// stub them out.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/activities"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/clusterclient"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/embeddings"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/store"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/workflows"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "skills-rae-worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger()

	dsn := os.Getenv("SKILLS_RAE_DSN")
	if dsn == "" {
		return errors.New("SKILLS_RAE_DSN is required")
	}
	embedURL := os.Getenv("BGE_SIDECAR_URL")
	embedModel := envOr("SKILLS_RAE_EMBED_MODEL", "bge-m3")
	if embedURL == "" {
		return errors.New("BGE_SIDECAR_URL is required")
	}
	clusterURL := os.Getenv("CLUSTER_SIDECAR_URL")
	if clusterURL == "" {
		return errors.New("CLUSTER_SIDECAR_URL is required (UMAP+HDBSCAN sidecar)")
	}
	temporalAddr := envOr("TEMPORAL_ADDRESS", "localhost:7233")
	temporalNS := envOr("TEMPORAL_NAMESPACE", "default")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.DialPostgres(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres pool ready")

	embedder := embeddings.NewClient(embedURL, embedModel)
	logger.WithFields(logrus.Fields{"url": embedURL, "model": embedModel}).Info("embedder ready")

	cluster := clusterclient.NewClient(clusterURL)
	logger.WithField("url", clusterURL).Info("cluster-sidecar client ready")

	c, err := client.Dial(client.Options{HostPort: temporalAddr, Namespace: temporalNS})
	if err != nil {
		return fmt.Errorf("temporal dial: %w", err)
	}
	defer c.Close()
	logger.WithFields(logrus.Fields{"addr": temporalAddr, "ns": temporalNS}).Info("temporal client ready")

	w := worker.New(c, workflows.TaskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     8,
		MaxConcurrentWorkflowTaskExecutionSize: 8,
	})

	// Workflows.
	w.RegisterWorkflowWithOptions(workflows.IndexSkillWorkflow, workflow.RegisterOptions{Name: workflows.WorkflowIndexSkill})
	w.RegisterWorkflowWithOptions(workflows.ReindexAllSkillsWorkflow, workflow.RegisterOptions{Name: workflows.WorkflowReindexAllSkills})
	w.RegisterWorkflowWithOptions(workflows.ReindexAllSkillsWorkflowResume, workflow.RegisterOptions{Name: "ReindexAllSkillsWorkflowResume"})
	w.RegisterWorkflowWithOptions(workflows.RecomputeCommunitiesWorkflow, workflow.RegisterOptions{Name: workflows.WorkflowRecomputeCommunities})

	// Activity suites.
	extract := &activities.ExtractDeps{Pool: pool}
	w.RegisterActivityWithOptions(extract.ExtractSubunits, activity.RegisterOptions{Name: workflows.ActExtractSubunits})
	w.RegisterActivityWithOptions(extract.LoadSkillForEmbedding, activity.RegisterOptions{Name: workflows.ActLoadSkillForEmbedding})

	embed := &activities.EmbedDeps{Embedder: embedder, Pool: pool}
	w.RegisterActivityWithOptions(embed.EmbedBatch, activity.RegisterOptions{Name: workflows.ActEmbedBatch})

	graph := &activities.GraphDeps{Pool: pool}
	w.RegisterActivityWithOptions(graph.UpsertGraph, activity.RegisterOptions{Name: workflows.ActUpsertGraph})

	clusterDeps := &activities.ClusterDeps{Pool: pool, Embedder: embedder, ClusterClient: cluster}
	w.RegisterActivityWithOptions(clusterDeps.HDBSCANCluster, activity.RegisterOptions{Name: workflows.ActHDBSCANCluster})

	assignDeps := &activities.AssignDeps{Pool: pool, ClusterClient: cluster}
	w.RegisterActivityWithOptions(assignDeps.AssignToNearestCommunity, activity.RegisterOptions{Name: workflows.ActAssignToNearestCommunity})

	// Bare activity for ReindexAll's list step.
	w.RegisterActivityWithOptions(listAllSkillIDsActivity(pool), activity.RegisterOptions{Name: "ListAllSkillIDs"})

	go func() {
		<-ctx.Done()
		logger.Info("shutdown signal received; stopping worker")
		w.Stop()
	}()

	logger.WithField("queue", workflows.TaskQueue).Info("worker starting")
	if err := w.Run(worker.InterruptCh()); err != nil {
		return fmt.Errorf("worker run: %w", err)
	}
	logger.Info("worker stopped")
	return nil
}

func listAllSkillIDsActivity(pool *pgxpool.Pool) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		ids, err := store.ListAllSkillIDs(ctx, pool)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = id.String()
		}
		return out, nil
	}
}

// parseUUIDs is used inside workflows/reindex_all.go too; the
// duplicate lives here because string→uuid conversion must happen
// outside the workflow function (workflows are deterministic and
// uuid.Parse is fine, but keeping it in a helper file makes the
// workflow easier to read).
var _ = uuid.New // appease the unused-import linter

func newLogger() *logrus.Logger {
	lg := logrus.New()
	if lvl, err := logrus.ParseLevel(envOr("SKILLS_RAE_LOG_LEVEL", "info")); err == nil {
		lg.SetLevel(lvl)
	}
	return lg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

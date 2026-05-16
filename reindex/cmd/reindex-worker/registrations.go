package main

// Per-queue registrations. Split (2026-05-14) so the reindex-worker
// process can run an ingest queue and an extract queue with separate
// activity-execution pools. Each helper registers the workflows +
// activity methods that belong on its queue; the activity *structs*
// stay shared (one pool, one HTTP client) but only the relevant
// methods are surfaced per worker.

import (
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamomaton-knowledge/reindex/activities"
	reindexworkflows "github.com/sirus20x6/adamomaton-knowledge/reindex/workflows"
)

// registerIngestSide wires the corpus + paper workflows and the
// CPU/parser-bound activities onto the ingest worker.
func registerIngestSide(
	w worker.Worker,
	pool *pgxpool.Pool,
	httpClient *http.Client,
	platformURL string,
	r2gURL string,
	arxivSkipURL string,
	logger *logrus.Logger,
) {
	// Workflows -------------------------------------------------------
	w.RegisterWorkflowWithOptions(
		reindexworkflows.ReindexArxivCorpusWorkflow,
		workflow.RegisterOptions{Name: reindexworkflows.WorkflowReindexCorpus},
	)
	w.RegisterWorkflowWithOptions(
		reindexworkflows.ReindexArxivPaperWorkflow,
		workflow.RegisterOptions{Name: reindexworkflows.WorkflowReindexPaper},
	)

	// Activities ------------------------------------------------------
	queueActs, ingestActs, skippedActs := buildActivityStructs(
		pool, httpClient, platformURL, r2gURL, arxivSkipURL, logger,
	)

	w.RegisterActivityWithOptions(queueActs.QueryWorkSet,
		activity.RegisterOptions{Name: "QueryWorkSet"})
	w.RegisterActivityWithOptions(queueActs.RewireCorpusDocument,
		activity.RegisterOptions{Name: "RewireCorpusDocument"})
	w.RegisterActivityWithOptions(ingestActs.PreflightArxiv,
		activity.RegisterOptions{Name: "PreflightArxiv"})
	w.RegisterActivityWithOptions(ingestActs.IngestArxivPaper,
		activity.RegisterOptions{Name: "IngestArxivPaper"})
	w.RegisterActivityWithOptions(ingestActs.DeleteR2RDocument,
		activity.RegisterOptions{Name: "DeleteR2RDocument"})
	w.RegisterActivityWithOptions(skippedActs.MarkArxivSkipped,
		activity.RegisterOptions{Name: "MarkArxivSkipped"})

	logger.WithFields(logrus.Fields{
		"queue":      reindexworkflows.TaskQueueIngest,
		"platform":   platformURL,
		"arxiv_skip": arxivSkipURL,
	}).Info("registered ingest-side workflows + activities")
}

// registerExtractSide wires the long-running ExtractCorpusWorkflow
// daemon and the LLM-bound ExtractEntities activity onto the extract
// worker. QueryReadyForExtraction also lives here because it's only
// called from ExtractCorpusWorkflow.
func registerExtractSide(
	w worker.Worker,
	pool *pgxpool.Pool,
	httpClient *http.Client,
	platformURL string,
	r2gURL string,
	arxivSkipURL string,
	logger *logrus.Logger,
) {
	// Workflows -------------------------------------------------------
	w.RegisterWorkflowWithOptions(
		reindexworkflows.ExtractCorpusWorkflow,
		workflow.RegisterOptions{Name: reindexworkflows.WorkflowExtractCorpus},
	)

	// Activities ------------------------------------------------------
	queueActs, ingestActs, _ := buildActivityStructs(
		pool, httpClient, platformURL, r2gURL, arxivSkipURL, logger,
	)

	w.RegisterActivityWithOptions(queueActs.QueryReadyForExtraction,
		activity.RegisterOptions{Name: "QueryReadyForExtraction"})
	w.RegisterActivityWithOptions(ingestActs.ExtractEntities,
		activity.RegisterOptions{Name: "ExtractEntities"})

	logger.WithFields(logrus.Fields{
		"queue": reindexworkflows.TaskQueueExtract,
		"r2g":   r2gURL,
	}).Info("registered extract-side workflow + activities")
}

// buildActivityStructs constructs the activity DI structs. Called once
// per worker so they're isolated, but the underlying pool + HTTP client
// are shared. The arxiv-skip client is nil-safe (NewArxivSkipClient
// handles the empty URL).
func buildActivityStructs(
	pool *pgxpool.Pool,
	httpClient *http.Client,
	platformURL string,
	r2gURL string,
	arxivSkipURL string,
	logger *logrus.Logger,
) (*activities.QueueActivities, *activities.IngestActivities, *activities.SkippedActivities) {
	client := httpClient
	if envBool("REINDEX_TLS_INSECURE") {
		client = activities.NewHTTPClient(true)
	}
	skipClient := activities.NewArxivSkipClient(arxivSkipURL)
	return &activities.QueueActivities{
			Pool:   pool,
			Logger: logger,
		}, &activities.IngestActivities{
			HTTPClient:  client,
			PlatformURL: platformURL,
			R2GURL:      r2gURL,
			Logger:      logger,
			SkipClient:  skipClient,
		}, &activities.SkippedActivities{
			Pool:   pool,
			Logger: logger,
		}
}

// envBool mirrors the parsing used elsewhere — 1/true/yes flips the
// bit, everything else is false.
func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

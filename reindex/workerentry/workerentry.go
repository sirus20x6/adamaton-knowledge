// Package workerentry wires the reindex subsystem onto Temporal
// workers. The original reindex-worker process hosts two Temporal
// workers in one binary (ingest + extract queues, with separate
// activity-execution pools so an extract retry-storm can't starve
// ingest). RegisterIngest and RegisterExtract are split so the
// unified adamaton-worker can run each queue in its own OS process
// for crash isolation, while the standalone reindex-worker continues
// to host both in one process.
//
// Activity structs are constructed lazily per-queue because each
// worker owns its own pool of in-flight activities; sharing the
// pgxpool and HTTP client is fine.
package workerentry

import (
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/reindex/activities"
	reindexworkflows "github.com/sirus20x6/adamaton-knowledge/reindex/workflows"
)

// Deps groups every dependency the reindex registration helpers need.
// PlatformURL is the host for the /platform/sources/ingest endpoint;
// R2GURL is the r2g sidecar; ArxivSkipURL enables the arxiv-skip
// preflight (empty string disables it, fail-open).
type Deps struct {
	Pool         *pgxpool.Pool
	HTTPClient   *http.Client
	Logger       *logrus.Logger
	PlatformURL  string
	R2GURL       string
	ArxivSkipURL string
	// EmbedURL is the OpenAI-compatible embedding sidecar the worker-side
	// ChunkEmbedPersist activity calls. Empty disables ChunkEmbedPersist
	// (it returns a non-retryable NoEmbedder error). EmbedModel defaults
	// to "bge-m3" when empty.
	EmbedURL   string
	EmbedModel string
}

// RegisterIngest wires the corpus + paper workflows and the
// CPU/parser-bound activities onto the ingest worker. Mirrors the
// behaviour of the old registerIngestSide in
// reindex/cmd/reindex-worker/registrations.go.
func RegisterIngest(w worker.Worker, d Deps) {
	w.RegisterWorkflowWithOptions(
		reindexworkflows.ReindexArxivCorpusWorkflow,
		workflow.RegisterOptions{Name: reindexworkflows.WorkflowReindexCorpus},
	)
	w.RegisterWorkflowWithOptions(
		reindexworkflows.ReindexArxivPaperWorkflow,
		workflow.RegisterOptions{Name: reindexworkflows.WorkflowReindexPaper},
	)

	queueActs, ingestActs, skippedActs := buildActivityStructs(d)

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

	// Split-ingest activities: PrepareArxivDocument (fetch+parse via r2g),
	// ChunkEmbedPersist (chunk+embed+persist on THIS node), and
	// ExtractArxivFigures (figure extract via r2g). The legacy
	// IngestArxivPaper above stays registered for one release so a
	// rollback to the old single-call workflow still has a worker.
	embedActs := buildEmbedDeps(d)
	w.RegisterActivityWithOptions(ingestActs.PrepareArxivDocument,
		activity.RegisterOptions{Name: "PrepareArxivDocument"})
	w.RegisterActivityWithOptions(embedActs.ChunkEmbedPersist,
		activity.RegisterOptions{Name: "ChunkEmbedPersist"})
	w.RegisterActivityWithOptions(ingestActs.ExtractArxivFigures,
		activity.RegisterOptions{Name: "ExtractArxivFigures"})

	d.Logger.WithFields(logrus.Fields{
		"queue":      reindexworkflows.TaskQueueIngest,
		"platform":   d.PlatformURL,
		"arxiv_skip": d.ArxivSkipURL,
		"embed_url":  d.EmbedURL,
	}).Info("registered ingest-side workflows + activities")
}

// buildEmbedDeps constructs the worker-side embedding activity deps. The
// embeddings client is nil when no sidecar URL is configured (ChunkEmbedPersist
// then fails fast and non-retryably). The chunker is a process-wide tiktoken
// singleton; an init failure is logged but left non-fatal so the rest of the
// ingest queue still registers.
func buildEmbedDeps(d Deps) *activities.EmbedDeps {
	var embedder *embeddings.Client
	if d.EmbedURL != "" {
		model := d.EmbedModel
		if model == "" {
			model = "bge-m3"
		}
		embedder = embeddings.NewClient(d.EmbedURL, model)
	} else {
		d.Logger.Warn("no embed sidecar URL configured; ChunkEmbedPersist will fail until BGE_EMBED_URL is set")
	}
	chunker, err := chunking.Default()
	if err != nil {
		d.Logger.WithError(err).Error("chunker init failed; ChunkEmbedPersist will fail")
	}
	return &activities.EmbedDeps{
		Embedder: embedder,
		Chunker:  chunker,
		Pool:     d.Pool,
		Logger:   d.Logger,
	}
}

// RegisterExtract wires the long-running ExtractCorpusWorkflow daemon
// and the LLM-bound ExtractEntities activity onto the extract worker.
// QueryReadyForExtraction also lives here because it's only called
// from ExtractCorpusWorkflow.
func RegisterExtract(w worker.Worker, d Deps) {
	w.RegisterWorkflowWithOptions(
		reindexworkflows.ExtractCorpusWorkflow,
		workflow.RegisterOptions{Name: reindexworkflows.WorkflowExtractCorpus},
	)

	queueActs, ingestActs, _ := buildActivityStructs(d)

	w.RegisterActivityWithOptions(queueActs.QueryReadyForExtraction,
		activity.RegisterOptions{Name: "QueryReadyForExtraction"})
	w.RegisterActivityWithOptions(ingestActs.ExtractEntities,
		activity.RegisterOptions{Name: "ExtractEntities"})

	d.Logger.WithFields(logrus.Fields{
		"queue": reindexworkflows.TaskQueueExtract,
		"r2g":   d.R2GURL,
	}).Info("registered extract-side workflow + activities")
}

// buildActivityStructs constructs the activity DI structs. The
// underlying pool + HTTP client are shared across them, but each
// worker gets its own struct instances so internal state stays
// isolated. The arxiv-skip client is nil-safe (NewArxivSkipClient
// handles the empty URL).
func buildActivityStructs(d Deps) (*activities.QueueActivities, *activities.IngestActivities, *activities.SkippedActivities) {
	client := d.HTTPClient
	if envBool("REINDEX_TLS_INSECURE") {
		client = activities.NewHTTPClient(true)
	}
	skipClient := activities.NewArxivSkipClient(d.ArxivSkipURL)
	return &activities.QueueActivities{
			Pool:   d.Pool,
			Logger: d.Logger,
		}, &activities.IngestActivities{
			HTTPClient:  client,
			PlatformURL: d.PlatformURL,
			R2GURL:      d.R2GURL,
			Logger:      d.Logger,
			SkipClient:  skipClient,
		}, &activities.SkippedActivities{
			Pool:   d.Pool,
			Logger: d.Logger,
		}
}

// envBool mirrors the 1/true/yes parsing used elsewhere across the
// umbrella. Kept private here so callers don't accidentally couple to
// it; the helper is too small to live in core.
func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

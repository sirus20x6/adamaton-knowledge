package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ExtractCorpusWorkflow drains the corpus of docs that need entity +
// relationship extraction. Decouples graph extraction from the reindex
// pipeline so per-paper reindex throughput stays unaffected, and so
// the existing backlog of eprint docs (whose extract was never fired)
// gets processed by the same code path as fresh ones.
//
// Algorithm:
//
//  1. Query for up to BatchSize doc IDs where ingest is fully done
//     (ingestion_status='success') AND graph extraction hasn't landed
//     (extraction_status IN ('pending','failed')). Filter is type='eprint'
//     so we don't waste work on legacy md docs that are about to be
//     replaced.
//  2. Fan out ExtractEntities activities, bounded by Concurrency. Each
//     activity is one POST to /v3/documents/{id}/extract; R2R queues
//     the work internally so the HTTP call returns quickly.
//  3. When the batch settles, sleep SleepBetweenSeconds if it was
//     short of BatchSize (queue drained — wait for more), else loop
//     immediately.
//  4. ContinueAsNew every N triggered docs to keep history bounded.
//
// Unlike ReindexArxivCorpusWorkflow, this is a *daemon* — it never
// terminates voluntarily. Stop it by terminating the workflow id.
// One instance per corpus.
func ExtractCorpusWorkflow(ctx workflow.Context, in ExtractCorpusInput) (*ExtractProgress, error) {
	logger := workflow.GetLogger(ctx)

	if in.CorpusID == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"extract: corpus_id required", "BadInput", nil,
		)
	}

	concurrency := in.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	if concurrency > 8 {
		concurrency = 8
	}
	batchSize := in.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	sleepSecs := in.SleepBetweenSeconds
	if sleepSecs <= 0 {
		sleepSecs = 60
	}
	caN := in.ContinueAsNewEvery
	if caN <= 0 {
		caN = 500
	}

	progress := &ExtractProgress{
		StartedAt: workflow.Now(ctx),
	}
	if in.Carry != nil {
		progress.Triggered = in.Carry.Triggered
		progress.Succeeded = in.Carry.Succeeded
		progress.Failed = in.Carry.Failed
		if !in.Carry.StartedAt.IsZero() {
			progress.StartedAt = in.Carry.StartedAt
		}
	}
	if err := workflow.SetQueryHandler(ctx, QueryExtractProgress, func() (*ExtractProgress, error) {
		return progress, nil
	}); err != nil {
		return progress, err
	}

	logger.Info("ExtractCorpusWorkflow start",
		"corpus", in.CorpusID,
		"concurrency", concurrency,
		"batch_size", batchSize,
		"sleep_between_seconds", sleepSecs,
		"has_carry", in.Carry != nil,
	)

	processedThisRun := 0
	for {
		// Step 1: pull a batch of ready doc IDs.
		queryCtx := workflow.WithActivityOptions(ctx, queryReadyOptions())
		var docs []string
		if err := workflow.ExecuteActivity(queryCtx, "QueryReadyForExtraction", queryReadyInput{
			CorpusID: in.CorpusID,
			Limit:    batchSize,
		}).Get(queryCtx, &docs); err != nil {
			logger.Error("QueryReadyForExtraction failed", "error", err)
			return progress, err
		}
		progress.LastSweep = workflow.Now(ctx)

		if len(docs) == 0 {
			logger.Info("ExtractCorpusWorkflow: queue empty, sleeping",
				"sleep_seconds", sleepSecs,
				"triggered_cumulative", progress.Triggered,
			)
			if err := workflow.Sleep(ctx, time.Duration(sleepSecs)*time.Second); err != nil {
				return progress, err
			}
			continue
		}

		// Step 2: fan out ExtractEntities with bounded concurrency.
		inFlight := map[string]workflow.Future{}
		i := 0
		for i < len(docs) || len(inFlight) > 0 {
			for i < len(docs) && len(inFlight) < concurrency {
				doc := docs[i]
				extractCtx := workflow.WithActivityOptions(ctx, extractActivityOptions())
				inFlight[doc] = workflow.ExecuteActivity(extractCtx, "ExtractEntities", doc)
				i++
			}
			if len(inFlight) == 0 {
				break
			}
			sel := workflow.NewSelector(ctx)
			var settled string
			var settledErr error
			for d, f := range inFlight {
				d := d
				f := f
				sel.AddFuture(f, func(fut workflow.Future) {
					settled = d
					if err := fut.Get(ctx, nil); err != nil {
						settledErr = err
					}
				})
			}
			sel.Select(ctx)
			delete(inFlight, settled)
			progress.Triggered++
			processedThisRun++
			if settledErr != nil {
				progress.Failed++
				logger.Warn("ExtractEntities settled with error",
					"doc", settled,
					"error", settledErr,
				)
			} else {
				progress.Succeeded++
			}
		}

		// Step 3: if the batch was short of BatchSize we drained the
		// queue — sleep before retrying. Otherwise loop immediately,
		// there's more to do.
		if len(docs) < batchSize {
			logger.Info("ExtractCorpusWorkflow: batch shorter than BatchSize, sleeping",
				"batch_size", batchSize,
				"got", len(docs),
				"sleep_seconds", sleepSecs,
			)
			if err := workflow.Sleep(ctx, time.Duration(sleepSecs)*time.Second); err != nil {
				return progress, err
			}
		}

		// Step 4: ContinueAsNew check.
		if processedThisRun >= caN {
			logger.Info("ContinueAsNew threshold reached",
				"processed_this_run", processedThisRun,
				"caN", caN,
			)
			carry := &ExtractProgress{
				Triggered: progress.Triggered,
				Succeeded: progress.Succeeded,
				Failed:    progress.Failed,
				StartedAt: progress.StartedAt,
				LastSweep: progress.LastSweep,
			}
			nextInput := ExtractCorpusInput{
				CorpusID:            in.CorpusID,
				Concurrency:         in.Concurrency,
				BatchSize:           in.BatchSize,
				SleepBetweenSeconds: in.SleepBetweenSeconds,
				ContinueAsNewEvery:  caN,
				Carry:               carry,
			}
			return progress, workflow.NewContinueAsNewError(ctx, WorkflowExtractCorpus, nextInput)
		}
	}
}

// queryReadyInput mirrors activities.QueryReadyInput. Duplicated here
// to avoid a workflows → activities import (same cycle-dodge as the
// other input mirrors in this package).
type queryReadyInput struct {
	CorpusID string `json:"corpus_id"`
	Limit    int    `json:"limit"`
}

// queryReadyOptions tunes the QueryReadyForExtraction activity. The
// query is bounded by LIMIT 50 and the index on (status, created_at)
// is cheap; 30s is plenty.
func queryReadyOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
}

// extractActivityOptions tunes ExtractEntities used by the sweep.
// R2R returns from /extract within ~1s (it queues the work internally
// even under orchestration.provider="simple"), so 60s StartToClose is
// generous; the HTTP client's 20-minute ceiling is the upper bound if
// the backend genuinely stalls. Retries cover transient 5xx from a
// backend deploy window.
func extractActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 60 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    60 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

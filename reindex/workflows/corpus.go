package workflows

import (
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ReindexArxivCorpusWorkflow is the parent workflow for the arxiv
// reindex job. Mirrors the outer `while read row` loop on lines 64-173
// of dashboard/reindex-arxiv-latex/reindex-arxiv-latex.sh, but with
// bounded concurrent fan-out instead of strictly-serial execution.
//
// Algorithm:
//
//  1. Cap Concurrency to [1, 8] (Pi resource ceiling).
//  2. Hydrate progress counters from in.Carry if a previous run
//     ContinueAsNew'd into us — cumulative Done/Skipped/Failed across
//     the full corpus pass survives the continuation boundary.
//  3. Run QueryWorkSet activity to materialize the queue. The query
//     filters out papers recorded in evo.reindex_skipped within the
//     last 30 days, so each continuation sees only fresh work.
//  4. Fan out ReindexArxivPaperWorkflow children with a counting
//     semaphore: never more than Concurrency outstanding at once.
//     Children block via workflow.NewSelector.AddFuture so the parent
//     keeps the workflow task thread responsive (and the QueryProgress
//     handler keeps returning up-to-the-tick state).
//  5. Each child completion updates progress in place AND bumps a
//     this-run-only counter. When that counter hits ContinueAsNewEvery,
//     we drain in-flight children and ContinueAsNew with the same
//     input plus a Carry struct holding cumulative counters.
//
// Continuation contract (operator-facing):
//
//   - External callers MUST NOT set Carry. It's the workflow's
//     bookkeeping; setting it from outside corrupts the cumulative
//     Done/Skipped/Failed displayed by the dashboard.
//   - ContinueAsNewEvery defaults to 500. Set higher to disable for
//     small one-off runs; set lower to test the continuation path.
//   - Each continuation re-runs QueryWorkSet, so Skipped papers
//     persisted into evo.reindex_skipped are filtered out and we
//     converge instead of looping. Resetting evo.reindex_skipped (or
//     the 30-day cutoff in queue.go) forces a re-attempt sweep.
//   - The Temporal workflow id stays stable across continuations —
//     the dashboard's progress query keeps working without reconnect.
//
// Workflow progress is exposed via the QueryProgress handler so the
// dashboard can poll it without blocking the workflow.
func ReindexArxivCorpusWorkflow(ctx workflow.Context, in ReindexArxivCorpusInput) (*CorpusProgress, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("ReindexArxivCorpusWorkflow start",
		"concurrency", in.Concurrency,
		"limit", in.Limit,
		"dry_run", in.DryRun,
		"continue_as_new_every", in.ContinueAsNewEvery,
		"has_carry", in.Carry != nil,
	)

	// Bound concurrency. The bash version was strictly serial (1); the
	// platform backend is now async-throughput-bound rather than
	// memory-spike-bound (pandoc runs on a threadpool via asyncio.to_thread
	// in arxiv_fetcher.py, so multiple concurrent ingests truly parallelize
	// through the single uvicorn process). Default 8, cap 8 — the Pi's
	// 4-core CPU has been our ceiling, not RAM.
	concurrency := in.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	if concurrency > 8 {
		concurrency = 8
	}

	// ContinueAsNewEvery defaults to 500 papers per run. At ~5 history
	// events per paper (start + 2-4 activity events + child complete)
	// that keeps a single run well under the 10k soft ceiling with
	// margin for the parent's own bookkeeping events.
	caN := in.ContinueAsNewEvery
	if caN <= 0 {
		caN = 500
	}

	progress := &CorpusProgress{
		StartedAt: workflow.Now(ctx),
		InFlight:  []string{},
	}
	// Hydrate cumulative counters from prior run. StartedAt only
	// carries forward if it's non-zero — protects against an
	// externally-supplied Carry with a zero time clobbering "now".
	if in.Carry != nil {
		progress.Done = in.Carry.Done
		progress.Skipped = in.Carry.Skipped
		progress.Failed = in.Carry.Failed
		if !in.Carry.StartedAt.IsZero() {
			progress.StartedAt = in.Carry.StartedAt
		}
	}
	if err := workflow.SetQueryHandler(ctx, QueryProgress, func() (*CorpusProgress, error) {
		return progress, nil
	}); err != nil {
		logger.Error("SetQueryHandler failed", "error", err)
		return progress, err
	}

	// Step 1: produce the work set. The query is cheap enough to keep
	// inside one activity attempt budget; the retry policy here covers
	// transient pgxpool disconnects, not a bad SQL filter.
	queryCtx := workflow.WithActivityOptions(ctx, queryWorkSetOptions())
	var papers []ArxivPaper
	if err := workflow.ExecuteActivity(queryCtx, "QueryWorkSet", in.Limit).Get(queryCtx, &papers); err != nil {
		logger.Error("QueryWorkSet failed", "error", err)
		return progress, err
	}

	// Compute Total. With Carry: inherited (already-counted from
	// previous runs) + len(papers) (still-to-do in this run) is the
	// new working estimate. If a previous run's Total was higher
	// (e.g. some papers got freshly-skipped between continuations and
	// dropped out of this run's query), preserve it so the dashboard
	// doesn't show progress regressing.
	inherited := 0
	inheritedTotal := 0
	if in.Carry != nil {
		inherited = in.Carry.Done + in.Carry.Skipped + in.Carry.Failed
		inheritedTotal = in.Carry.Total
	}
	currentTotal := inherited + len(papers)
	if inheritedTotal > currentTotal {
		progress.Total = inheritedTotal
	} else {
		progress.Total = currentTotal
	}
	logger.Info("queue produced",
		"papers_this_run", len(papers),
		"inherited", inherited,
		"total", progress.Total,
	)

	if len(papers) == 0 {
		logger.Info("ReindexArxivCorpusWorkflow done (no work)",
			"total", progress.Total,
			"done", progress.Done,
			"skipped", progress.Skipped,
			"failed", progress.Failed,
		)
		return progress, nil
	}

	// Step 2: fan-out with bounded parallelism. inFlight tracks which
	// arxiv IDs are currently owned by a child workflow, mirroring the
	// CorpusProgress.InFlight slice the QueryProgress handler returns.
	// Workflow code is single-threaded so no locking is needed.
	inFlight := map[string]pendingChild{}

	// processedThisRun counts children settled in THIS workflow run,
	// not cumulative across continuations. When it hits caN we
	// ContinueAsNew so a single run stays bounded.
	processedThisRun := 0

	// drainOne blocks the workflow on a Selector until at least one
	// child future fires, then folds its result into progress and
	// removes it from inFlight. Returns false only when there is
	// nothing left to wait for (shouldn't happen given the call sites,
	// but defensive).
	drainOne := func() bool {
		if len(inFlight) == 0 {
			return false
		}
		sel := workflow.NewSelector(ctx)
		var settled string
		var settledErr error
		var settledResult PaperResult
		for axid, p := range inFlight {
			axid := axid
			p := p
			sel.AddFuture(p.future, func(f workflow.Future) {
				settled = axid
				if err := f.Get(ctx, &settledResult); err != nil {
					settledErr = err
				}
			})
		}
		sel.Select(ctx)

		// Fold result.
		entry := inFlight[settled]
		delete(inFlight, settled)
		progress.InFlight = inFlightKeys(inFlight)
		processedThisRun++

		switch {
		case settledErr != nil:
			progress.Failed++
			logger.Warn("child workflow failed",
				"arxiv", entry.paper.ArxivID,
				"error", settledErr,
			)
		case settledResult.Skipped:
			progress.Skipped++
			logger.Info("paper skipped",
				"arxiv", entry.paper.ArxivID,
				"reason", settledResult.SkipReason,
			)
		default:
			progress.Done++
		}
		return true
	}

	// Index-based loop so we can check "are there more papers after
	// this one?" — required for the ContinueAsNew check below.
	for i := 0; i < len(papers); i++ {
		paper := papers[i]

		// Block until a child slot opens.
		for len(inFlight) >= concurrency {
			if !drainOne() {
				break
			}
		}

		// ContinueAsNew check: if we've already settled enough papers
		// in this run AND there's strictly more work to dispatch after
		// this one (i+1 < len), drain everything in-flight (so their
		// results land in this run's history) and ContinueAsNew with
		// cumulative counters. Skipping the continuation on the very
		// last paper avoids a pointless continuation that would do
		// zero work — the post-loop drain handles termination cleanly.
		// The next run's QueryWorkSet excludes everything in
		// evo.reindex_skipped, so we converge instead of looping.
		if processedThisRun >= caN && i+1 < len(papers) {
			logger.Info("ContinueAsNew threshold reached; draining and continuing",
				"processed_this_run", processedThisRun,
				"remaining_in_run", len(papers)-i,
				"caN", caN,
			)
			for len(inFlight) > 0 {
				if !drainOne() {
					break
				}
			}
			carry := &CorpusProgress{
				Total:     progress.Total,
				Done:      progress.Done,
				Skipped:   progress.Skipped,
				Failed:    progress.Failed,
				StartedAt: progress.StartedAt,
			}
			nextInput := ReindexArxivCorpusInput{
				Concurrency:        in.Concurrency,
				DryRun:             in.DryRun,
				Limit:              in.Limit,
				ContinueAsNewEvery: caN,
				Carry:              carry,
			}
			return progress, workflow.NewContinueAsNewError(ctx, WorkflowReindexCorpus, nextInput)
		}

		childOpts := workflow.ChildWorkflowOptions{
			WorkflowID: "reindex-paper-" + paper.ArxivID,
			TaskQueue:  TaskQueueIngest,
			// ALLOW_DUPLICATE so a paper that's already been processed
			// (succeeded or skipped) in a previous corpus run CAN be
			// retried in a later run. ALLOW_DUPLICATE_FAILED_ONLY was
			// wrong here: a paper with no LaTeX source completes-as-skipped
			// (status=Completed in Temporal), so subsequent corpus runs
			// were rejected with "child workflow execution already started"
			// even though the work-set query (which only looks at
			// source_tier != 'latex' OR summary IS NULL) considers them
			// still eligible. ALLOW_DUPLICATE still rejects currently-running
			// duplicates, so it doesn't break concurrent-corpus-run safety.
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			// No RetryPolicy on the child: per-activity retry inside the
			// paper workflow already covers transient errors. A child
			// retry would re-run the whole workflow including its
			// already-completed activities, which we don't want.
		}
		childCtx := workflow.WithChildOptions(ctx, childOpts)
		childInput := ReindexArxivPaperInput{
			ArxivID:  paper.ArxivID,
			OldDocID: paper.OldDocID,
			Title:    paper.Title,
			DryRun:   in.DryRun,
		}
		fut := workflow.ExecuteChildWorkflow(childCtx, WorkflowReindexPaper, childInput)
		inFlight[paper.ArxivID] = pendingChild{paper: paper, future: fut}
		progress.InFlight = inFlightKeys(inFlight)
	}

	// Drain remaining children.
	for len(inFlight) > 0 {
		if !drainOne() {
			break
		}
	}

	logger.Info("ReindexArxivCorpusWorkflow done",
		"total", progress.Total,
		"done", progress.Done,
		"skipped", progress.Skipped,
		"failed", progress.Failed,
	)
	return progress, nil
}

// pendingChild bundles the child workflow's future with the paper
// row it was launched for, so the Selector callback can attribute the
// settled result back to the right arxiv id without re-querying the
// map (Selector closures only carry the loop variable, not the value).
type pendingChild struct {
	paper  ArxivPaper
	future workflow.ChildWorkflowFuture
}

func inFlightKeys(m map[string]pendingChild) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// queryWorkSetOptions tunes the QueryWorkSet activity. The query reads
// ~10k rows; 30s is plenty. Retries cover pgxpool reconnect races.
func queryWorkSetOptions() workflow.ActivityOptions {
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

package workflows

import (
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// childAlreadyRunningReason is the marker we slot into BatchCheckResult.Failed
// when a child start collided with an in-flight one-off check for the same
// skill_id. See CheckAllSkillSourcesWorkflow.
const childAlreadyRunningReason = "child workflow already running"

// childState is the per-child bookkeeping shared between the
// start-future and result-future selector callbacks. Workflow code is
// single-threaded, so a plain pointer-to-struct is safe to mutate from
// either callback.
type childState struct {
	startFailed bool
}

// Activity names registered by the skills-worker. Workflows reference
// activities by string (same pattern as skills/workflows/sync.go) so
// this package doesn't have to import skills/activities.
const (
	activityLoadSkillSourceMeta     = "LoadSkillSourceMeta"
	activityRefetchSource           = "RefetchSource"
	activityMarkSourceChecked       = "MarkSourceChecked"
	activityFlagDrift               = "FlagDrift"
	activityQuerySkillsWithSources  = "QuerySkillsWithSourceURLs"
)

// batchParallelism caps how many child CheckSkillSourceWorkflow runs
// the batch workflow allows in flight simultaneously. Keeps GitHub's
// 5,000 req/hr authenticated rate limit happy even with a few thousand
// skills, and bounds Postgres write concurrency from the activities.
const batchParallelism = 5

// checkSourceActivityOptions tunes the per-activity timeouts for the
// drift detector. 60s start-to-close is plenty for a single Markdown
// blob fetch + a small Postgres write; the 3-attempt cap with a 5s/2x/30s
// backoff means a flapping GitHub will retry twice (5s, 10s) before
// giving up — we don't want to hammer the upstream on a transient blip.
func checkSourceActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 60 * time.Second,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// CheckSkillSourceWorkflow re-fetches one skill's upstream source and
// flips drift_detected when the new content's sha256 differs from the
// stored source_sha.
//
// Replaces the synchronous POST /api/v1/skills/{id}/check-source path
// at dashboard/apiserver/skills_import_handlers.go:324-384. By running
// through Temporal the call survives dashboard restarts, GitHub
// hiccups, and worker-host crashes — retry policy handles transient
// failures up to MaxAttempts=3.
//
// Activity sequence:
//
//  1. LoadSkillSourceMeta — pulls source_url + source_sha out of evo.skills.
//  2. (early exit) if source_url is empty, returns Skipped with reason.
//  3. RefetchSource — downloads the bytes, returns body + sha256.
//  4. MarkSourceChecked — stamps source_checked_at = NOW() unconditionally.
//  5. (conditional) FlagDrift — flips drift_detected = TRUE when SHAs differ.
//
// Result.NewBody is only populated when Changed == true; saves history
// bytes for the common no-drift case.
func CheckSkillSourceWorkflow(ctx workflow.Context, in CheckSkillSourceInput) (*CheckSkillSourceResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("CheckSkillSourceWorkflow start", "skill_id", in.SkillID)

	ctx = workflow.WithActivityOptions(ctx, checkSourceActivityOptions())

	var meta SourceMetaResult
	if err := workflow.ExecuteActivity(ctx, activityLoadSkillSourceMeta, in.SkillID).Get(ctx, &meta); err != nil {
		logger.Warn("CheckSkillSourceWorkflow load failed", "skill_id", in.SkillID, "error", err)
		return nil, err
	}

	if meta.SourceURL == "" {
		logger.Info("CheckSkillSourceWorkflow skipped (no source_url)", "skill_id", in.SkillID)
		return &CheckSkillSourceResult{
			SkillID: in.SkillID,
			Changed: false,
			OldSHA:  meta.SourceSHA,
			Reason:  "no source_url",
		}, nil
	}

	var refetch RefetchActivityResult
	if err := workflow.ExecuteActivity(ctx, activityRefetchSource, meta.SourceURL).Get(ctx, &refetch); err != nil {
		logger.Warn("CheckSkillSourceWorkflow refetch failed", "skill_id", in.SkillID, "error", err)
		return nil, err
	}

	// Stamp checked_at regardless of outcome — the column proves we
	// looked, even when the body matched. Mirrors the synchronous
	// handler's behaviour.
	if err := workflow.ExecuteActivity(ctx, activityMarkSourceChecked, in.SkillID).Get(ctx, nil); err != nil {
		logger.Warn("CheckSkillSourceWorkflow stamp failed", "skill_id", in.SkillID, "error", err)
		return nil, err
	}

	res := &CheckSkillSourceResult{
		SkillID: in.SkillID,
		OldSHA:  meta.SourceSHA,
		NewSHA:  refetch.NewSHA,
	}
	if refetch.NewSHA == meta.SourceSHA {
		res.Changed = false
		res.Reason = "source sha matches existing skill"
		logger.Info("CheckSkillSourceWorkflow done (unchanged)", "skill_id", in.SkillID)
		return res, nil
	}

	if err := workflow.ExecuteActivity(ctx, activityFlagDrift, in.SkillID).Get(ctx, nil); err != nil {
		logger.Warn("CheckSkillSourceWorkflow flag-drift failed", "skill_id", in.SkillID, "error", err)
		return nil, err
	}
	res.Changed = true
	res.NewBody = refetch.Body
	res.Reason = "source content differs from stored sha"
	logger.Info("CheckSkillSourceWorkflow done (drift)", "skill_id", in.SkillID, "new_sha", refetch.NewSHA)
	return res, nil
}

// CheckAllSkillSourcesWorkflow fans out CheckSkillSourceWorkflow over
// every skill with a non-null source_url. Owned by the daily Temporal
// Schedule (ScheduleIDCheckAllDaily); also runnable on demand for
// debugging.
//
// Concurrency: a counting-semaphore over child workflows. We start up
// to batchParallelism children, then for each additional skill we use
// workflow.NewSelector to wait until one in-flight child completes
// before starting the next. Selector is the deterministic way to await
// "any of N futures" inside a workflow — using a plain Go channel or
// goroutine would violate Temporal's determinism contract.
//
// Child workflow IDs are deterministic ("skill-check-<skill_id>") so an
// in-flight one-off check from the dashboard doesn't conflict. We set
// WorkflowIDReusePolicy=ALLOW_DUPLICATE so any *closed* prior run lets a
// fresh attempt start regardless of its terminal status — the previous
// ALLOW_DUPLICATE_FAILED_ONLY surfaced WorkflowExecutionAlreadyStarted
// when a one-off check from the dashboard was still running, which the
// batch incorrectly counted as a hard failure.
//
// For the *running-conflict* case (a one-off is genuinely in-flight),
// the Go SDK v1.43 ChildWorkflowOptions doesn't expose the newer
// WorkflowIDConflictPolicy=USE_EXISTING knob that would let us attach to
// the running execution. We instead detect the start-time error via
// temporal.IsWorkflowExecutionAlreadyStartedError on the
// GetChildWorkflowExecution future, skip the child, and record a
// purpose-specific Failed entry so the daily totals reflect reality
// without producing a noisy error trace.
func CheckAllSkillSourcesWorkflow(ctx workflow.Context) (*BatchCheckResult, error) {
	logger := workflow.GetLogger(ctx)
	startedAt := workflow.Now(ctx)
	logger.Info("CheckAllSkillSourcesWorkflow start", "started_at", startedAt)

	ctx = workflow.WithActivityOptions(ctx, checkSourceActivityOptions())

	var skillIDs []string
	if err := workflow.ExecuteActivity(ctx, activityQuerySkillsWithSources).Get(ctx, &skillIDs); err != nil {
		logger.Warn("CheckAllSkillSourcesWorkflow query failed", "error", err)
		return nil, err
	}
	logger.Info("CheckAllSkillSourcesWorkflow candidates", "n", len(skillIDs))

	res := &BatchCheckResult{
		StartedAt: startedAt,
		Drifted:   []string{},
		Failed:    []string{},
	}

	if len(skillIDs) == 0 {
		res.FinishedAt = workflow.Now(ctx)
		return res, nil
	}

	// Counter-based concurrency control. We never key any map by
	// workflow.Future — the SDK passes a Future interface to the
	// selector callback that compares unequal to the ChildWorkflowFuture
	// we stored at start time. Instead, each callback captures its
	// skillID by closure, and a simple counter tracks in-flight children
	// for the parallelism gate.
	selector := workflow.NewSelector(ctx)
	inflight := 0

	startChild := func(skillID string) {
		childOpts := workflow.ChildWorkflowOptions{
			WorkflowID:            "skill-check-" + skillID,
			TaskQueue:             TaskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		}
		childCtx := workflow.WithChildOptions(ctx, childOpts)
		fut := workflow.ExecuteChildWorkflow(childCtx, WorkflowCheckSkillSource, CheckSkillSourceInput{SkillID: skillID})

		// Capture skillID directly in the closure — no map lookup, so
		// Future identity wobbles don't cost us attribution. Per-child
		// state lives in a pointer-to-struct that both callbacks share.
		sid := skillID
		state := &childState{}

		// Two selector cases per child: the start-time future and the
		// result-time future. Both attributed to this skillID. The
		// start-future fires first; if it carries
		// WorkflowExecutionAlreadyStartedError we mark `state.skip` so
		// the result callback (which would re-raise the same error)
		// doesn't double-count.
		selector.AddFuture(fut.GetChildWorkflowExecution(), func(f workflow.Future) {
			if err := f.Get(ctx, nil); err != nil {
				state.startFailed = true
				if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
					logger.Info("child check skipped: already running", "skill_id", sid)
					res.Failed = append(res.Failed, sid+":"+childAlreadyRunningReason)
					return
				}
				logger.Warn("child check failed to start", "skill_id", sid, "error", err)
				res.Failed = append(res.Failed, sid)
			}
		})
		selector.AddFuture(fut, func(f workflow.Future) {
			inflight--
			if state.startFailed {
				// Start-future callback already recorded the failure;
				// don't double-count.
				return
			}
			var childRes CheckSkillSourceResult
			if err := f.Get(ctx, &childRes); err != nil {
				logger.Warn("child check failed", "skill_id", sid, "error", err)
				res.Failed = append(res.Failed, sid)
				return
			}
			res.Checked++
			if childRes.Changed {
				res.Drifted = append(res.Drifted, sid)
			}
		})
		inflight++
	}

	for _, id := range skillIDs {
		// If we've hit the parallelism cap, drain one slot before
		// adding the next. Selector.Select blocks the workflow
		// (deterministically) until one of the added futures fires.
		for inflight >= batchParallelism {
			selector.Select(ctx)
		}
		startChild(id)
	}

	// Drain remaining in-flight children.
	for inflight > 0 {
		selector.Select(ctx)
	}

	res.FinishedAt = workflow.Now(ctx)
	logger.Info("CheckAllSkillSourcesWorkflow done",
		"checked", res.Checked,
		"drifted", len(res.Drifted),
		"failed", len(res.Failed),
	)
	return res, nil
}

// SourceMetaResult mirrors activities.SourceMeta but is declared in
// this package to keep the workflow → activities import edge one-way
// (activities → workflows for the input types). Temporal's argument
// encoder round-trips between the two by JSON field tag.
type SourceMetaResult struct {
	SourceURL string `json:"source_url"`
	SourceSHA string `json:"source_sha"`
}

// RefetchActivityResult mirrors activities.RefetchResult. Same
// rationale as SourceMetaResult — keep the workflows package
// dependency-free.
type RefetchActivityResult struct {
	Body   string `json:"body"`
	NewSHA string `json:"new_sha"`
}

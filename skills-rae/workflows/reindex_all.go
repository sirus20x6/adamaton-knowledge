package workflows

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ReindexAllSkillsWorkflow iterates every skill and runs
// IndexSkillWorkflow as a child workflow in concurrency-capped
// batches. After the last skill is processed, runs
// RecomputeCommunities to rebuild the community graph.
//
// Carries progress across ContinueAsNew boundaries so history stays
// bounded at scale — mirrors the reindex/workflows/corpus.go pattern.
//
// Inputs:
//
//	ReindexAllInput{Concurrency, ContinueAsNewEvery, SkipClustering, K}
//
// Outputs (on final completion, not on each ContinueAsNew):
//
//	ReindexAllResult{SkillsIndexed, SkillsFailed, CommunitiesBuilt, ...}
func ReindexAllSkillsWorkflow(ctx workflow.Context, in ReindexAllInput) (*ReindexAllResult, error) {
	return reindexAllWithCarry(ctx, in, ReindexAllCarry{})
}

// ReindexAllSkillsWorkflowResume is the ContinueAsNew entry point. The
// caller (the workflow itself, mid-loop) hands us its current carry so
// progress survives. Registered under its own name so Temporal knows
// to dispatch to this handler, not the initial-call one.
func ReindexAllSkillsWorkflowResume(ctx workflow.Context, in ReindexAllInput, carry ReindexAllCarry) (*ReindexAllResult, error) {
	return reindexAllWithCarry(ctx, in, carry)
}

func reindexAllWithCarry(ctx workflow.Context, in ReindexAllInput, carry ReindexAllCarry) (*ReindexAllResult, error) {
	logger := workflow.GetLogger(ctx)

	conc := in.Concurrency
	if conc <= 0 {
		conc = 4
	}
	if conc > 16 {
		conc = 16
	}
	limit := in.ContinueAsNewEvery
	if limit <= 0 {
		limit = 200
	}

	// On the very first call (carry empty), hydrate the work-set
	// from the DB. Subsequent ContinueAsNew passes keep the same
	// list so the workflow is deterministic across continuations.
	if len(carry.SkillIDs) == 0 {
		listOpts := workflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Second,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 3,
			},
		}
		var idStrings []string
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, listOpts), "ListAllSkillIDs").Get(ctx, &idStrings); err != nil {
			return nil, err
		}
		carry.SkillIDs = parseUUIDs(idStrings)
		carry.StartedAtUnixMs = workflow.Now(ctx).UnixMilli()
		logger.Info("ReindexAll loaded work-set", "count", len(carry.SkillIDs))
	}

	// Process up to ``limit`` more skills in concurrency-capped
	// batches, then ContinueAsNew (or finalise if we're done).
	endThisRun := carry.Index + limit
	if endThisRun > len(carry.SkillIDs) {
		endThisRun = len(carry.SkillIDs)
	}

	// Simple batched fan-out: spawn ``conc`` children, wait for all,
	// repeat until ``endThisRun`` reached. Deterministic + bounded
	// in-flight count without the deterministic-semaphore dance.
	for i := carry.Index; i < endThisRun; i += conc {
		batchEnd := i + conc
		if batchEnd > endThisRun {
			batchEnd = endThisRun
		}
		futures := make([]workflow.ChildWorkflowFuture, 0, batchEnd-i)
		for j := i; j < batchEnd; j++ {
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				WorkflowID:               childWorkflowID(carry.SkillIDs[j]),
				WorkflowExecutionTimeout: 10 * time.Minute,
				WorkflowTaskTimeout:      30 * time.Second,
			})
			fut := workflow.ExecuteChildWorkflow(childCtx, WorkflowIndexSkill, IndexSkillInput{SkillID: carry.SkillIDs[j]})
			futures = append(futures, fut)
		}
		for _, f := range futures {
			var res IndexSkillResult
			if err := f.Get(ctx, &res); err != nil {
				carry.SkillsFailed++
				continue
			}
			carry.SkillsIndexed++
			carry.SubunitsTotal += res.SubunitsTotal
		}
	}
	carry.Index = endThisRun

	if carry.Index < len(carry.SkillIDs) {
		// More to do; pass the baton via ContinueAsNew. The resume
		// workflow gets the carry pre-populated so it skips the
		// listing activity.
		return nil, workflow.NewContinueAsNewError(ctx, "ReindexAllSkillsWorkflowResume", in, carry)
	}

	// Final pass: rebuild communities.
	commBuilt := 0
	if !in.SkipClustering {
		commOpts := workflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 3,
			},
		}
		commCtx := workflow.WithActivityOptions(ctx, commOpts)
		var commRes RecomputeCommunitiesResult
		commIn := RecomputeCommunitiesInput{TopSubunitsPerSkill: 5}
		if err := workflow.ExecuteActivity(commCtx, ActHDBSCANCluster, commIn).Get(commCtx, &commRes); err != nil {
			logger.Error("RecomputeCommunities failed", "err", err)
		} else {
			commBuilt = commRes.CommunitiesCreated
		}
	}

	elapsed := workflow.Now(ctx).UnixMilli() - carry.StartedAtUnixMs

	return &ReindexAllResult{
		SkillsIndexed:    carry.SkillsIndexed,
		SkillsFailed:     carry.SkillsFailed,
		CommunitiesBuilt: commBuilt,
		SubunitsTotal:    carry.SubunitsTotal,
		ElapsedMs:        int(elapsed),
	}, nil
}

// parseUUIDs converts a []string of UUIDs (the activity output shape)
// back to []uuid.UUID for the workflow's typed carry.
func parseUUIDs(ss []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// childWorkflowID builds a stable child WF id so retries dedupe.
// Stamping the skill UUID is enough — at most one IndexSkillWorkflow
// per skill in-flight at a time.
func childWorkflowID(id uuid.UUID) string {
	return fmt.Sprintf("skills-rae-index-%s", id.String())
}

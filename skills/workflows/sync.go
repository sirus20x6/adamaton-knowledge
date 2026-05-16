package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// activity method names. Workflows reference activities by string name
// (rather than method value) so this package stays free of an import
// edge to skills/activities — keeps the dependency direction one-way
// (activities → workflows for the input types) and matches how the
// dashboard's ExecuteWorkflow calls reference workflows by name.
const (
	activityPostSkillToR2R     = "PostSkillToR2R"
	activityDeleteSkillFromR2R = "DeleteSkillFromR2R"
	activityStampR2RDocID      = "StampR2RDocumentID"
)

// SyncSkillToR2RWorkflow mirrors an evo.skills record to the platform
// R2R index. Replaces the fire-and-forget goroutine that previously
// lived in dashboard/apiserver/skills_r2r_sync.go: by running through
// Temporal the mirror survives dashboard restarts, R2R outages, and
// activity-host crashes — Temporal retries until success or the 20-
// attempt ceiling.
//
// Two-activity shape:
//
//  1. PostSkillToR2R — POSTs the skill payload to /platform/skills/ingest
//  2. StampR2RDocumentID — UPDATEs evo.skills.r2r_document_id so the
//     dashboard can link to the indexed document without polling R2R.
//
// Retry policy mirrors temporal/workflows/delegation_workflow.go:54-73
// but is much more patient (MaximumAttempts=20): R2R can be down for
// hours during a Pi reboot and the workflow should ride that out.
func SyncSkillToR2RWorkflow(ctx workflow.Context, in SyncSkillToR2RInput) (*SyncSkillToR2RResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("SyncSkillToR2RWorkflow start", "skill_id", in.SkillID)

	postOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 90 * time.Second,
		// HeartbeatTimeout removed: PostSkillToR2R only heartbeats once
		// at entry and a slow R2R response > 30s would cause a false
		// heartbeat-timeout while the HTTP call was still in flight.
		// StartToCloseTimeout (90s) is the real watchdog.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    20,
		},
	}
	postCtx := workflow.WithActivityOptions(ctx, postOpts)
	if err := workflow.ExecuteActivity(postCtx, activityPostSkillToR2R, in).Get(postCtx, nil); err != nil {
		logger.Warn("SyncSkillToR2RWorkflow post failed", "skill_id", in.SkillID, "error", err)
		return nil, err
	}

	stampOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		// HeartbeatTimeout removed for symmetry with postOpts — the
		// stamp activity is a single Postgres UPDATE, no need to
		// heartbeat inside it.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    10,
		},
	}
	stampCtx := workflow.WithActivityOptions(ctx, stampOpts)
	// The activity reads SKILLS_R2R_CORPUS_ID off its struct field; the
	// workflow just calls Stamp and lets the activity branch on whether
	// the corpus id was configured at worker startup.
	if err := workflow.ExecuteActivity(stampCtx, activityStampR2RDocID, in.SkillID).Get(stampCtx, nil); err != nil {
		logger.Warn("SyncSkillToR2RWorkflow stamp failed", "skill_id", in.SkillID, "error", err)
		return nil, err
	}

	info := workflow.GetInfo(ctx)
	res := &SyncSkillToR2RResult{
		R2RDocumentID: in.SkillID,
		Attempts:      int(info.Attempt),
	}
	logger.Info("SyncSkillToR2RWorkflow done",
		"skill_id", in.SkillID,
		"attempts", res.Attempts,
	)
	return res, nil
}

// DeleteSkillFromR2RWorkflow removes a previously-mirrored skill from
// the R2R index. The evo.skills row has already been deleted by the
// dashboard before this workflow runs (the document_id was captured
// into the input ahead of the DELETE), so the workflow's only job is
// to make the platform mirror catch up.
//
// 404 from the platform is classified as success by the activity (the
// document is already gone — that's the goal state), so the workflow
// returns nil and the retry policy never fires for that case.
func DeleteSkillFromR2RWorkflow(ctx workflow.Context, in DeleteSkillFromR2RInput) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("DeleteSkillFromR2RWorkflow start", "skill_id", in.SkillID)

	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		// HeartbeatTimeout removed — DeleteSkillFromR2R heartbeats once
		// at entry only and a slow platform response > 30s would
		// otherwise produce a false timeout.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    10,
		},
	}
	actCtx := workflow.WithActivityOptions(ctx, opts)
	if err := workflow.ExecuteActivity(actCtx, activityDeleteSkillFromR2R, in).Get(actCtx, nil); err != nil {
		logger.Warn("DeleteSkillFromR2RWorkflow failed", "skill_id", in.SkillID, "error", err)
		return err
	}
	logger.Info("DeleteSkillFromR2RWorkflow done", "skill_id", in.SkillID)
	return nil
}

package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// RecomputeCommunitiesWorkflow runs only the cluster step on the
// current corpus state. Useful when you want to rebuild communities
// without re-running every IndexSkillWorkflow (e.g. after tuning K
// or after the IDF math changed and you don't want to re-embed every
// subunit).
//
// Inputs mirror RecomputeCommunitiesInput so callers can override K,
// MaxIter, TopSubunitsPerSkill, Seed.
func RecomputeCommunitiesWorkflow(ctx workflow.Context, in RecomputeCommunitiesInput) (*RecomputeCommunitiesResult, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, opts)
	var out RecomputeCommunitiesResult
	if err := workflow.ExecuteActivity(ctx, ActHDBSCANCluster, in).Get(ctx, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

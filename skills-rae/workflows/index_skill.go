package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// IndexSkillWorkflow is the per-skill indexing pipeline. Sequence:
//
//   ExtractSubunits           — deterministic; cheap; postgres read.
//   LoadSkillForEmbedding     — fetch description for the per-skill embed.
//   EmbedBatch                — one call to bge-m3 covering all subunit
//                               texts + the description; ~30 inputs.
//   UpsertGraph               — single activity wraps three pg writes
//                               (subunits, edges, description_embedding).
//
// We keep activities small + idempotent so a retry on a transient
// failure doesn't break the graph. The whole workflow is ~3-4
// seconds on the Pi for a skill with ~30 subunits, dominated by
// the embedding call.
func IndexSkillWorkflow(ctx workflow.Context, in IndexSkillInput) (*IndexSkillResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("IndexSkillWorkflow", "skill_id", in.SkillID)

	actOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, actOpts)

	var subs []SubunitWire
	if err := workflow.ExecuteActivity(ctx, ActExtractSubunits, in).Get(ctx, &subs); err != nil {
		return nil, err
	}

	var meta SkillDescriptionWire
	if err := workflow.ExecuteActivity(ctx, ActLoadSkillForEmbedding, in).Get(ctx, &meta); err != nil {
		return nil, err
	}

	// Build the embed batch: every subunit's content followed by the
	// skill description, in that order. The slice order is what
	// UpsertGraph keys vectors against.
	texts := make([]string, 0, len(subs)+1)
	for _, s := range subs {
		texts = append(texts, s.Content)
	}
	descIdx := -1
	if meta.Description != "" {
		descIdx = len(texts)
		texts = append(texts, meta.Description)
	}

	var embedRes EmbedBatchResult
	if len(texts) > 0 {
		if err := workflow.ExecuteActivity(ctx, ActEmbedBatch, EmbedBatchInput{Texts: texts}).Get(ctx, &embedRes); err != nil {
			return nil, err
		}
	}

	subVecs := make([][]float32, len(subs))
	for i := range subs {
		if i < len(embedRes.Vectors) {
			subVecs[i] = embedRes.Vectors[i]
		}
	}
	var descVec []float32
	if descIdx >= 0 && descIdx < len(embedRes.Vectors) {
		descVec = embedRes.Vectors[descIdx]
	}

	var upsertRes UpsertGraphResult
	upIn := UpsertGraphInput{
		SkillID:           in.SkillID,
		Subunits:          subs,
		SubunitVectors:    subVecs,
		DescriptionVector: descVec,
	}
	if err := workflow.ExecuteActivity(ctx, ActUpsertGraph, upIn).Get(ctx, &upsertRes); err != nil {
		return nil, err
	}

	// Best-effort: ask the cluster-sidecar to snap this skill into the
	// nearest existing leaf community. If the sidecar is down, no
	// communities exist yet, or the assign fails, we still consider
	// the index successful — the skill remains findable via L1 +
	// bottom-up retrieval; only top-down community routing is affected.
	assignOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    5 * time.Second,
			MaximumAttempts:    2,
		},
	}
	assignCtx := workflow.WithActivityOptions(ctx, assignOpts)
	var assignRes AssignToNearestCommunityResult
	if err := workflow.ExecuteActivity(assignCtx, ActAssignToNearestCommunity,
		AssignToNearestCommunityInput{SkillID: in.SkillID}).Get(ctx, &assignRes); err != nil {
		logger.Warn("AssignToNearestCommunity failed; skill remains uncommunified",
			"skill_id", in.SkillID, "error", err)
	}

	return &IndexSkillResult{
		SkillID:          in.SkillID,
		SubunitsTotal:    upsertRes.SubunitsTotal,
		SubunitsByKind:   upsertRes.SubunitsByKind,
		EmbeddingsCalled: len(texts),
	}, nil
}

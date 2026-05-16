package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/clusterclient"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/store"
)

// AssignDeps wraps the pool + cluster-sidecar client. No embedder
// here — the assign hot path reuses an embedding already computed by
// IndexSkillWorkflow (description_embedding) rather than calling
// bge-m3 again.
type AssignDeps struct {
	Pool          *pgxpool.Pool
	ClusterClient *clusterclient.Client
}

// AssignToNearestCommunityInput is the per-skill input for the assign
// step that runs at the tail of IndexSkillWorkflow.
type AssignToNearestCommunityInput struct {
	SkillID uuid.UUID `json:"skill_id"`
}

// AssignToNearestCommunityResult records what the assign decided.
// Surfaced in Temporal history for debugging — the canonical state
// is the evo.skills row this activity updated.
type AssignToNearestCommunityResult struct {
	SkillID     uuid.UUID  `json:"skill_id"`
	CommunityID *uuid.UUID `json:"community_id,omitempty"`
	Similarity  float64    `json:"similarity"`
	Noise       bool       `json:"noise"`
	Skipped     bool       `json:"skipped"`
	SkipReason  string     `json:"skip_reason,omitempty"`
}

// AssignToNearestCommunity loads the skill's embedding (prefers
// skill_embedding; falls back to description_embedding), loads every
// leaf-level community representative, and asks the cluster-sidecar
// /v1/assign endpoint for the nearest match. Persistence is a single
// UPDATE that stamps community_id + community_noise atomically.
//
// Failure modes that are NOT errors (return Skipped=true so the
// workflow proceeds without failing):
//
//   - sidecar disabled (URL not set): operator hasn't deployed the
//     sidecar yet; skill stays unassigned.
//   - no leaf communities exist yet: the corpus has never been
//     clustered; the next RecomputeCommunities will fix that.
//   - skill has neither skill_embedding nor description_embedding
//     (shouldn't happen if IndexSkillWorkflow ran, but defensive).
//
// Any of those leave evo.skills.community_id alone, which means the
// skill remains invisible to top-down retrieval but L1 + bottom-up
// retrieval still work.
func (d *AssignDeps) AssignToNearestCommunity(ctx context.Context, in AssignToNearestCommunityInput) (*AssignToNearestCommunityResult, error) {
	if d.ClusterClient == nil || !d.ClusterClient.Enabled() {
		return &AssignToNearestCommunityResult{
			SkillID:    in.SkillID,
			Skipped:    true,
			SkipReason: "cluster-sidecar not configured",
		}, nil
	}

	vec, err := store.LoadVectorForAssign(ctx, d.Pool, in.SkillID)
	if err != nil {
		return nil, fmt.Errorf("load assign vector: %w", err)
	}
	if vec == nil {
		return &AssignToNearestCommunityResult{
			SkillID:    in.SkillID,
			Skipped:    true,
			SkipReason: "no skill_embedding or description_embedding",
		}, nil
	}

	reps, err := store.ListLeafCommunityReps(ctx, d.Pool)
	if err != nil {
		return nil, fmt.Errorf("list leaf communities: %w", err)
	}
	if len(reps) == 0 {
		return &AssignToNearestCommunityResult{
			SkillID:    in.SkillID,
			Skipped:    true,
			SkipReason: "no leaf communities yet (corpus not clustered)",
		}, nil
	}

	candidates := make([]clusterclient.AssignCommunity, len(reps))
	for i, r := range reps {
		candidates[i] = clusterclient.AssignCommunity{
			ID:        r.ID.String(),
			Embedding: r.Embedding,
		}
	}

	resp, err := d.ClusterClient.Assign(ctx, clusterclient.AssignRequest{
		Vector:      vec,
		Communities: candidates,
	})
	if err != nil {
		return nil, fmt.Errorf("sidecar assign: %w", err)
	}

	var commID *uuid.UUID
	if resp.CommunityID != nil {
		parsed, err := uuid.Parse(*resp.CommunityID)
		if err != nil {
			return nil, fmt.Errorf("parse community id %q: %w", *resp.CommunityID, err)
		}
		commID = &parsed
	}

	if err := store.StampCommunityAssignment(ctx, d.Pool, in.SkillID, commID, resp.Noise); err != nil {
		return nil, fmt.Errorf("stamp assignment: %w", err)
	}

	return &AssignToNearestCommunityResult{
		SkillID:     in.SkillID,
		CommunityID: commID,
		Similarity:  resp.Similarity,
		Noise:       resp.Noise,
	}, nil
}

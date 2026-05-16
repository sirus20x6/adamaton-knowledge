// Package workflows holds the Temporal workflow definitions for
// skills-rae. Workflow names are stable strings so they can be
// invoked from the existing skills-worker (which doesn't import this
// package) via workflow.ExecuteChildWorkflow on a string name.
package workflows

import (
	"github.com/google/uuid"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/activities"
)

// TaskQueue is the Temporal task queue this module's workflows + activities
// land on. Distinct from the existing "skills" queue so the indexer
// scales independently of the R2R-sync flow.
const TaskQueue = "skills-rae"

// Workflow names are exposed as strings so external callers can
// register child-workflow invocations without importing this package.
const (
	WorkflowIndexSkill          = "IndexSkillWorkflow"
	WorkflowReindexAllSkills    = "ReindexAllSkillsWorkflow"
	WorkflowRecomputeCommunities = "RecomputeCommunitiesWorkflow"
)

// Activity names. Used both by workflow registration in workflows/*
// and by the worker's RegisterActivityWithOptions calls in
// cmd/skills-rae-worker/main.go.
const (
	ActExtractSubunits            = "ExtractSubunits"
	ActLoadSkillForEmbedding      = "LoadSkillForEmbedding"
	ActEmbedBatch                 = "EmbedBatch"
	ActUpsertGraph                = "UpsertGraph"
	ActHDBSCANCluster             = "HDBSCANCluster"
	ActAssignToNearestCommunity   = "AssignToNearestCommunity"
)

// Re-export the activity input/output types so callers can avoid
// importing the activities package directly.
type (
	IndexSkillInput  = activities.IndexSkillInput
	IndexSkillResult = activities.IndexSkillResult

	SubunitWire = activities.SubunitWire

	EmbedBatchInput  = activities.EmbedBatchInput
	EmbedBatchResult = activities.EmbedBatchResult

	UpsertGraphInput  = activities.UpsertGraphInput
	UpsertGraphResult = activities.UpsertGraphResult

	RecomputeCommunitiesInput  = activities.RecomputeCommunitiesInput
	RecomputeCommunitiesResult = activities.RecomputeCommunitiesResult

	AssignToNearestCommunityInput  = activities.AssignToNearestCommunityInput
	AssignToNearestCommunityResult = activities.AssignToNearestCommunityResult

	ReindexAllInput  = activities.ReindexAllInput
	ReindexAllResult = activities.ReindexAllResult

	SkillDescriptionWire = activities.SkillDescriptionWire
)

// ReindexAllCarry is the per-iteration carry passed across
// ContinueAsNew boundaries so progress survives history compaction.
// We replicate the reindex/workflows/corpus.go pattern.
type ReindexAllCarry struct {
	SkillIDs       []uuid.UUID `json:"skill_ids"`
	Index          int         `json:"index"`
	SkillsIndexed  int         `json:"skills_indexed"`
	SkillsFailed   int         `json:"skills_failed"`
	SubunitsTotal  int         `json:"subunits_total"`
	StartedAtUnixMs int64      `json:"started_at_unix_ms"`
}

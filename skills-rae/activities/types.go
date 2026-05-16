// Package activities holds the Temporal activities executed by
// skills-rae-worker. Inputs + outputs are designed to be small and
// JSON-friendly so they replay through Temporal history cleanly.
package activities

import "github.com/google/uuid"

// IndexSkillInput is the payload for IndexSkillWorkflow.
type IndexSkillInput struct {
	SkillID uuid.UUID `json:"skill_id"`
}

// IndexSkillResult summarises what IndexSkillWorkflow did. Counts are
// surfaced into the Temporal UI for at-a-glance debugging.
type IndexSkillResult struct {
	SkillID       uuid.UUID `json:"skill_id"`
	SubunitsTotal int       `json:"subunits_total"`
	SubunitsByKind map[string]int `json:"subunits_by_kind"`
	EmbeddingsCalled int    `json:"embeddings_called"`
}

// ReindexAllInput is the payload for ReindexAllSkillsWorkflow. The
// Carry block lives in workflows/reindex_all.go's type; this is the
// initial-call shape only. K-means knobs are gone — HDBSCAN params
// live on the cluster-sidecar (env vars).
type ReindexAllInput struct {
	Concurrency        int  `json:"concurrency"`             // default 4
	ContinueAsNewEvery int  `json:"continue_as_new_every"`   // default 200
	SkipClustering     bool `json:"skip_clustering"`         // for tests
}

// ReindexAllResult is what ReindexAllSkillsWorkflow returns on final
// completion (post-ContinueAsNew loop).
type ReindexAllResult struct {
	SkillsIndexed     int `json:"skills_indexed"`
	SkillsFailed      int `json:"skills_failed"`
	CommunitiesBuilt  int `json:"communities_built"`
	SubunitsTotal     int `json:"subunits_total"`
	ElapsedMs         int `json:"elapsed_ms"`
}

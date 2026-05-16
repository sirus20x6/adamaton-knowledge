// Package workflows holds the Temporal workflow definitions for the
// skills subsystem: R2R mirror sync, GitHub importers, and check-source
// drift detection. Activities live in github.com/sirus20x6/adamomaton-knowledge/skills/activities;
// the worker binary that registers them lives in cmd/skills-worker.
//
// Workflow IDs follow a deterministic naming scheme so callers (the
// evo dashboard) can re-trigger work without coordinating with a state
// table:
//
//	skill-sync-{skill_id}       — one in-flight sync per skill at a time
//	skill-delete-{skill_id}     — same shape, delete half
//	skill-import-{request_uuid} — server-generated per import POST
//	skill-check-{skill_id}      — one-off check-source
//	skills-check-all-daily      — owned by the daily Temporal Schedule
package workflows

import "time"

// TaskQueue is the Temporal task queue this module's worker polls.
// The dashboard reads the same constant when calling ExecuteWorkflow,
// so both ends agree without a config file.
const TaskQueue = "skills"

// Workflow names registered on the task queue. ExecuteWorkflow callers
// reference these by string (per delegator/scheduler.go's pattern) to
// avoid pulling the workflows package into the caller.
const (
	WorkflowSyncSkillToR2R       = "SyncSkillToR2RWorkflow"
	WorkflowDeleteSkillFromR2R   = "DeleteSkillFromR2RWorkflow"
	WorkflowImportSkillsFromGH   = "ImportSkillsFromGithubWorkflow"
	WorkflowCheckSkillSource     = "CheckSkillSourceWorkflow"
	WorkflowCheckAllSkillSources = "CheckAllSkillSourcesWorkflow"
)

// QueryProgress is the query handler name exposed by long-running
// workflows (importers + bulk check). Callers run
// client.QueryWorkflow(ctx, workflowID, QueryProgress) to read live
// progress without affecting the workflow.
const QueryProgress = "progress"

// ScheduleIDCheckAllDaily is the Temporal Schedule that fires the
// daily fan-out of CheckSkillSourceWorkflow over every skill with a
// source_url. Created idempotently at skills-worker startup.
const ScheduleIDCheckAllDaily = "skills-check-all-daily"

// Skill mirrors dashboard/apiserver.Skill — JSON tags identical so the
// payload round-trips losslessly through Temporal's argument encoder.
// Defined here (not imported from dashboard) to keep module boundaries
// clean: the skills worker doesn't depend on the dashboard.
type Skill struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	Body            string     `json:"body"`
	WhenToUse       *string    `json:"when_to_use,omitempty"`
	Example         *string    `json:"example,omitempty"`
	Community       *string    `json:"community,omitempty"`
	Tags            []string   `json:"tags"`
	DependsOn       []string   `json:"depends_on"`
	Origin          string     `json:"origin"`
	SourceURL       *string    `json:"source_url,omitempty"`
	SourceSHA       *string    `json:"source_sha,omitempty"`
	SourceCheckedAt *time.Time `json:"source_checked_at,omitempty"`
	R2RDocumentID   *string    `json:"r2r_document_id,omitempty"`
	R2RCorpusID     *string    `json:"r2r_corpus_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// SyncSkillToR2RInput carries the full skill record so the workflow's
// normal path is a single activity call — no DB read needed unless
// retries push us into a re-fetch branch.
type SyncSkillToR2RInput struct {
	SkillID string `json:"skill_id"`
	Skill   Skill  `json:"skill"`
}

// SyncSkillToR2RResult reports the final R2R document_id (always the
// skill_id today — the platform mirror reuses it) and how many activity
// attempts were needed. Visible via DescribeWorkflowExecution.
type SyncSkillToR2RResult struct {
	R2RDocumentID string `json:"r2r_document_id"`
	Attempts      int    `json:"attempts"`
}

// DeleteSkillFromR2RInput is the input for the delete-half mirror. The
// document_id is the skill's UUID; passing it explicitly keeps the
// workflow free of a DB lookup (the row has already been deleted by the
// caller before this workflow is launched).
type DeleteSkillFromR2RInput struct {
	SkillID       string `json:"skill_id"`
	R2RDocumentID string `json:"r2r_document_id"`
}

// ImportSkillsFromGithubInput is the request shape for the importer
// workflow. Mirrors the dashboard's POST /api/v1/skills/import body
// minus fields that only make sense for the synchronous call (no
// "wait_for_complete" knob — the workflow is always async).
type ImportSkillsFromGithubInput struct {
	Kind      string   `json:"kind"`            // "github-file" | "github-repo"
	URL       string   `json:"url"`
	Subpath   string   `json:"subpath,omitempty"` // for github-repo
	Community string   `json:"community,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	// MaxFiles caps the GitHub repo walker. 0 = workflow's built-in
	// default (currently 2500, matching the synchronous importer).
	MaxFiles int `json:"max_files,omitempty"`
}

// ImportProgress is what the QueryProgress handler returns. UI polls
// every 2 seconds during import; values are monotonic except LastFile,
// which is the most recent path the walker touched.
type ImportProgress struct {
	Discovered int    `json:"discovered"`
	Imported   int    `json:"imported"`
	Updated    int    `json:"updated"`
	Unchanged  int    `json:"unchanged"`
	Errors     int    `json:"errors"`
	LastFile   string `json:"last_file"`
	Done       bool   `json:"done"`
}

// CheckSkillSourceInput drives a one-shot check-source workflow.
// SkillID is enough — the activity reads source_url out of evo.skills
// itself, so the workflow stays small and the source URL doesn't need
// to be re-encoded into Temporal history every retry.
type CheckSkillSourceInput struct {
	SkillID string `json:"skill_id"`
}

// CheckSkillSourceResult is what the workflow returns when complete.
// NewBody is only populated when Changed = true.
type CheckSkillSourceResult struct {
	SkillID string `json:"skill_id"`
	Changed bool   `json:"changed"`
	OldSHA  string `json:"old_sha"`
	NewSHA  string `json:"new_sha"`
	NewBody string `json:"new_body,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// BatchCheckResult is the aggregate the daily CheckAllSkillSourcesWorkflow
// returns. Drifted is a list of skill IDs that the child workflows
// flagged for review — the UI surfaces this as a notification.
type BatchCheckResult struct {
	Checked    int      `json:"checked"`
	Drifted    []string `json:"drifted"`
	Failed     []string `json:"failed"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

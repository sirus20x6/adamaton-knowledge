package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// WalkInput is the input shape for the WalkGithubRepo activity. Defined
// here because workflow code constructs it; the activity is in
// skills/activities/github.go and references this type.
type WalkInput struct {
	URL      string `json:"url"`
	Subpath  string `json:"subpath,omitempty"`
	Ref      string `json:"ref,omitempty"`
	MaxFiles int    `json:"max_files,omitempty"`
}

// UpsertInput is what UpsertSkill consumes: the parsed skill record plus
// the per-import overlays (community default + tag append) the workflow
// received from the caller. The activity applies them at write time.
type UpsertInput struct {
	Skill     Skill    `json:"skill"`
	Community string   `json:"community,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Origin    string   `json:"origin,omitempty"`
}

// Activity name constants — workflow.ExecuteActivity takes either a
// function reference or a string. We use strings so the workflow doesn't
// drag activity dependencies (pgxpool, http.Client) into its build graph.
const (
	activityWalkGithubRepo = "WalkGithubRepo"
	activityFetchGithubFile = "FetchGithubFile"
	activityUpsertSkill     = "UpsertSkill"
)

// defaultMaxFiles caps a single import workflow run at 2500 files —
// matches the synchronous importer's hard cap in
// dashboard/apiserver/skills_importers.go.
const defaultMaxFiles = 2500

// ImportSkillsFromGithubWorkflow walks a GitHub URL (single file or
// repo subtree), fetches every matching markdown file, parses it, and
// upserts it into evo.skills. Progress is exposed via the QueryProgress
// query handler so the dashboard can poll without blocking the workflow.
//
// The workflow is intentionally chatty: between each file we sleep
// 5 seconds (workflow.Sleep, not time.Sleep — survives worker restart
// and doesn't pin a worker slot) so a 2500-file repo spreads its GitHub
// load over hours rather than spiking a rate limit in seconds.
func ImportSkillsFromGithubWorkflow(ctx workflow.Context, in ImportSkillsFromGithubInput) (*ImportProgress, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("ImportSkillsFromGithubWorkflow start",
		"kind", in.Kind,
		"url", in.URL,
		"subpath", in.Subpath,
		"max_files", in.MaxFiles,
	)

	// Progress pointer is mutated in-place between activity calls; the
	// query handler hands back the live snapshot. Workflow code is single
	// threaded so no locking is needed.
	progress := &ImportProgress{}
	if err := workflow.SetQueryHandler(ctx, QueryProgress, func() (*ImportProgress, error) {
		return progress, nil
	}); err != nil {
		logger.Error("SetQueryHandler failed", "error", err)
		return progress, err
	}

	// Determine the cap. Spec: min(in.MaxFiles, 2500); default 2500 when
	// MaxFiles == 0.
	maxFiles := in.MaxFiles
	if maxFiles <= 0 || maxFiles > defaultMaxFiles {
		maxFiles = defaultMaxFiles
	}

	// Step 1: produce the file list. github-file is trivial (one URL);
	// github-repo runs the BFS walker activity.
	var files []string
	switch in.Kind {
	case "github-file":
		files = []string{in.URL}
	case "github-repo":
		walkCtx := workflow.WithActivityOptions(ctx, walkActivityOptions())
		walkInput := WalkInput{
			URL:      in.URL,
			Subpath:  in.Subpath,
			MaxFiles: maxFiles,
		}
		err := workflow.ExecuteActivity(walkCtx, activityWalkGithubRepo, walkInput).Get(walkCtx, &files)
		if err != nil {
			logger.Error("WalkGithubRepo failed", "error", err)
			progress.Done = true
			return progress, err
		}
	default:
		err := temporal.NewNonRetryableApplicationError(
			"unknown import kind: "+in.Kind, "BadInput", nil,
		)
		progress.Done = true
		return progress, err
	}

	// Cap discovered list at maxFiles. Walker may return more if its own
	// cap behaved differently — defensive trim here.
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	progress.Discovered = len(files)
	logger.Info("discovered files", "count", progress.Discovered)

	// Step 2: per-file fetch + upsert loop with 5s gap.
	fetchCtx := workflow.WithActivityOptions(ctx, fetchActivityOptions())
	upsertCtx := workflow.WithActivityOptions(ctx, upsertActivityOptions())

	for i, fileURL := range files {
		progress.LastFile = fileURL

		var skill Skill
		err := workflow.ExecuteActivity(fetchCtx, activityFetchGithubFile, fileURL).Get(fetchCtx, &skill)
		if err != nil {
			// Permanent failures (NonRetryable) reach us here; rate-limit
			// retries are handled inside the SDK via NextRetryDelay. Log
			// + count + move on so one bad file doesn't kill the batch.
			progress.Errors++
			logger.Warn("FetchGithubFile failed", "url", fileURL, "error", err)
			if i < len(files)-1 {
				if serr := workflow.Sleep(ctx, 5*time.Second); serr != nil {
					// Workflow cancellation while we were sleeping —
					// surface partial progress and bail.
					progress.Done = true
					return progress, serr
				}
			}
			continue
		}

		var status string
		upInput := UpsertInput{
			Skill:     skill,
			Community: in.Community,
			Tags:      in.Tags,
			Origin:    "github",
		}
		err = workflow.ExecuteActivity(upsertCtx, activityUpsertSkill, upInput).Get(upsertCtx, &status)
		if err != nil {
			progress.Errors++
			logger.Warn("UpsertSkill failed", "url", fileURL, "error", err)
			if i < len(files)-1 {
				if serr := workflow.Sleep(ctx, 5*time.Second); serr != nil {
					progress.Done = true
					return progress, serr
				}
			}
			continue
		}

		switch status {
		case "imported":
			progress.Imported++
		case "updated":
			progress.Updated++
		case "unchanged":
			progress.Unchanged++
		default:
			// Unknown status — treat as imported for visibility, log it.
			progress.Imported++
			logger.Warn("UpsertSkill returned unknown status", "status", status, "url", fileURL)
		}

		if i < len(files)-1 {
			if err := workflow.Sleep(ctx, 5*time.Second); err != nil {
				// Cancellation / workflow termination — return progress
				// as-is so the caller sees partial results.
				progress.Done = true
				return progress, err
			}
		}
	}

	progress.Done = true
	logger.Info("ImportSkillsFromGithubWorkflow done",
		"discovered", progress.Discovered,
		"imported", progress.Imported,
		"updated", progress.Updated,
		"unchanged", progress.Unchanged,
		"errors", progress.Errors,
	)
	return progress, nil
}

// walkActivityOptions tunes WalkGithubRepo — single call, lists
// directory contents only, but BFS can touch many subdirs. 60s
// StartToClose with a heartbeat every 10 files.
func walkActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    60 * time.Second,
		ScheduleToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:       30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    5 * time.Minute,
			MaximumAttempts:    0, // unlimited — rate-limit retries are honoured via NextRetryDelay
		},
	}
}

// fetchActivityOptions tunes FetchGithubFile — one HTTP GET + parse,
// 30s is plenty. Retries on transient network errors only; the activity
// classifies parse errors as NonRetryable.
func fetchActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: 1 * time.Hour, // accommodate rate-limit backoff
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    5 * time.Minute,
			MaximumAttempts:    5,
		},
	}
}

// upsertActivityOptions tunes UpsertSkill — pure Postgres write,
// idempotent. 10s StartToClose; permanent errors (name collision) are
// NonRetryable.
func upsertActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

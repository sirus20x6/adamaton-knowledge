// Phase 2C registrations + bootstrap for the check-source drift
// detector. Kept in its own file so the three Phase 2 agents can land
// their pieces without conflicting on main.go. main.go's stubs for
// registerWorkflows / registerActivities / ensureSchedules delegate
// here via the corresponding registerCheckXxx + ensureCheckSchedule
// helpers — wired in once all three agents have merged.
package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	skillsactivities "github.com/sirus20x6/adamaton-knowledge/skills/activities"
	skillsworkflows "github.com/sirus20x6/adamaton-knowledge/skills/workflows"
)

// registerCheckWorkflows wires the two check-source workflow functions
// into the worker under the names from skills/workflows/types.go. The
// dashboard's ExecuteWorkflow lookups reference those constants, so
// the names here must match exactly.
func registerCheckWorkflows(w worker.Worker) {
	w.RegisterWorkflowWithOptions(skillsworkflows.CheckSkillSourceWorkflow,
		workflow.RegisterOptions{Name: skillsworkflows.WorkflowCheckSkillSource})
	w.RegisterWorkflowWithOptions(skillsworkflows.CheckAllSkillSourcesWorkflow,
		workflow.RegisterOptions{Name: skillsworkflows.WorkflowCheckAllSkillSources})
}

// registerCheckActivities constructs CheckActivities with its injected
// dependencies and registers it on the worker. Each method on the
// struct becomes an activity addressable by its Go method name; the
// workflows reference them via string constants in check_source.go.
func registerCheckActivities(w worker.Worker, pool *pgxpool.Pool, httpClient *http.Client, logger *logrus.Logger) {
	check := &skillsactivities.CheckActivities{
		Pool:       pool,
		HTTPClient: httpClient,
		Logger:     logger,
	}
	w.RegisterActivityWithOptions(check.LoadSkillSourceMeta,
		activity.RegisterOptions{Name: "LoadSkillSourceMeta"})
	w.RegisterActivityWithOptions(check.RefetchSource,
		activity.RegisterOptions{Name: "RefetchSource"})
	w.RegisterActivityWithOptions(check.MarkSourceChecked,
		activity.RegisterOptions{Name: "MarkSourceChecked"})
	w.RegisterActivityWithOptions(check.FlagDrift,
		activity.RegisterOptions{Name: "FlagDrift"})
	w.RegisterActivityWithOptions(check.QuerySkillsWithSourceURLs,
		activity.RegisterOptions{Name: "QuerySkillsWithSourceURLs"})
}

// ensureCheckSchedule installs the daily Temporal Schedule that fans
// out CheckAllSkillSourcesWorkflow over every skill with a source_url.
// Mirrors delegator/scheduler.go:81-127 for the Create call shape and
// delegator/scheduler.go:227-233 for the already-exists tolerance.
//
// Cron "17 3 * * *" — 03:17 local, daily. The off-minute (17 rather
// than 0 or 30) matches the Temporal-conventional avoidance of
// round-number minutes that hammer the scheduler at the same instant.
//
// Idempotent: when the schedule already exists, the Create call
// returns an error whose message contains "already exists"; we
// recognise it via a substring match (rather than errors.Is, because
// the SDK doesn't export a typed error for this case) and return nil.
// The worker continues either way.
func ensureCheckSchedule(ctx context.Context, c client.Client, taskQueue string, logger *logrus.Logger) error {
	scheduleClient := c.ScheduleClient()
	_, err := scheduleClient.Create(ctx, client.ScheduleOptions{
		ID: skillsworkflows.ScheduleIDCheckAllDaily,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{"17 3 * * *"},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "skills-check-all",
			Workflow:  skillsworkflows.WorkflowCheckAllSkillSources,
			TaskQueue: taskQueue,
		},
		// Explicit overlap policy: a previous batch may still be
		// draining the 2500-skill tail when 03:17 rolls around again.
		// SKIP means we don't queue a second copy on top — the drift
		// detector is idempotent, missing a day is fine, two copies of
		// it hammering GitHub at once is not.
		Overlap: enums.SCHEDULE_OVERLAP_POLICY_SKIP,
		// Catchup window: if Temporal was down across the scheduled
		// time, only fire one make-up run within the past hour. The
		// default (~1 year) would spawn dozens of catch-up runs if the
		// Pi was offline for a weekend.
		CatchupWindow: 1 * time.Hour,
		Note:          "daily check-source drift detection",
	})
	if err != nil {
		if isAlreadyExistsErr(err) {
			logger.WithField("schedule_id", skillsworkflows.ScheduleIDCheckAllDaily).
				Info("daily check-source schedule already exists; reusing")
			return nil
		}
		return err
	}
	logger.WithField("schedule_id", skillsworkflows.ScheduleIDCheckAllDaily).
		Info("daily check-source schedule created")
	return nil
}

// isAlreadyExistsErr recognises the schedule-already-exists case. The
// Go SDK surfaces it as *serviceerror.AlreadyExists (gRPC code 6); we
// prefer the typed check via errors.As. The lowercase-substring
// fallback covers transports/wrappers that may strip the typed error
// shape (kept for backwards compatibility with the original
// delegator/scheduler.go pattern).
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	var ae *serviceerror.AlreadyExists
	if errors.As(err, &ae) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "alreadyexists")
}

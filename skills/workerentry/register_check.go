package workerentry

// Phase 2C registrations: the check-source drift detector workflows
// and their activities. Lifted from
// skills/cmd/skills-worker/registrations_check.go; the schedule
// bootstrap moved to schedule.go in this package.

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
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

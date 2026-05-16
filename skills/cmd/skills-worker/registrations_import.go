// Phase 2B registrations: the GitHub importer workflow + its three
// activities. Lives alongside main.go in this binary so the worker
// build is self-contained; main.go calls these at startup.
package main

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	skillsactivities "github.com/sirus20x6/adamaton-knowledge/skills/activities"
	skillsworkflows "github.com/sirus20x6/adamaton-knowledge/skills/workflows"
)

// registerImportWorkflows binds the ImportSkillsFromGithubWorkflow under
// the name the dashboard uses when calling client.ExecuteWorkflow.
func registerImportWorkflows(w worker.Worker) {
	w.RegisterWorkflowWithOptions(
		skillsworkflows.ImportSkillsFromGithubWorkflow,
		workflow.RegisterOptions{Name: skillsworkflows.WorkflowImportSkillsFromGH},
	)
}

// registerImportActivities constructs the GithubActivities struct with
// its dependencies and registers it so each method (WalkGithubRepo,
// FetchGithubFile, UpsertSkill) becomes addressable by name on the
// "skills" task queue.
func registerImportActivities(w worker.Worker, pool *pgxpool.Pool, httpClient *http.Client, logger *logrus.Logger) {
	acts := &skillsactivities.GithubActivities{
		HTTPClient: httpClient,
		Pool:       pool,
		Logger:     logger,
	}
	w.RegisterActivity(acts)
}

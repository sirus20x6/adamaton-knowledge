package main

// Phase 2A registrations: SyncSkillToR2RWorkflow / DeleteSkillFromR2RWorkflow
// plus the R2RActivities and PersistActivities they depend on.
// Lives in its own file so 2B (import) and 2C (check-source) agents can
// add their own registrations_*.go without merge conflicts on main.go.

import (
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamomaton-knowledge/skills/activities"
	skillsworkflows "github.com/sirus20x6/adamomaton-knowledge/skills/workflows"
)

// registerSyncWorkflows wires the two Phase 2A workflows onto the
// worker under the names declared in skills/workflows/types.go. Names
// are explicit (not relying on the function name) so the dashboard's
// string-based ExecuteWorkflow calls stay decoupled from Go identifier
// changes.
func registerSyncWorkflows(w worker.Worker) {
	w.RegisterWorkflowWithOptions(
		skillsworkflows.SyncSkillToR2RWorkflow,
		workflow.RegisterOptions{Name: skillsworkflows.WorkflowSyncSkillToR2R},
	)
	w.RegisterWorkflowWithOptions(
		skillsworkflows.DeleteSkillFromR2RWorkflow,
		workflow.RegisterOptions{Name: skillsworkflows.WorkflowDeleteSkillFromR2R},
	)
}

// registerSyncActivities constructs the R2R and persist activity
// structs (reading SKILLS_R2R_* env vars once at startup, per the
// "values into struct fields, not per-call lookups" convention in
// evolve/activities/smoke.go:19-22) and registers them with the
// worker. The HTTP client passed in is reused — main.go already
// allocates one with a 120s ceiling.
func registerSyncActivities(w worker.Worker, pool *pgxpool.Pool, httpClient *http.Client, logger *logrus.Logger) {
	baseURL := envOr("SKILLS_R2R_BASE_URL", "https://deepresearch.local")
	corpusID := os.Getenv("SKILLS_R2R_CORPUS_ID")
	insecure := envBool("SKILLS_R2R_INSECURE")

	// If the caller-supplied client doesn't already opt in to insecure
	// TLS but the env says we should, swap in a dedicated client. We
	// don't mutate the shared client in place — other activity sets
	// (Phase 2B/2C) may want strict TLS.
	client := httpClient
	if insecure {
		client = activities.NewHTTPClient(true)
	}

	r2r := &activities.R2RActivities{
		HTTPClient: client,
		BaseURL:    baseURL,
		CorpusID:   corpusID,
		Insecure:   insecure,
		Logger:     logger,
	}
	persist := &activities.PersistActivities{
		Pool:     pool,
		CorpusID: corpusID,
		Logger:   logger,
	}
	w.RegisterActivity(r2r)
	w.RegisterActivity(persist)

	logger.WithFields(logrus.Fields{
		"base_url":  baseURL,
		"corpus":    truthy(corpusID),
		"insecure":  insecure,
	}).Info("registered Phase 2A sync activities")
}

// envBool mirrors the SKILLS_R2R_INSECURE parsing in
// dashboard/apiserver/skills_r2r_sync.go:48-51.
func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

// truthy collapses a string to a yes/no label for log lines so we don't
// leak a corpus UUID into structured logs at info level. Empty → "no",
// anything else → "yes".
func truthy(s string) string {
	if s == "" {
		return "no"
	}
	return "yes"
}

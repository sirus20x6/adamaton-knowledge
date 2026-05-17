// Package workerentry wires the skills subsystem onto a Temporal
// worker. Both the standalone skills-worker binary and the unified
// adamaton-worker binary call Register(...) to install workflows,
// activities, and the daily check-source schedule.
//
// Previously this code lived under skills/cmd/skills-worker/ as
// package main; lifting it into its own package lets the umbrella's
// consolidated worker import the same registration logic instead of
// duplicating it.
package workerentry

import (
	"context"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Deps groups the dependencies every registration helper in this
// package needs. The caller (skills-worker main.go or adamaton-worker)
// is responsible for opening the postgres pool and Temporal client;
// SKILLS_R2R_* env vars are read inline by the registration helpers
// because they tune per-activity behaviour (insecure TLS, corpus id).
type Deps struct {
	Pool       *pgxpool.Pool
	HTTPClient *http.Client
	Logger     *logrus.Logger
	Temporal   client.Client
	TaskQueue  string
}

// Register installs every skills workflow + activity onto w and
// bootstraps the daily check-source Temporal Schedule. The schedule
// bootstrap is idempotent (already-exists is treated as success) and
// non-fatal: a failure here is logged and the worker continues to
// serve workflows. Activity / workflow registration calls do not
// return errors at the SDK level, so this function never reports one
// for them either.
func Register(ctx context.Context, w worker.Worker, deps Deps) {
	registerSyncWorkflows(w)
	registerSyncActivities(w, deps.Pool, deps.HTTPClient, deps.Logger)

	registerImportWorkflows(w)
	registerImportActivities(w, deps.Pool, deps.HTTPClient, deps.Logger)

	registerCheckWorkflows(w)
	registerCheckActivities(w, deps.Pool, deps.HTTPClient, deps.Logger)

	if err := ensureCheckSchedule(ctx, deps.Temporal, deps.TaskQueue, deps.Logger); err != nil {
		deps.Logger.WithError(err).Warn("daily check-source schedule bootstrap failed; worker continues")
	}
}

// envOr returns the env var's value or fallback if unset/empty.
func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envBool mirrors the SKILLS_R2R_INSECURE parsing in
// dashboard/apiserver/skills_r2r_sync.go (1/true/yes => true).
func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true" || v == "yes"
}

// truthy collapses a string to a yes/no label so log lines don't leak
// a corpus UUID at info level.
func truthy(s string) string {
	if s == "" {
		return "no"
	}
	return "yes"
}

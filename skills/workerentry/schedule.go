package workerentry

// Daily check-source schedule bootstrap. Lifted from
// skills/cmd/skills-worker/registrations_check.go; kept in its own
// file so the registration helpers and the schedule policy are easy
// to inspect separately.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	skillsworkflows "github.com/sirus20x6/adamaton-knowledge/skills/workflows"
)

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

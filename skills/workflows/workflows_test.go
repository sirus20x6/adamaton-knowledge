package workflows

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// workflows_test.go drives the R2R sync / delete and check-source
// workflows through the Temporal test environment with stub activities
// registered under the production string names. No R2R, no Postgres,
// no GitHub.

func TestSyncSkillToR2RWorkflow_HappyPath(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var posted SyncSkillToR2RInput
	var stamped string
	env.RegisterActivityWithOptions(func(ctx context.Context, in SyncSkillToR2RInput) error {
		posted = in
		return nil
	}, activity.RegisterOptions{Name: activityPostSkillToR2R})
	env.RegisterActivityWithOptions(func(ctx context.Context, skillID string) error {
		stamped = skillID
		return nil
	}, activity.RegisterOptions{Name: activityStampR2RDocID})
	env.RegisterWorkflow(SyncSkillToR2RWorkflow)

	in := SyncSkillToR2RInput{
		SkillID: "skill-123",
		Skill:   Skill{ID: "skill-123", Name: "n", Description: "d", Body: "b"},
	}
	env.ExecuteWorkflow(SyncSkillToR2RWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res SyncSkillToR2RResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.R2RDocumentID != "skill-123" {
		t.Errorf("R2RDocumentID = %q, want skill-123", res.R2RDocumentID)
	}
	if posted.SkillID != "skill-123" || posted.Skill.Name != "n" {
		t.Errorf("posted = %+v", posted)
	}
	if stamped != "skill-123" {
		t.Errorf("stamped = %q, want skill-123", stamped)
	}
}

func TestSyncSkillToR2RWorkflow_PostFailurePropagates(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	stampCalled := false
	env.RegisterActivityWithOptions(func(ctx context.Context, in SyncSkillToR2RInput) error {
		// Non-retryable so the test doesn't sit through 20 mock retries.
		return temporal.NewNonRetryableApplicationError("r2r rejected payload", "BadPayload", nil)
	}, activity.RegisterOptions{Name: activityPostSkillToR2R})
	env.RegisterActivityWithOptions(func(ctx context.Context, skillID string) error {
		stampCalled = true
		return nil
	}, activity.RegisterOptions{Name: activityStampR2RDocID})
	env.RegisterWorkflow(SyncSkillToR2RWorkflow)

	env.ExecuteWorkflow(SyncSkillToR2RWorkflow, SyncSkillToR2RInput{SkillID: "s"})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow error when post fails")
	}
	if stampCalled {
		t.Error("StampR2RDocumentID must not run when the post failed")
	}
}

func TestDeleteSkillFromR2RWorkflow(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var got DeleteSkillFromR2RInput
	env.RegisterActivityWithOptions(func(ctx context.Context, in DeleteSkillFromR2RInput) error {
		got = in
		return nil
	}, activity.RegisterOptions{Name: activityDeleteSkillFromR2R})
	env.RegisterWorkflow(DeleteSkillFromR2RWorkflow)

	env.ExecuteWorkflow(DeleteSkillFromR2RWorkflow, DeleteSkillFromR2RInput{
		SkillID:       "skill-9",
		R2RDocumentID: "doc-9",
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got.SkillID != "skill-9" || got.R2RDocumentID != "doc-9" {
		t.Errorf("activity input = %+v", got)
	}
}

// checkSourceStubs registers the four check-source activities with
// canned data and records which ones ran.
type checkSourceStubs struct {
	meta        SourceMetaResult
	refetch     RefetchActivityResult
	refetchErr  error
	markCalled  bool
	driftCalled bool
}

func (s *checkSourceStubs) register(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(ctx context.Context, skillID string) (SourceMetaResult, error) {
		return s.meta, nil
	}, activity.RegisterOptions{Name: activityLoadSkillSourceMeta})
	env.RegisterActivityWithOptions(func(ctx context.Context, url string) (RefetchActivityResult, error) {
		if s.refetchErr != nil {
			return RefetchActivityResult{}, s.refetchErr
		}
		return s.refetch, nil
	}, activity.RegisterOptions{Name: activityRefetchSource})
	env.RegisterActivityWithOptions(func(ctx context.Context, skillID string) error {
		s.markCalled = true
		return nil
	}, activity.RegisterOptions{Name: activityMarkSourceChecked})
	env.RegisterActivityWithOptions(func(ctx context.Context, skillID string) error {
		s.driftCalled = true
		return nil
	}, activity.RegisterOptions{Name: activityFlagDrift})
}

func TestCheckSkillSourceWorkflow_NoSourceURL(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	stubs := &checkSourceStubs{meta: SourceMetaResult{SourceURL: "", SourceSHA: "old-sha"}}
	stubs.register(env)
	env.RegisterWorkflow(CheckSkillSourceWorkflow)

	env.ExecuteWorkflow(CheckSkillSourceWorkflow, CheckSkillSourceInput{SkillID: "s1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res CheckSkillSourceResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.Changed || res.Reason != "no source_url" || res.OldSHA != "old-sha" {
		t.Errorf("result = %+v", res)
	}
	if stubs.markCalled {
		t.Error("MarkSourceChecked must not run for skipped skill")
	}
	if stubs.driftCalled {
		t.Error("FlagDrift must not run for skipped skill")
	}
}

func TestCheckSkillSourceWorkflow_Unchanged(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	stubs := &checkSourceStubs{
		meta:    SourceMetaResult{SourceURL: "https://example.com/skill.md", SourceSHA: "sha-same"},
		refetch: RefetchActivityResult{Body: "content", NewSHA: "sha-same"},
	}
	stubs.register(env)
	env.RegisterWorkflow(CheckSkillSourceWorkflow)

	env.ExecuteWorkflow(CheckSkillSourceWorkflow, CheckSkillSourceInput{SkillID: "s2"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res CheckSkillSourceResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("Changed = true, want false")
	}
	if res.NewBody != "" {
		t.Errorf("NewBody = %q, want empty for unchanged skill", res.NewBody)
	}
	if !stubs.markCalled {
		t.Error("MarkSourceChecked must run even when unchanged")
	}
	if stubs.driftCalled {
		t.Error("FlagDrift must not run when SHA matches")
	}
}

func TestCheckSkillSourceWorkflow_Drift(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	stubs := &checkSourceStubs{
		meta:    SourceMetaResult{SourceURL: "https://example.com/skill.md", SourceSHA: "sha-old"},
		refetch: RefetchActivityResult{Body: "new content", NewSHA: "sha-new"},
	}
	stubs.register(env)
	env.RegisterWorkflow(CheckSkillSourceWorkflow)

	env.ExecuteWorkflow(CheckSkillSourceWorkflow, CheckSkillSourceInput{SkillID: "s3"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res CheckSkillSourceResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.NewSHA != "sha-new" || res.OldSHA != "sha-old" {
		t.Errorf("result = %+v", res)
	}
	if res.NewBody != "new content" {
		t.Errorf("NewBody = %q, want new content", res.NewBody)
	}
	if !stubs.markCalled || !stubs.driftCalled {
		t.Errorf("mark=%v drift=%v, want both true", stubs.markCalled, stubs.driftCalled)
	}
}

func TestCheckSkillSourceWorkflow_RefetchFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	stubs := &checkSourceStubs{
		meta:       SourceMetaResult{SourceURL: "https://example.com/skill.md", SourceSHA: "x"},
		refetchErr: temporal.NewNonRetryableApplicationError("404 from github", "NotFound", nil),
	}
	stubs.register(env)
	env.RegisterWorkflow(CheckSkillSourceWorkflow)

	env.ExecuteWorkflow(CheckSkillSourceWorkflow, CheckSkillSourceInput{SkillID: "s4"})
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow error when refetch fails")
	}
	if stubs.markCalled {
		t.Error("MarkSourceChecked must not run when refetch failed")
	}
}

func TestCheckAllSkillSourcesWorkflow_FanOut(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(func(ctx context.Context) ([]string, error) {
		return []string{"a", "b", "c"}, nil
	}, activity.RegisterOptions{Name: activityQuerySkillsWithSources})

	// Child stub: "b" drifts, "c" fails, "a" is clean.
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, in CheckSkillSourceInput) (*CheckSkillSourceResult, error) {
		switch in.SkillID {
		case "b":
			return &CheckSkillSourceResult{SkillID: "b", Changed: true}, nil
		case "c":
			return nil, errors.New("boom")
		default:
			return &CheckSkillSourceResult{SkillID: in.SkillID}, nil
		}
	}, workflow.RegisterOptions{Name: WorkflowCheckSkillSource})
	env.RegisterWorkflow(CheckAllSkillSourcesWorkflow)

	env.ExecuteWorkflow(CheckAllSkillSourcesWorkflow)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res BatchCheckResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.Checked != 2 {
		t.Errorf("Checked = %d, want 2", res.Checked)
	}
	if len(res.Drifted) != 1 || res.Drifted[0] != "b" {
		t.Errorf("Drifted = %v, want [b]", res.Drifted)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "c" {
		t.Errorf("Failed = %v, want [c]", res.Failed)
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("FinishedAt %v before StartedAt %v", res.FinishedAt, res.StartedAt)
	}
}

func TestCheckAllSkillSourcesWorkflow_EmptySet(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	env.RegisterActivityWithOptions(func(ctx context.Context) ([]string, error) {
		return nil, nil
	}, activity.RegisterOptions{Name: activityQuerySkillsWithSources})
	env.RegisterWorkflow(CheckAllSkillSourcesWorkflow)

	env.ExecuteWorkflow(CheckAllSkillSourcesWorkflow)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res BatchCheckResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.Checked != 0 || len(res.Drifted) != 0 || len(res.Failed) != 0 {
		t.Errorf("result = %+v, want all-zero", res)
	}
}

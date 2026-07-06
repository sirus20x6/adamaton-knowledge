package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// workflows_test.go drives the three skills-rae workflows through the
// Temporal test environment with stub activities registered under the
// production string names. No DB, no embedder, no sidecar.

// indexStubs records what the workflow sent to each activity so tests
// can assert on the batched-embed contract (subunit texts first, then
// the description, in slice order).
type indexStubs struct {
	subs        []SubunitWire
	desc        SkillDescriptionWire
	embedIn     EmbedBatchInput
	upsertIn    UpsertGraphInput
	assignCalls int
	assignErr   error
}

func (s *indexStubs) register(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(ctx context.Context, in IndexSkillInput) ([]SubunitWire, error) {
		return s.subs, nil
	}, activity.RegisterOptions{Name: ActExtractSubunits})
	env.RegisterActivityWithOptions(func(ctx context.Context, in IndexSkillInput) (SkillDescriptionWire, error) {
		return s.desc, nil
	}, activity.RegisterOptions{Name: ActLoadSkillForEmbedding})
	env.RegisterActivityWithOptions(func(ctx context.Context, in EmbedBatchInput) (EmbedBatchResult, error) {
		s.embedIn = in
		vecs := make([][]float32, len(in.Texts))
		for i := range vecs {
			vecs[i] = []float32{float32(i), 1}
		}
		return EmbedBatchResult{Vectors: vecs, Model: "bge-m3-test"}, nil
	}, activity.RegisterOptions{Name: ActEmbedBatch})
	env.RegisterActivityWithOptions(func(ctx context.Context, in UpsertGraphInput) (UpsertGraphResult, error) {
		s.upsertIn = in
		byKind := map[string]int{}
		for _, u := range in.Subunits {
			byKind[u.Kind]++
		}
		return UpsertGraphResult{SubunitsTotal: len(in.Subunits), SubunitsByKind: byKind}, nil
	}, activity.RegisterOptions{Name: ActUpsertGraph})
	env.RegisterActivityWithOptions(func(ctx context.Context, in AssignToNearestCommunityInput) (AssignToNearestCommunityResult, error) {
		s.assignCalls++
		if s.assignErr != nil {
			return AssignToNearestCommunityResult{}, s.assignErr
		}
		return AssignToNearestCommunityResult{SkillID: in.SkillID}, nil
	}, activity.RegisterOptions{Name: ActAssignToNearestCommunity})
}

func TestIndexSkillWorkflow_HappyPath(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	skillID := uuid.New()
	stubs := &indexStubs{
		subs: []SubunitWire{
			{Content: "step one", Hash: "h1", Kind: "step", Length: 8, LineNo: 1},
			{Content: "constraint two", Hash: "h2", Kind: "constraint", Length: 14, LineNo: 3},
		},
		desc: SkillDescriptionWire{SkillID: skillID, Name: "s", Description: "does things"},
	}
	stubs.register(env)
	env.RegisterWorkflow(IndexSkillWorkflow)

	env.ExecuteWorkflow(IndexSkillWorkflow, IndexSkillInput{SkillID: skillID})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	var res IndexSkillResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if res.SkillID != skillID {
		t.Errorf("SkillID = %s, want %s", res.SkillID, skillID)
	}
	if res.SubunitsTotal != 2 {
		t.Errorf("SubunitsTotal = %d, want 2", res.SubunitsTotal)
	}
	// 2 subunits + 1 description = 3 embed inputs.
	if res.EmbeddingsCalled != 3 {
		t.Errorf("EmbeddingsCalled = %d, want 3", res.EmbeddingsCalled)
	}
	if res.SubunitsByKind["step"] != 1 || res.SubunitsByKind["constraint"] != 1 {
		t.Errorf("SubunitsByKind = %v", res.SubunitsByKind)
	}

	// The embed batch contract: subunit contents in order, then the
	// description last.
	wantTexts := []string{"step one", "constraint two", "does things"}
	if len(stubs.embedIn.Texts) != len(wantTexts) {
		t.Fatalf("embed texts = %v", stubs.embedIn.Texts)
	}
	for i, w := range wantTexts {
		if stubs.embedIn.Texts[i] != w {
			t.Errorf("embed text[%d] = %q, want %q", i, stubs.embedIn.Texts[i], w)
		}
	}

	// UpsertGraph gets the description vector (last slot) and one
	// vector per subunit, keyed by slice order.
	if len(stubs.upsertIn.SubunitVectors) != 2 {
		t.Fatalf("subunit vectors = %d, want 2", len(stubs.upsertIn.SubunitVectors))
	}
	if stubs.upsertIn.SubunitVectors[1][0] != 1 {
		t.Errorf("subunit vector order broken: %v", stubs.upsertIn.SubunitVectors)
	}
	if stubs.upsertIn.DescriptionVector == nil || stubs.upsertIn.DescriptionVector[0] != 2 {
		t.Errorf("description vector = %v, want index-2 vector", stubs.upsertIn.DescriptionVector)
	}
	if stubs.assignCalls != 1 {
		t.Errorf("assign calls = %d, want 1", stubs.assignCalls)
	}
}

func TestIndexSkillWorkflow_EmptyDescriptionSkipsDescVector(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	skillID := uuid.New()
	stubs := &indexStubs{
		subs: []SubunitWire{{Content: "only step", Hash: "h1", Kind: "step", Length: 9}},
		desc: SkillDescriptionWire{SkillID: skillID}, // no description
	}
	stubs.register(env)
	env.RegisterWorkflow(IndexSkillWorkflow)

	env.ExecuteWorkflow(IndexSkillWorkflow, IndexSkillInput{SkillID: skillID})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res IndexSkillResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.EmbeddingsCalled != 1 {
		t.Errorf("EmbeddingsCalled = %d, want 1 (no description slot)", res.EmbeddingsCalled)
	}
	if stubs.upsertIn.DescriptionVector != nil {
		t.Errorf("DescriptionVector = %v, want nil", stubs.upsertIn.DescriptionVector)
	}
}

func TestIndexSkillWorkflow_AssignFailureIsTolerated(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	skillID := uuid.New()
	stubs := &indexStubs{
		subs:      []SubunitWire{{Content: "s", Hash: "h", Kind: "step", Length: 1}},
		desc:      SkillDescriptionWire{SkillID: skillID, Description: "d"},
		assignErr: errors.New("cluster sidecar down"),
	}
	stubs.register(env)
	env.RegisterWorkflow(IndexSkillWorkflow)

	env.ExecuteWorkflow(IndexSkillWorkflow, IndexSkillInput{SkillID: skillID})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("assign failure must not fail the workflow: %v", err)
	}
	var res IndexSkillResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.SubunitsTotal != 1 {
		t.Errorf("SubunitsTotal = %d, want 1", res.SubunitsTotal)
	}
	// Retry policy caps assign at 2 attempts.
	if stubs.assignCalls != 2 {
		t.Errorf("assign attempts = %d, want 2 (MaximumAttempts)", stubs.assignCalls)
	}
}

func TestRecomputeCommunitiesWorkflow(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	var gotIn RecomputeCommunitiesInput
	env.RegisterActivityWithOptions(func(ctx context.Context, in RecomputeCommunitiesInput) (RecomputeCommunitiesResult, error) {
		gotIn = in
		return RecomputeCommunitiesResult{CommunitiesCreated: 7, SkillsAssigned: 40, SkillsSkipped: 2}, nil
	}, activity.RegisterOptions{Name: ActHDBSCANCluster})
	env.RegisterWorkflow(RecomputeCommunitiesWorkflow)

	env.ExecuteWorkflow(RecomputeCommunitiesWorkflow, RecomputeCommunitiesInput{TopSubunitsPerSkill: 9})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res RecomputeCommunitiesResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.CommunitiesCreated != 7 || res.SkillsAssigned != 40 {
		t.Errorf("result = %+v", res)
	}
	if gotIn.TopSubunitsPerSkill != 9 {
		t.Errorf("input not forwarded: %+v", gotIn)
	}
}

// registerReindexAllStubs wires ListAllSkillIDs + a stub child
// IndexSkillWorkflow + the cluster activity for ReindexAll tests.
func registerReindexAllStubs(env *testsuite.TestWorkflowEnvironment, ids []string, failSkill string, clusterCalls *int) {
	env.RegisterActivityWithOptions(func(ctx context.Context) ([]string, error) {
		return ids, nil
	}, activity.RegisterOptions{Name: "ListAllSkillIDs"})
	env.RegisterActivityWithOptions(func(ctx context.Context, in RecomputeCommunitiesInput) (RecomputeCommunitiesResult, error) {
		if clusterCalls != nil {
			*clusterCalls++
		}
		return RecomputeCommunitiesResult{CommunitiesCreated: 3}, nil
	}, activity.RegisterOptions{Name: ActHDBSCANCluster})
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, in IndexSkillInput) (*IndexSkillResult, error) {
		if failSkill != "" && in.SkillID.String() == failSkill {
			return nil, errors.New("boom")
		}
		return &IndexSkillResult{SkillID: in.SkillID, SubunitsTotal: 5}, nil
	}, workflow.RegisterOptions{Name: WorkflowIndexSkill})
	env.RegisterWorkflowWithOptions(ReindexAllSkillsWorkflow, workflow.RegisterOptions{Name: WorkflowReindexAllSkills})
	env.RegisterWorkflowWithOptions(ReindexAllSkillsWorkflowResume, workflow.RegisterOptions{Name: "ReindexAllSkillsWorkflowResume"})
}

func TestReindexAllSkillsWorkflow_CompletesAndClusters(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	clusterCalls := 0
	registerReindexAllStubs(env, ids, "", &clusterCalls)

	env.ExecuteWorkflow(ReindexAllSkillsWorkflow, ReindexAllInput{Concurrency: 2})
	if !env.IsWorkflowCompleted() {
		t.Fatal("not completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res ReindexAllResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.SkillsIndexed != 3 || res.SkillsFailed != 0 {
		t.Errorf("indexed/failed = %d/%d, want 3/0", res.SkillsIndexed, res.SkillsFailed)
	}
	if res.SubunitsTotal != 15 {
		t.Errorf("SubunitsTotal = %d, want 15", res.SubunitsTotal)
	}
	if res.CommunitiesBuilt != 3 {
		t.Errorf("CommunitiesBuilt = %d, want 3", res.CommunitiesBuilt)
	}
	if clusterCalls != 1 {
		t.Errorf("cluster calls = %d, want 1", clusterCalls)
	}
}

func TestReindexAllSkillsWorkflow_ChildFailureCountsFailed(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	ids := []string{uuid.NewString(), uuid.NewString()}
	registerReindexAllStubs(env, ids, ids[0], nil)

	env.ExecuteWorkflow(ReindexAllSkillsWorkflow, ReindexAllInput{SkipClustering: true})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res ReindexAllResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.SkillsIndexed != 1 || res.SkillsFailed != 1 {
		t.Errorf("indexed/failed = %d/%d, want 1/1", res.SkillsIndexed, res.SkillsFailed)
	}
	if res.CommunitiesBuilt != 0 {
		t.Errorf("CommunitiesBuilt = %d, want 0 (SkipClustering)", res.CommunitiesBuilt)
	}
}

func TestReindexAllSkillsWorkflow_ContinueAsNew(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	registerReindexAllStubs(env, ids, "", nil)

	env.ExecuteWorkflow(ReindexAllSkillsWorkflow, ReindexAllInput{ContinueAsNewEvery: 2, SkipClustering: true})
	if !env.IsWorkflowCompleted() {
		t.Fatal("not completed")
	}
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected ContinueAsNewError after processing 2 of 3 skills")
	}
	if !workflow.IsContinueAsNewError(err) && !strings.Contains(err.Error(), "continue as new") {
		t.Fatalf("expected continue-as-new, got: %v", err)
	}
}

func TestReindexAllSkillsWorkflowResume_FinishesFromCarry(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()

	idA, idB, idC := uuid.New(), uuid.New(), uuid.New()
	// ListAllSkillIDs must NOT be called on resume (carry pre-populated);
	// register a poisoned stub to prove it.
	env.RegisterActivityWithOptions(func(ctx context.Context) ([]string, error) {
		return nil, errors.New("resume must not re-list")
	}, activity.RegisterOptions{Name: "ListAllSkillIDs"})
	env.RegisterWorkflowWithOptions(func(ctx workflow.Context, in IndexSkillInput) (*IndexSkillResult, error) {
		return &IndexSkillResult{SkillID: in.SkillID, SubunitsTotal: 1}, nil
	}, workflow.RegisterOptions{Name: WorkflowIndexSkill})
	env.RegisterWorkflowWithOptions(ReindexAllSkillsWorkflowResume, workflow.RegisterOptions{Name: "ReindexAllSkillsWorkflowResume"})

	carry := ReindexAllCarry{
		SkillIDs:        []uuid.UUID{idA, idB, idC},
		Index:           2, // first two already done in a previous run
		SkillsIndexed:   2,
		SubunitsTotal:   10,
		StartedAtUnixMs: 1000,
	}
	env.ExecuteWorkflow(ReindexAllSkillsWorkflowResume, ReindexAllInput{SkipClustering: true}, carry)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res ReindexAllResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	if res.SkillsIndexed != 3 {
		t.Errorf("SkillsIndexed = %d, want 3 (2 carried + 1 new)", res.SkillsIndexed)
	}
	if res.SubunitsTotal != 11 {
		t.Errorf("SubunitsTotal = %d, want 11", res.SubunitsTotal)
	}
}

func TestParseUUIDs_DropsInvalid(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	out := parseUUIDs([]string{a.String(), "not-a-uuid", b.String(), ""})
	if len(out) != 2 || out[0] != a || out[1] != b {
		t.Errorf("parseUUIDs = %v, want [%s %s]", out, a, b)
	}
}

func TestChildWorkflowID_Stable(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	got := childWorkflowID(id)
	want := "skills-rae-index-11111111-2222-3333-4444-555555555555"
	if got != want {
		t.Errorf("childWorkflowID = %q, want %q", got, want)
	}
}

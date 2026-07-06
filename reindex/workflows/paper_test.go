package workflows

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// paper_test.go drives ReindexArxivPaperWorkflow through the Temporal
// test environment with stub activities registered under the
// production string names. Pins the skip-classification contract the
// parent corpus workflow depends on (Skipped vs Failed vs Done), which
// previously had zero workflow-level coverage.

// paperStubs is the recording stub-set for one workflow execution.
type paperStubs struct {
	preflightHasLatex bool
	preflightReason   string

	prepareOK         bool
	prepareTier       string
	prepareHasFigures bool

	rewireNoOp bool

	// Recorded calls.
	skipReasons   []string
	chunkedDocIDs []string
	figureDocIDs  []string
	rewired       [][2]string // old, new
	deleted       []string
	staleDeleted  []string
}

type prepareIn struct {
	ArxivID  string `json:"arxiv_id"`
	NewDocID string `json:"new_doc_id"`
}
type prepareOut struct {
	OK         bool           `json:"ok"`
	SourceTier string         `json:"source_tier"`
	DocType    string         `json:"doctype"`
	Text       string         `json:"text"`
	Title      string         `json:"title"`
	SizeBytes  int64          `json:"size_bytes"`
	HasFigures bool           `json:"has_figures"`
	Metadata   map[string]any `json:"metadata"`
	Detail     string         `json:"detail"`
}
type chunkIn struct {
	DocumentID string `json:"document_id"`
}
type chunkOut struct {
	Chunks      int    `json:"chunks"`
	TotalTokens int    `json:"total_tokens"`
	Model       string `json:"model"`
}
type figuresIn struct {
	ArxivID    string `json:"arxiv_id"`
	DocumentID string `json:"document_id"`
}
type figuresOut struct {
	Figures int `json:"figures"`
}
type rewireIn struct {
	OldDocID string `json:"old_doc_id"`
	NewDocID string `json:"new_doc_id"`
}

func (s *paperStubs) register(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(ctx context.Context, in PreflightInput) (PreflightResult, error) {
		return PreflightResult{HasLatex: s.preflightHasLatex, Reason: s.preflightReason}, nil
	}, activity.RegisterOptions{Name: "PreflightArxiv"})

	env.RegisterActivityWithOptions(func(ctx context.Context, in markSkippedInput) error {
		s.skipReasons = append(s.skipReasons, in.Reason)
		return nil
	}, activity.RegisterOptions{Name: "MarkArxivSkipped"})

	env.RegisterActivityWithOptions(func(ctx context.Context, in prepareIn) (prepareOut, error) {
		return prepareOut{
			OK:         s.prepareOK,
			SourceTier: s.prepareTier,
			DocType:    "eprint",
			Text:       "paper text",
			Title:      "A Paper",
			SizeBytes:  1234,
			HasFigures: s.prepareHasFigures,
			Detail:     "detail",
		}, nil
	}, activity.RegisterOptions{Name: "PrepareArxivDocument"})

	env.RegisterActivityWithOptions(func(ctx context.Context, in chunkIn) (chunkOut, error) {
		s.chunkedDocIDs = append(s.chunkedDocIDs, in.DocumentID)
		return chunkOut{Chunks: 12, TotalTokens: 4000, Model: "bge-m3"}, nil
	}, activity.RegisterOptions{Name: "ChunkEmbedPersist"})

	env.RegisterActivityWithOptions(func(ctx context.Context, in figuresIn) (figuresOut, error) {
		s.figureDocIDs = append(s.figureDocIDs, in.DocumentID)
		return figuresOut{Figures: 3}, nil
	}, activity.RegisterOptions{Name: "ExtractArxivFigures"})

	env.RegisterActivityWithOptions(func(ctx context.Context, in rewireIn) error {
		if s.rewireNoOp {
			return temporal.NewNonRetryableApplicationError("corpus pointer already moved", "RewireNoOp", nil)
		}
		s.rewired = append(s.rewired, [2]string{in.OldDocID, in.NewDocID})
		return nil
	}, activity.RegisterOptions{Name: "RewireCorpusDocument"})

	env.RegisterActivityWithOptions(func(ctx context.Context, oldDocID string) error {
		s.staleDeleted = append(s.staleDeleted, oldDocID)
		return nil
	}, activity.RegisterOptions{Name: "DeleteStaleFigures"})

	env.RegisterActivityWithOptions(func(ctx context.Context, docID string) error {
		s.deleted = append(s.deleted, docID)
		return nil
	}, activity.RegisterOptions{Name: "DeleteR2RDocument"})
}

func runPaper(t *testing.T, stubs *paperStubs, in ReindexArxivPaperInput) *PaperResult {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	stubs.register(env)
	env.RegisterWorkflow(ReindexArxivPaperWorkflow)
	env.ExecuteWorkflow(ReindexArxivPaperWorkflow, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res PaperResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatal(err)
	}
	return &res
}

func TestReindexPaper_InvalidArxivID(t *testing.T) {
	stubs := &paperStubs{}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "definitely not an id", OldDocID: "old"})
	if !res.Skipped || res.SkipReason != "invalid_arxiv_id" {
		t.Errorf("result = %+v, want Skipped/invalid_arxiv_id", res)
	}
	if len(stubs.skipReasons) != 1 || stubs.skipReasons[0] != "invalid_arxiv_id" {
		t.Errorf("skip records = %v", stubs.skipReasons)
	}
	if len(stubs.chunkedDocIDs) != 0 || len(stubs.rewired) != 0 {
		t.Error("no downstream activity should run for an invalid id")
	}
}

func TestReindexPaper_OldStyleIDAccepted(t *testing.T) {
	// Pre-2007 "category/NNNNNNN" ids must not be classed invalid.
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: true, prepareTier: "latex"}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "hep-th/9901001", OldDocID: "old-1"})
	if res.Skipped {
		t.Errorf("old-style id skipped: %+v", res)
	}
}

func TestReindexPaper_PreflightNoLatex(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: false, preflightReason: "no_source"}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00001", OldDocID: "old"})
	if !res.Skipped || !strings.HasPrefix(res.SkipReason, "preflight_no_latex:") {
		t.Errorf("result = %+v, want preflight_no_latex skip", res)
	}
	if len(stubs.chunkedDocIDs) != 0 {
		t.Error("prepare path must not run after preflight skip")
	}
}

func TestReindexPaper_PrepareNotOK(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: false, prepareTier: "latex"}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00002", OldDocID: "old"})
	if !res.Skipped || res.SkipReason != "prepare_not_ok" {
		t.Errorf("result = %+v", res)
	}
}

func TestReindexPaper_NonLatexTierSkipped(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: true, prepareTier: "ar5iv"}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00003", OldDocID: "old"})
	if !res.Skipped || res.SkipReason != "not_latex" {
		t.Errorf("result = %+v", res)
	}
	if res.SourceTier != "ar5iv" {
		t.Errorf("SourceTier = %q, want ar5iv", res.SourceTier)
	}
	if len(stubs.chunkedDocIDs) != 0 || len(stubs.rewired) != 0 {
		t.Error("non-latex paper must not be persisted or rewired")
	}
}

func TestReindexPaper_DryRunStopsBeforePersist(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: true, prepareTier: "latex"}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00004", OldDocID: "old", DryRun: true})
	if res.Skipped {
		t.Errorf("dry run must not be Skipped: %+v", res)
	}
	if res.NewDocID == "" {
		t.Error("dry run should still mint a new doc id")
	}
	if len(stubs.chunkedDocIDs) != 0 || len(stubs.rewired) != 0 || len(stubs.deleted) != 0 {
		t.Error("dry run must not persist, rewire, or delete")
	}
}

func TestReindexPaper_HappyPath(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: true, prepareTier: "latex", prepareHasFigures: true}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00005", OldDocID: "old-doc"})
	if res.Skipped {
		t.Fatalf("happy path skipped: %+v", res)
	}
	if res.NewDocID == "" || res.SourceTier != "latex" || res.SizeBytes != 1234 {
		t.Errorf("result = %+v", res)
	}
	if res.FigureCount != 3 {
		t.Errorf("FigureCount = %d, want 3", res.FigureCount)
	}
	if len(stubs.chunkedDocIDs) != 1 || stubs.chunkedDocIDs[0] != res.NewDocID {
		t.Errorf("chunked = %v, want [%s]", stubs.chunkedDocIDs, res.NewDocID)
	}
	if len(stubs.rewired) != 1 || stubs.rewired[0] != [2]string{"old-doc", res.NewDocID} {
		t.Errorf("rewired = %v", stubs.rewired)
	}
	if len(stubs.staleDeleted) != 1 || stubs.staleDeleted[0] != "old-doc" {
		t.Errorf("stale figures deleted for %v, want [old-doc]", stubs.staleDeleted)
	}
	if len(stubs.deleted) != 1 || stubs.deleted[0] != "old-doc" {
		t.Errorf("R2R deletes = %v, want [old-doc]", stubs.deleted)
	}
	if len(stubs.skipReasons) != 0 {
		t.Errorf("skip records = %v, want none", stubs.skipReasons)
	}
}

func TestReindexPaper_NoFiguresSkipsFigureExtraction(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: true, prepareTier: "latex", prepareHasFigures: false}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00006", OldDocID: "old"})
	if res.Skipped || res.FigureCount != 0 {
		t.Errorf("result = %+v", res)
	}
	if len(stubs.figureDocIDs) != 0 {
		t.Errorf("figure extraction ran: %v", stubs.figureDocIDs)
	}
}

func TestReindexPaper_RewireNoOpMapsToSkipped(t *testing.T) {
	stubs := &paperStubs{preflightHasLatex: true, prepareOK: true, prepareTier: "latex", rewireNoOp: true}
	res := runPaper(t, stubs, ReindexArxivPaperInput{ArxivID: "2101.00007", OldDocID: "old"})
	if !res.Skipped || res.SkipReason != "rewire_no_op" {
		t.Fatalf("result = %+v, want Skipped/rewire_no_op", res)
	}
	// Cleanup: the freshly-minted NEW doc gets deleted, not the old one.
	if len(stubs.deleted) != 1 || stubs.deleted[0] != res.NewDocID {
		t.Errorf("deleted = %v, want [%s]", stubs.deleted, res.NewDocID)
	}
}

func TestExtractCorpusWorkflow_RequiresCorpusID(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ExtractCorpusWorkflow)

	env.ExecuteWorkflow(ExtractCorpusWorkflow, ExtractCorpusInput{})
	if !env.IsWorkflowCompleted() {
		t.Fatal("not completed")
	}
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected BadInput error for missing corpus_id")
	}
	var appErr *temporal.ApplicationError
	if !temporalAsApplicationError(err, &appErr) || appErr.Type() != "BadInput" {
		t.Fatalf("error = %v, want non-retryable BadInput", err)
	}
}

// temporalAsApplicationError unwraps a workflow-execution error chain
// down to *temporal.ApplicationError.
func temporalAsApplicationError(err error, target **temporal.ApplicationError) bool {
	for err != nil {
		if ae, ok := err.(*temporal.ApplicationError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

package workflows

import (
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// arxivIDPattern matches both the new-style IDs (YYMM.NNNNN[vN]) and
// the older "category/NNNNNNN" form. The task description says
// `^[0-9]+\.[0-9]+(v[0-9]+)?$` — we keep both branches because the
// bash script accepted both on lines 75-76, and rejecting the old form
// here would orphan pre-2007 arxiv documents that already live in the
// corpus.
var arxivIDPattern = regexp.MustCompile(`^([A-Za-z\-\.]+/[0-9]{7}|[0-9]+\.[0-9]+(v[0-9]+)?)$`)

// ReindexArxivPaperWorkflow handles a single arxiv paper end-to-end.
// Mirrors the bash script body on lines 64-173, one activity per phase:
//
//  1. Validate the arxiv id (SideEffect — no network)
//  2. Generate the new document UUID (SideEffect — non-deterministic)
//  3. IngestArxivPaper — POST /platform/sources/ingest
//  4. If source_tier != "latex": DeleteR2RDocument(new) + skip
//  5. RewireCorpusDocument — UPDATE platform.corpus_documents
//  6. DeleteR2RDocument(old) — best-effort cleanup
//
// Returns a PaperResult either way. Skipped=true covers the bash
// script's "skip_invalid" / "no_latex" cases; Failed (an error return)
// covers the cases the parent will count against CorpusProgress.Failed.
func ReindexArxivPaperWorkflow(ctx workflow.Context, in ReindexArxivPaperInput) (*PaperResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("ReindexArxivPaperWorkflow start",
		"arxiv", in.ArxivID,
		"old_doc", in.OldDocID,
		"dry_run", in.DryRun,
	)

	res := &PaperResult{ArxivID: in.ArxivID}

	// Step 1: validate arxiv id. Regex matching is deterministic, so
	// no SideEffect wrapper is needed — wrapping bloats workflow history
	// for no upside.
	if !arxivIDPattern.MatchString(in.ArxivID) {
		res.Skipped = true
		res.SkipReason = "invalid_arxiv_id"
		logger.Info("paper skipped: invalid arxiv id", "arxiv", in.ArxivID)
		recordSkip(ctx, in.ArxivID, "invalid_arxiv_id", logger)
		return res, nil
	}

	// Step 1b: pre-flight against the arxiv-skip sidecar. If the
	// sidecar says "no LaTeX upstream", short-circuit here — saves
	// ~30s of full backend round-trip per skipped paper. The activity
	// fails OPEN: a sidecar error returns has_latex=true so the slow
	// path still runs, preserving the authoritative behaviour.
	preflightCtx := workflow.WithActivityOptions(ctx, preflightOptions())
	var pre PreflightResult
	if err := workflow.ExecuteActivity(preflightCtx, "PreflightArxiv", PreflightInput{ArxivID: in.ArxivID}).Get(preflightCtx, &pre); err != nil {
		// Activity itself failed (all retries exhausted) — let the
		// downstream ingest call provide the truth. Don't block the
		// paper on a sidecar outage.
		logger.Warn("PreflightArxiv failed; falling back to full ingest path",
			"arxiv", in.ArxivID,
			"error", err,
		)
	} else if !pre.HasLatex {
		res.Skipped = true
		res.SkipReason = "preflight_no_latex:" + pre.Reason
		logger.Info("paper skipped: preflight reports no latex",
			"arxiv", in.ArxivID,
			"reason", pre.Reason,
		)
		recordSkip(ctx, in.ArxivID, res.SkipReason, logger)
		return res, nil
	}

	// Step 2: generate new doc UUID via SideEffect. uuid.New() reads
	// from crypto/rand, which is non-deterministic — SideEffect captures
	// the value once in history so replays don't generate a new UUID.
	var newDocID string
	uuidOut := workflow.SideEffect(ctx, func(ctx workflow.Context) interface{} {
		return uuid.New().String()
	})
	if err := uuidOut.Get(&newDocID); err != nil {
		return res, err
	}
	res.NewDocID = newDocID

	// Step 3: prepare (arxiv fetch + parse only, no persist). The retry
	// policy here is unlimited (MaxAttempts=0,
	// 5s/2x/120s backoff, ScheduleToClose=24h) so a backend outage
	// during deploy turns into a quiet pause rather than a paper-burning
	// failure. Each attempt fails fast on `dial connection refused`
	// (sub-second), so retries effectively become a "ping the backend
	// periodically" pattern using the real ingest endpoint as the
	// canary — no separate health endpoint needed, no extra load on
	// an already-saturated backend.
	prepareCtx := workflow.WithActivityOptions(ctx, ingestActivityOptions())
	type prepareInputT struct {
		ArxivID  string `json:"arxiv_id"`
		NewDocID string `json:"new_doc_id"`
	}
	type prepareResultT struct {
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
	var prep prepareResultT
	err := workflow.ExecuteActivity(prepareCtx, "PrepareArxivDocument", prepareInputT{
		ArxivID:  in.ArxivID,
		NewDocID: newDocID,
	}).Get(prepareCtx, &prep)
	if err != nil {
		logger.Warn("PrepareArxivDocument failed", "arxiv", in.ArxivID, "error", err)
		return res, err
	}
	res.SourceTier = prep.SourceTier
	res.SizeBytes = prep.SizeBytes

	if !prep.OK {
		// r2g couldn't fetch/parse the paper. Nothing was persisted, so
		// (unlike the old ingest path) there's no doc to clean up; mark
		// Skipped so the parent counts it once.
		res.Skipped = true
		res.SkipReason = "prepare_not_ok"
		logger.Info("paper skipped: prepare reported not-ok",
			"arxiv", in.ArxivID,
			"detail", prep.Detail,
		)
		recordSkip(ctx, in.ArxivID, "prepare_not_ok", logger)
		return res, nil
	}

	delCtx := workflow.WithActivityOptions(ctx, deleteActivityOptions())

	// Step 4: non-latex tiers (ar5iv/vanity/pdf) aren't reindexed — the
	// bash original dropped them. /prepare didn't persist anything, so
	// unlike the old ingest path there's no freshly-created doc to delete.
	if prep.SourceTier != "latex" {
		res.Skipped = true
		res.SkipReason = "not_latex"
		logger.Info("paper skipped: source_tier != latex",
			"arxiv", in.ArxivID,
			"tier", prep.SourceTier,
		)
		recordSkip(ctx, in.ArxivID, "not_latex", logger)
		return res, nil
	}

	// Dry run stops here: fetch + parse + tier check happened, but we
	// don't persist, rewire, or delete anything.
	if in.DryRun {
		logger.Info("dry-run: prepared but not persisting",
			"arxiv", in.ArxivID,
			"tier", prep.SourceTier,
		)
		return res, nil
	}

	// Step 4b: chunk + embed + persist ON THE WORKER NODE. The embedding
	// step is now a Temporal activity (per-batch heartbeat + retry,
	// visible in history) instead of synchronous work inside the r2g web
	// process. Vectors are produced and persisted in-process, so nothing
	// large crosses an activity boundary.
	cepCtx := workflow.WithActivityOptions(ctx, chunkEmbedPersistOptions())
	type chunkEmbedInputT struct {
		DocumentID string         `json:"document_id"`
		Title      string         `json:"title"`
		DocType    string         `json:"doctype"`
		Text       string         `json:"text"`
		SizeBytes  int64          `json:"size_bytes"`
		Metadata   map[string]any `json:"metadata,omitempty"`
	}
	type chunkEmbedResultT struct {
		Chunks      int    `json:"chunks"`
		TotalTokens int    `json:"total_tokens"`
		Model       string `json:"model"`
	}
	var cep chunkEmbedResultT
	if err := workflow.ExecuteActivity(cepCtx, "ChunkEmbedPersist", chunkEmbedInputT{
		DocumentID: newDocID,
		Title:      prep.Title,
		DocType:    prep.DocType,
		Text:       prep.Text,
		SizeBytes:  prep.SizeBytes,
		Metadata:   prep.Metadata,
	}).Get(cepCtx, &cep); err != nil {
		logger.Warn("ChunkEmbedPersist failed", "arxiv", in.ArxivID, "error", err)
		return res, err
	}
	logger.Info("paper chunk+embed+persist done",
		"arxiv", in.ArxivID,
		"chunks", cep.Chunks,
		"model", cep.Model,
	)

	// Step 4c: figures (eprint only, best-effort). r2g re-fetches the
	// tarball and runs the MuPDF extractor; a failure here doesn't fail
	// the paper since the document + chunks are already committed.
	if prep.HasFigures {
		figCtx := workflow.WithActivityOptions(ctx, figuresActivityOptions())
		type figuresInputT struct {
			ArxivID    string `json:"arxiv_id"`
			DocumentID string `json:"document_id"`
		}
		type figuresResultT struct {
			Figures int `json:"figures"`
		}
		var fig figuresResultT
		if err := workflow.ExecuteActivity(figCtx, "ExtractArxivFigures", figuresInputT{
			ArxivID:    in.ArxivID,
			DocumentID: newDocID,
		}).Get(figCtx, &fig); err != nil {
			logger.Warn("ExtractArxivFigures failed (best-effort)",
				"arxiv", in.ArxivID,
				"error", err,
			)
		} else {
			res.FigureCount = fig.Figures
		}
	}

	// Step 5: swap the corpus pointer. This is the only state change
	// that the dashboard depends on for the new doc to become visible
	// in search.
	rewireCtx := workflow.WithActivityOptions(ctx, rewireActivityOptions())
	type rewireInputT struct {
		OldDocID string `json:"old_doc_id"`
		NewDocID string `json:"new_doc_id"`
	}
	if err := workflow.ExecuteActivity(rewireCtx, "RewireCorpusDocument", rewireInputT{
		OldDocID: in.OldDocID,
		NewDocID: newDocID,
	}).Get(rewireCtx, nil); err != nil {
		// A "RewireNoOp" non-retryable error means the corpus pointer
		// has already moved (typically a concurrent reindex). Surface
		// it as a Skipped result so the parent counts it correctly
		// rather than burning a Failed slot.
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && appErr.Type() == "RewireNoOp" {
			res.Skipped = true
			res.SkipReason = "rewire_no_op"
			logger.Info("paper skipped: rewire matched zero rows",
				"arxiv", in.ArxivID,
				"old_doc", in.OldDocID,
				"new_doc", newDocID,
			)
			// Clean up the freshly-ingested doc so we don't orphan it.
			if delErr := workflow.ExecuteActivity(delCtx, "DeleteR2RDocument", newDocID).Get(delCtx, nil); delErr != nil {
				logger.Warn("DeleteR2RDocument(new) failed during rewire-no-op cleanup",
					"new_doc", newDocID,
					"error", delErr,
				)
			}
			return res, nil
		}
		logger.Error("RewireCorpusDocument failed", "arxiv", in.ArxivID, "error", err)
		return res, err
	}

	// Step 6: delete the old R2R doc. Best-effort — the bash version
	// used `curl ... || true` for this step (line 142). We still
	// return the error if every retry attempt fails, so a systemic
	// platform problem surfaces in the workflow result instead of
	// being swept under the rug. Bash's old-doc orphan accumulates
	// silently; Temporal will report.
	if err := workflow.ExecuteActivity(delCtx, "DeleteR2RDocument", in.OldDocID).Get(delCtx, nil); err != nil {
		// Don't fail the whole paper for this — the swap already
		// landed and search will work. Skipped stays false so the
		// corpus workflow counts this as Done; the Warn below is the
		// audit trail.
		logger.Warn("DeleteR2RDocument(old) failed",
			"old_doc", in.OldDocID,
			"error", err,
		)
	}

	logger.Info("paper reindexed",
		"arxiv", in.ArxivID,
		"new_doc", newDocID,
		"figs", res.FigureCount,
	)
	return res, nil
}

// ingestActivityOptions tunes IngestArxivPaper. 240s StartToClose per
// attempt matches the bash script's curl -m 240. HeartbeatTimeout=30s
// catches stuck workers (the activity heartbeats every 10s).
//
// Retry policy is **unlimited** (MaximumAttempts=0) with a 120s cap on
// the backoff interval and a 24h ScheduleToClose. Rationale: a transient
// backend outage (e.g. deploy restart, OOM kill) should NOT burn papers
// out of the work-set. Each retry attempt fails fast on `dial connection
// refused` (<1s), so unlimited retries effectively implement the "ping
// periodically and resume" pattern using the real endpoint as the canary.
// When the backend comes back, the next retry succeeds.
//
// The 24h ScheduleToClose is the only hard upper bound — long enough to
// ride out any realistic outage, short enough that a permanently-broken
// paper doesn't sit in flight forever blocking its workflow-id slot.
func ingestActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		// 600s lines up with the upstream 300s ceilings (platform httpx
		// + r2g embed HTTP client) plus headroom for parser + figures
		// + corpus rewire. The earlier 240s was the *tightest* link in
		// the chain and was firing context-cancellations into r2g
		// mid-embed under BGE contention, which Temporal then retried
		// (amplifying the contention). 600s gives the pipeline room to
		// drain a slow batch without falsely cancelling.
		StartToCloseTimeout:    600 * time.Second,
		ScheduleToCloseTimeout: 24 * time.Hour,
		HeartbeatTimeout:       30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    120 * time.Second,
			MaximumAttempts:    0,
		},
	}
}

// chunkEmbedPersistOptions tunes ChunkEmbedPersist — the worker-side
// chunk + embed + persist activity. 600s StartToClose covers a large paper
// embedded in 64-chunk batches against a contended BGE sidecar; the
// activity heartbeats every 15s so the 60s HeartbeatTimeout catches a
// genuinely stuck worker. Retry profile mirrors skills-rae's EmbedBatch
// (5 attempts, 1s→30s) — the whole activity retries idempotently because
// store.InsertDocumentWithChunks clears stale chunks for the doc id first.
func chunkEmbedPersistOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 600 * time.Second,
		HeartbeatTimeout:    60 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
}

// figuresActivityOptions tunes ExtractArxivFigures. Best-effort and
// eprint-only: r2g re-fetches the tarball and runs MuPDF, so 300s is ample.
// 3 attempts because a failure is non-fatal (the doc + chunks are already
// committed); we don't want to spin forever on a flaky figure extract.
func figuresActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 300 * time.Second,
		HeartbeatTimeout:    60 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// deleteActivityOptions tunes DeleteR2RDocument. Cheap call — 30s
// budget is plenty. MaxAttempts=5 because the call is idempotent
// (404 is already treated as success) and retrying is harmless.
func deleteActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
		},
	}
}

// rewireActivityOptions tunes RewireCorpusDocument. Single UPDATE
// against a small table — 30s is more than enough. MaxAttempts=3 to
// ride out a brief postgres restart.
func rewireActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// markSkippedOptions tunes MarkArxivSkipped. The activity is a single
// upsert (idempotent thanks to ON CONFLICT DO UPDATE), so 30s/3 retries
// is plenty. A retry that double-records would just bump skip_count by
// one extra, which is observable but not destructive.
func markSkippedOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// markSkippedInput mirrors activities.MarkSkippedInput. Duplicated here
// to avoid a workflows → activities import (the activities package
// imports this one for ArxivPaper, so the reverse would cycle).
type markSkippedInput struct {
	ArxivID string `json:"arxiv_id"`
	Reason  string `json:"reason"`
}

// PreflightInput / PreflightResult mirror the activities-side types of
// the same name. The duplication is the same cycle-avoidance dodge as
// markSkippedInput; activities.PreflightArxiv is invoked by string and
// the JSON shapes must agree.
type PreflightInput struct {
	ArxivID string `json:"arxiv_id"`
}

type PreflightResult struct {
	HasLatex bool   `json:"has_latex"`
	Reason   string `json:"reason"`
}

// preflightOptions tunes the PreflightArxiv activity. 30s StartToClose
// is plenty — the sidecar's own timeout is 5s and its HTTP client
// adds another ~10s. MaximumAttempts=2 with a short backoff because
// the activity itself fails open (returns has_latex=true on sidecar
// errors); the retry only catches transient gRPC/heartbeat hiccups
// between worker and Temporal.
func preflightOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    15 * time.Second,
			MaximumAttempts:    2,
		},
	}
}

// recordSkip fires the MarkArxivSkipped activity synchronously and
// folds its failure into a Warn log. We block on Get(...) rather than
// fire-and-forget so the activity is guaranteed to execute before the
// child workflow completes — otherwise the per-paper workflow would
// finish before its skip record landed and the parent's ContinueAsNew
// could re-query the work-set before evo.reindex_skipped saw the row.
// The retry policy caps total time at ~3*30s = 90s, well under the
// per-paper budget.
func recordSkip(ctx workflow.Context, arxivID, reason string, logger log.Logger) {
	markCtx := workflow.WithActivityOptions(ctx, markSkippedOptions())
	err := workflow.ExecuteActivity(markCtx, "MarkArxivSkipped", markSkippedInput{
		ArxivID: arxivID,
		Reason:  reason,
	}).Get(markCtx, nil)
	if err != nil {
		logger.Warn("MarkArxivSkipped failed; paper will be re-attempted next pass",
			"arxiv", arxivID,
			"reason", reason,
			"error", err,
		)
	}
}

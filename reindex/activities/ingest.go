package activities

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// IngestActivities groups the HTTP-bound activities. Two base URLs
// because the strangler-fig cutover (2026-05-14) split the surface:
//
//   - IngestArxivPaper → POST {PlatformURL}/platform/sources/ingest
//     (still served by the Python platform wrapper)
//   - DeleteR2RDocument → DELETE {R2GURL}/v3/documents/{id}
//   - ExtractEntities  → POST   {R2GURL}/v3/documents/{id}/extract
//     (both served by r2g)
//
// If R2GURL is empty, the activities fall back to PlatformURL so old
// deployments without the cutover continue to work unchanged.
//
// The struct holds the HTTPClient + base URLs + logger so each call
// reuses the same connection pool. TLS-insecure mode is the caller's
// responsibility: registrations.go inspects REINDEX_TLS_INSECURE and
// builds an HTTPClient with InsecureSkipVerify when set.
type IngestActivities struct {
	HTTPClient  *http.Client
	PlatformURL string
	R2GURL      string
	Logger      *logrus.Logger
	// SkipClient is the optional arxiv-skip sidecar client. When nil
	// (env var ARXIV_SKIP_URL unset), the PreflightArxiv activity
	// returns has_latex=true unconditionally so the caller falls
	// through to the existing slow path — preflight is opt-in,
	// fail-open.
	SkipClient ArxivSkipClient
}

// PreflightArxiv asks the arxiv-skip sidecar whether this paper has
// LaTeX source. Returns (has_latex, reason). If the sidecar is
// unconfigured or returns an error, returns has_latex=true so the
// caller falls through to the existing slow path — fail OPEN. The
// sidecar's only job is to save work, never to block it.
func (a *IngestActivities) PreflightArxiv(ctx context.Context, in PreflightInput) (*PreflightResult, error) {
	if a.SkipClient == nil {
		return &PreflightResult{HasLatex: true, Reason: "preflight disabled"}, nil
	}
	has, reason, err := a.SkipClient.CheckHasLatex(ctx, in.ArxivID)
	if err != nil {
		// Sidecar transient failure — fail open and let the full path
		// run. The activity returns a non-error result so Temporal
		// doesn't burn retries on the sidecar's behalf; the slow path
		// will provide the authoritative answer.
		if a.Logger != nil {
			a.Logger.WithError(err).WithField("arxiv", in.ArxivID).
				Warn("PreflightArxiv: sidecar error; failing open")
		}
		return &PreflightResult{HasLatex: true, Reason: "preflight error: " + err.Error()}, nil
	}
	return &PreflightResult{HasLatex: has, Reason: reason}, nil
}

// NewHTTPClient builds an http.Client honouring the insecure-TLS flag.
// The 20-minute ceiling is sized for the longest call in the suite —
// ExtractEntities, which runs synchronously inside R2R under
// orchestration.provider="simple" and can spend several minutes
// per paper on LLM-driven graph extraction. Shorter activities
// (Ingest=240s, Delete=30s) get their effective timeout from Temporal's
// per-activity StartToCloseTimeout, which cancels the request context
// long before this ceiling matters.
func NewHTTPClient(insecure bool) *http.Client {
	t := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if insecure {
		// nolint:gosec — opt-in via REINDEX_TLS_INSECURE; the platform
		// on the Pi serves a self-signed cert.
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Timeout:   20 * time.Minute,
		Transport: t,
	}
}

// IngestInput is the per-paper ingest payload. Mirrors the curl body on
// line 95 of the bash script: a target string ("arxiv:<id>") plus a
// pre-assigned new document id.
type IngestInput struct {
	ArxivID  string `json:"arxiv_id"`
	NewDocID string `json:"new_doc_id"`
	DryRun   bool   `json:"dry_run,omitempty"`
}

// IngestResult is what the activity returns to the workflow. Fields
// mirror the JSON the platform's /platform/sources/ingest endpoint
// emits — the bash script ran four separate python -c json.load
// invocations to pull each one out (lines 97-151); we parse once.
type IngestResult struct {
	OK          bool   `json:"ok"`
	SourceTier  string `json:"source_tier"`
	FigureCount int    `json:"figure_count"`
	SizeBytes   int64  `json:"size_bytes"`
	Detail      string `json:"detail,omitempty"`
}

// ingestResponseRaw mirrors the platform JSON. Two size keys exist in
// the wild ("size" in the older R2R surface, "size_bytes" in the newer
// platform); we accept either.
type ingestResponseRaw struct {
	OK          bool            `json:"ok"`
	SourceTier  string          `json:"source_tier"`
	FigureCount int             `json:"figure_count"`
	SizeBytes   int64           `json:"size_bytes"`
	Size        int64           `json:"size"`
	Detail      json.RawMessage `json:"detail,omitempty"`
}

// IngestArxivPaper POSTs to {PlatformURL}/platform/sources/ingest with
// the bash script's payload shape. Long-running: the platform pipeline
// runs LaTeX parse + chunk + embed + figure extract synchronously
// inside this call, so we heartbeat every 10s to stay alive against
// the workflow's 30s HeartbeatTimeout.
//
// Returns ok=false when the platform reports a fetch failure ("paper
// not on arxiv", "tarball missing"). The workflow treats ok=false as
// a non-fatal skip; we still return nil error from the activity so
// Temporal doesn't burn retries on a paper the upstream simply
// doesn't have.
//
// Genuinely permanent errors (the platform returns 4xx with a "not on
// arxiv" detail) are surfaced via temporal.NewNonRetryableApplicationError
// so the retry budget isn't wasted.
func (a *IngestActivities) IngestArxivPaper(ctx context.Context, in IngestInput) (*IngestResult, error) {
	log := a.logger()
	if in.ArxivID == "" || in.NewDocID == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"ingest: arxiv_id and new_doc_id required", "BadInput", nil,
		)
	}

	if in.DryRun {
		log.WithFields(logrus.Fields{
			"arxiv":   in.ArxivID,
			"new_doc": in.NewDocID,
		}).Info("IngestArxivPaper: dry-run, skipping platform POST")
		return &IngestResult{OK: true, SourceTier: "latex"}, nil
	}

	stop := startHeartbeat(ctx, 10*time.Second, in.ArxivID)
	defer stop()

	// /platform/sources/ingest moved from Python backend to r2g on
	// 2026-05-15. Use r2gBase so deployments with the legacy backend
	// route still work via the PlatformURL fallback.
	url := strings.TrimRight(a.r2gBase(), "/") + "/platform/sources/ingest"
	body, err := json.Marshal(map[string]string{
		"target":      "arxiv:" + in.ArxivID,
		"document_id": in.NewDocID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal ingest payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build ingest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// 4xx classification:
	//   - 400/401/403/404/410/422 are permanent client-side errors —
	//     retrying the same arxiv id won't change the upstream's answer,
	//     so mark NonRetryable to free the retry budget.
	//   - 5xx is treated as transient (the platform pipeline crashes or
	//     restarts) so the unlimited retry loop covers deploy windows.
	//   - Any other 4xx is treated as non-retryable (safe default for
	//     codes we don't explicitly classify).
	switch {
	case resp.StatusCode == 400, resp.StatusCode == 401, resp.StatusCode == 403,
		resp.StatusCode == 404, resp.StatusCode == 410, resp.StatusCode == 422:
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("ingest %d: %s", resp.StatusCode, tail(string(respBody), 200)),
			fmt.Sprintf("HTTP%d", resp.StatusCode), nil,
		)
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		// Retryable — backend transient issue (5xx, including 502/503/504).
		return nil, fmt.Errorf("ingest %d (transient): %s", resp.StatusCode, tail(string(respBody), 200))
	case resp.StatusCode >= 400:
		// Other 4xx — treat as non-retryable.
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("ingest %d: %s", resp.StatusCode, tail(string(respBody), 200)),
			fmt.Sprintf("HTTP%d", resp.StatusCode), nil,
		)
	}

	var raw ingestResponseRaw
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("parse ingest response: %w (body=%s)", err, tail(string(respBody), 200))
	}

	out := &IngestResult{
		OK:          raw.OK,
		SourceTier:  raw.SourceTier,
		FigureCount: raw.FigureCount,
		SizeBytes:   raw.SizeBytes,
	}
	if out.SizeBytes == 0 && raw.Size > 0 {
		out.SizeBytes = raw.Size
	}
	if len(raw.Detail) > 0 {
		// detail may be a JSON string or an object; flatten to a string
		// either way so the workflow can log it without re-marshaling.
		var s string
		if json.Unmarshal(raw.Detail, &s) == nil {
			out.Detail = s
		} else {
			out.Detail = string(raw.Detail)
		}
	}

	log.WithFields(logrus.Fields{
		"arxiv":  in.ArxivID,
		"ok":     out.OK,
		"tier":   out.SourceTier,
		"figs":   out.FigureCount,
		"size":   out.SizeBytes,
	}).Info("IngestArxivPaper: platform ingest finished")
	return out, nil
}

// ExtractEntities POSTs to {PlatformURL}/v3/documents/{id}/extract to
// kick off R2R's graph extraction pipeline (entities + relationships).
// R2R runs the extraction asynchronously via Hatchet, so the HTTP call
// itself returns in ~1s; we just need to trigger it.
//
// Idempotent — R2R deduplicates by document_id, so re-triggering an
// already-extracted document is a no-op. Treats 404 as success: the
// doc was probably deleted between rewire and extract (e.g. rewire
// no-op + delete-new), and forcing the workflow to retry isn't useful.
func (a *IngestActivities) ExtractEntities(ctx context.Context, docID string) error {
	log := a.logger()
	if docID == "" {
		return temporal.NewNonRetryableApplicationError(
			"extract: doc_id required", "BadInput", nil,
		)
	}

	url := strings.TrimRight(a.r2gBase(), "/") + "/v3/documents/" + docID + "/extract"
	body := []byte(`{"run_with_orchestration":true}`)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build extract request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == 404:
		log.WithField("doc_id", docID).Warn("ExtractEntities: 404 (doc gone)")
		return nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		log.WithField("doc_id", docID).Debug("ExtractEntities: triggered")
		return nil
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return fmt.Errorf("extract %s: HTTP %d (transient): %s", docID, resp.StatusCode, tail(string(respBody), 200))
	default:
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("extract %d: %s", resp.StatusCode, tail(string(respBody), 200)),
			fmt.Sprintf("HTTP%d", resp.StatusCode), nil,
		)
	}
}

// DeleteR2RDocument issues a DELETE against the v3 documents endpoint.
// Treats 404 as a success — the bash version did the same with
// `curl ... || true` on lines 127 and 142. Other 4xx/5xx surface as
// retryable errors so a flaky platform request can be retried.
func (a *IngestActivities) DeleteR2RDocument(ctx context.Context, docID string) error {
	log := a.logger()
	if docID == "" {
		return temporal.NewNonRetryableApplicationError(
			"delete: doc_id required", "BadInput", nil,
		)
	}

	url := strings.TrimRight(a.r2gBase(), "/") + "/v3/documents/" + docID
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == 404:
		// Already gone — treat as success.
		log.WithField("doc_id", docID).Debug("DeleteR2RDocument: 404 (already gone)")
		return nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		log.WithField("doc_id", docID).Debug("DeleteR2RDocument: ok")
		return nil
	default:
		return fmt.Errorf("delete %s: HTTP %d (%s)", docID, resp.StatusCode, tail(string(body), 200))
	}
}

func (a *IngestActivities) logger() *logrus.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return logrus.New()
}

// r2gBase returns the base URL for /v3/* calls. Defaults to PlatformURL
// for compatibility with deployments that haven't separated r2g from
// the Python backend.
func (a *IngestActivities) r2gBase() string {
	if a.R2GURL != "" {
		return a.R2GURL
	}
	return a.PlatformURL
}

// startHeartbeat fires activity.RecordHeartbeat at the given interval
// until the returned stop func is called. Mirrors the heartbeat-pump
// pattern in temporal/activities/delegation_activities.go:178.
func startHeartbeat(ctx context.Context, interval time.Duration, details ...interface{}) func() {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx, details...)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

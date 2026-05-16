// Package activities holds the Temporal activity implementations for
// the skills subsystem. The R2R mirror lives here (ported from the
// dashboard's fire-and-forget goroutines) plus, in later phases, the
// GitHub importer and the daily check-source drift detector.
package activities

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamomaton-knowledge/skills/workflows"
)

// R2RActivities groups the activity methods that talk to the platform
// R2R surface. Dependencies are injected once at worker startup so
// every invocation reuses the same HTTP client and config — much
// cheaper than the per-call client construction the dashboard's
// goroutines used.
type R2RActivities struct {
	HTTPClient *http.Client
	BaseURL    string
	CorpusID   string
	Insecure   bool
	Logger     *logrus.Logger
}

// NewHTTPClient builds a transport that honours the SKILLS_R2R_INSECURE
// flag and caps the request lifetime. Mirrors
// dashboard/apiserver/skills_r2r_sync.go:159-168 — kept as a helper so
// main.go can construct an R2RActivities without re-implementing the
// TLS opt-in dance.
func NewHTTPClient(insecure bool) *http.Client {
	c := &http.Client{Timeout: 90 * time.Second}
	if insecure {
		c.Transport = &http.Transport{
			// nolint:gosec — opt-in via SKILLS_R2R_INSECURE for local dev only.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return c
}

// PostSkillToR2R is the activity body ported from
// dashboard/apiserver/skills_r2r_sync.go:107-153. The payload shape and
// /platform/skills/ingest endpoint are unchanged; the only structural
// edit is dependency injection (HTTPClient/BaseURL/CorpusID off the
// receiver instead of env-var globals) and the Temporal heartbeat used
// for visibility while a stuck POST is still inside its 90s deadline.
func (a *R2RActivities) PostSkillToR2R(ctx context.Context, in workflows.SyncSkillToR2RInput) error {
	// Heartbeat once at entry so an in-flight attempt is visible in the
	// Temporal UI even before the HTTP call returns. The activity is
	// short enough that further heartbeats aren't useful.
	activity.RecordHeartbeat(ctx, nil)

	sk := in.Skill
	payload := map[string]interface{}{
		"skill_id":    sk.ID,
		"name":        sk.Name,
		"description": sk.Description,
		"body":        sk.Body,
		"tags":        sk.Tags,
		"depends_on":  sk.DependsOn,
	}
	if sk.WhenToUse != nil {
		payload["when_to_use"] = *sk.WhenToUse
	}
	if sk.Example != nil {
		payload["example"] = *sk.Example
	}
	if sk.Community != nil {
		payload["community"] = *sk.Community
	}
	if a.CorpusID != "" {
		payload["corpus_id"] = a.CorpusID
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal skill payload: %w", err)
	}

	url := fmt.Sprintf("%s/platform/skills/ingest", a.BaseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	defer drainAndClose(resp.Body)
	if err := classifyR2RStatus("POST", url, resp); err != nil {
		return err
	}
	if a.Logger != nil {
		a.Logger.WithField("skill_id", sk.ID).Debug("R2R skill ingest succeeded")
	}
	return nil
}

// DeleteSkillFromR2R issues a DELETE against /platform/skills/{id}.
// 404 is treated as success — the document is already gone, which is
// the desired end state, and retrying won't change anything. Other
// 4xx/5xx surfaces as a retryable error so Temporal's retry policy
// kicks in.
//
// Ported from dashboard/apiserver/skills_r2r_sync.go:74-105.
func (a *R2RActivities) DeleteSkillFromR2R(ctx context.Context, in workflows.DeleteSkillFromR2RInput) error {
	activity.RecordHeartbeat(ctx, nil)

	url := fmt.Sprintf("%s/platform/skills/%s", a.BaseURL, in.SkillID)
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		if a.Logger != nil {
			a.Logger.WithField("skill_id", in.SkillID).
				Debug("R2R skill delete: already gone (404 treated as success)")
		}
		return nil
	}
	return classifyR2RStatus("DELETE", url, resp)
}

// classifyR2RStatus turns an HTTP response status into the right kind of
// Temporal error: 4xx (other than 429) and "gone" responses become
// NonRetryableApplicationError so the workflow's 20-attempt retry policy
// doesn't waste a half hour pummelling the platform with the same bad
// request. 429 + 5xx stay as plain errors so Temporal retries with backoff.
// Returns nil for 2xx/3xx.
func classifyR2RStatus(method, url string, resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	preview := string(bytes.TrimSpace(body))
	msg := fmt.Sprintf("platform %s %s returned HTTP %d: %s", method, url, resp.StatusCode, preview)
	switch {
	case resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusGone:
		return temporal.NewNonRetryableApplicationError(msg, "NotFound", nil)
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusUnprocessableEntity:
		return temporal.NewNonRetryableApplicationError(msg, "BadRequest", nil)
	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate-limited — retryable, server should recover.
		return fmt.Errorf("%s", msg)
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		// Server-side error — retryable.
		return fmt.Errorf("%s", msg)
	default:
		// All other 4xx — caller bug, treat as non-retryable.
		return temporal.NewNonRetryableApplicationError(msg, "BadRequest", nil)
	}
}

// drainAndClose copies the rest of the response body to discard before
// closing so the underlying TCP connection can be reused by the
// transport's keep-alive pool. Tiny io.Copy on partially-read bodies is
// the difference between hot pool reuse and a fresh TCP+TLS handshake
// per request on long-lived workers.
func drainAndClose(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

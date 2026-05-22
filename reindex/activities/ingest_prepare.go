package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/temporal"
)

// PrepareInput drives PrepareArxivDocument. NewDocID is informational for
// r2g (the prepare endpoint doesn't persist); it's echoed for logging.
type PrepareInput struct {
	ArxivID  string `json:"arxiv_id"`
	NewDocID string `json:"new_doc_id"`
}

// PrepareResult mirrors the r2g POST /platform/sources/prepare response.
// Text is the parsed body the worker will chunk + embed.
type PrepareResult struct {
	OK         bool           `json:"ok"`
	SourceTier string         `json:"source_tier"`
	DocType    string         `json:"doctype"`
	Text       string         `json:"text"`
	Title      string         `json:"title"`
	SizeBytes  int64          `json:"size_bytes"`
	HasFigures bool           `json:"has_figures"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Detail     string         `json:"detail,omitempty"`
}

// PrepareArxivDocument POSTs to {r2g}/platform/sources/prepare to run the
// arxiv fetch cascade + parser (which need pdftotext/latex2text, shipped
// only in the r2g image) and returns the parsed text + tier + metadata
// WITHOUT persisting. The worker then chunks + embeds + persists via
// ChunkEmbedPersist. Error classification matches IngestArxivPaper: 4xx are
// non-retryable, 5xx (incl. the fetch-cascade-exhausted 502) are transient.
func (a *IngestActivities) PrepareArxivDocument(ctx context.Context, in PrepareInput) (*PrepareResult, error) {
	log := a.logger()
	if in.ArxivID == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"prepare: arxiv_id required", "BadInput", nil)
	}

	stop := startHeartbeat(ctx, 10*time.Second, in.ArxivID)
	defer stop()

	url := strings.TrimRight(a.r2gBase(), "/") + "/platform/sources/prepare"
	body, err := json.Marshal(map[string]string{
		"target":      "arxiv:" + in.ArxivID,
		"document_id": in.NewDocID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal prepare payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build prepare request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	// The parsed text rides in this response, so allow a generous read
	// ceiling (a large LaTeX paper can parse to a few hundred KB).
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch {
	case resp.StatusCode == 400, resp.StatusCode == 401, resp.StatusCode == 403,
		resp.StatusCode == 404, resp.StatusCode == 410, resp.StatusCode == 415,
		resp.StatusCode == 422:
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("prepare %d: %s", resp.StatusCode, tail(string(respBody), 200)),
			fmt.Sprintf("HTTP%d", resp.StatusCode), nil)
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return nil, fmt.Errorf("prepare %d (transient): %s", resp.StatusCode, tail(string(respBody), 200))
	case resp.StatusCode >= 400:
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("prepare %d: %s", resp.StatusCode, tail(string(respBody), 200)),
			fmt.Sprintf("HTTP%d", resp.StatusCode), nil)
	}

	var out PrepareResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse prepare response: %w (body=%s)", err, tail(string(respBody), 200))
	}

	log.WithFields(logrus.Fields{
		"arxiv":       in.ArxivID,
		"tier":        out.SourceTier,
		"doctype":     out.DocType,
		"text_len":    len(out.Text),
		"has_figures": out.HasFigures,
	}).Info("PrepareArxivDocument: fetched + parsed")
	return &out, nil
}

// FiguresInput drives ExtractArxivFigures. The document must already be
// persisted (figure rows FK-reference it).
type FiguresInput struct {
	ArxivID    string `json:"arxiv_id"`
	DocumentID string `json:"document_id"`
}

// FiguresResult mirrors the r2g POST /platform/sources/figures response.
type FiguresResult struct {
	Figures int `json:"figures"`
}

// ExtractArxivFigures POSTs to {r2g}/platform/sources/figures, which
// re-fetches the tarball and runs the cgo/MuPDF figure extractor against an
// already-persisted document. Best-effort from the workflow's side, but the
// activity still classifies errors so a transient 5xx is retried.
func (a *IngestActivities) ExtractArxivFigures(ctx context.Context, in FiguresInput) (*FiguresResult, error) {
	log := a.logger()
	if in.ArxivID == "" || in.DocumentID == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"figures: arxiv_id and document_id required", "BadInput", nil)
	}

	stop := startHeartbeat(ctx, 10*time.Second, in.DocumentID)
	defer stop()

	url := strings.TrimRight(a.r2gBase(), "/") + "/platform/sources/figures"
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal figures payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build figures request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// fall through to parse
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return nil, fmt.Errorf("figures %d (transient): %s", resp.StatusCode, tail(string(respBody), 200))
	default:
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("figures %d: %s", resp.StatusCode, tail(string(respBody), 200)),
			fmt.Sprintf("HTTP%d", resp.StatusCode), nil)
	}

	var out FiguresResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("parse figures response: %w (body=%s)", err, tail(string(respBody), 200))
	}
	log.WithFields(logrus.Fields{
		"arxiv":       in.ArxivID,
		"document_id": in.DocumentID,
		"figures":     out.Figures,
	}).Info("ExtractArxivFigures: done")
	return &out, nil
}

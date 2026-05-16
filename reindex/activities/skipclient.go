// Package activities — skipclient.go: thin HTTP client for the
// arxiv-skip sidecar (tools/arxiv-skip). The sidecar HEADs arxiv's
// e-print endpoint to decide whether a paper has LaTeX source, which
// lets us short-circuit ~86% of the reindex backlog before the
// expensive deepresearch ingest pipeline ever runs.
//
// The interface is split out so workflows can take it via a struct
// field and substitute a fake during tests without standing up an HTTP
// server.

package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ArxivSkipClient is the surface the reindex activities use. Implemented
// by httpArxivSkipClient and (in tests) by stubs.
type ArxivSkipClient interface {
	// CheckHasLatex asks the sidecar whether arxivID has LaTeX
	// source. Returns the verdict + a short reason on success. Any
	// non-2xx response from the sidecar (other than the 502 retryable
	// case) surfaces as err so the caller can fail open.
	CheckHasLatex(ctx context.Context, arxivID string) (hasLatex bool, reason string, err error)
}

type httpArxivSkipClient struct {
	base   string
	client *http.Client
}

// NewArxivSkipClient builds a client against the given base URL
// (typically "http://arxiv-skip:9871"). Returns nil when baseURL is
// empty — the caller treats nil as "preflight disabled" and the
// activity returns has_latex=true unconditionally.
func NewArxivSkipClient(baseURL string) ArxivSkipClient {
	if baseURL == "" {
		return nil
	}
	return &httpArxivSkipClient{
		base: baseURL,
		client: &http.Client{
			// Sidecar should answer in <500ms typical; cap at 10s so a
			// hung HEAD against arxiv doesn't lock up the activity any
			// longer than the activity's own StartToCloseTimeout would
			// have. The Go context still wins via NewRequestWithContext.
			Timeout: 10 * time.Second,
		},
	}
}

type checkRequest struct {
	ArxivID string `json:"arxiv_id"`
}

type checkResponse struct {
	HasLatex bool   `json:"has_latex"`
	Reason   string `json:"reason"`
	Error    string `json:"error,omitempty"`
}

// CheckHasLatex POSTs /check and parses the verdict. The sidecar always
// returns JSON; we read up to 64 KiB defensively to avoid wedging on a
// misbehaving upstream.
func (c *httpArxivSkipClient) CheckHasLatex(ctx context.Context, arxivID string) (bool, string, error) {
	body, err := json.Marshal(checkRequest{ArxivID: arxivID})
	if err != nil {
		return false, "", fmt.Errorf("marshal check request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/check", bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("build check request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("POST %s/check: %w", c.base, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		// The sidecar emits 502 on transient upstream failures (arxiv
		// 5xx, timeouts, etc.). Surface as an error so the caller can
		// log and fall through to the slow path.
		return false, "", fmt.Errorf("arxiv-skip upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return false, "", fmt.Errorf("arxiv-skip rejected request: %d", resp.StatusCode)
	}

	var out checkResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return false, "", fmt.Errorf("decode check response: %w", err)
	}
	return out.HasLatex, out.Reason, nil
}

// PreflightInput / PreflightResult are the per-activity payloads. Kept
// at package scope so the workflow can refer to them by name without an
// extra struct-juggle.
type PreflightInput struct {
	ArxivID string `json:"arxiv_id"`
}

type PreflightResult struct {
	HasLatex bool   `json:"has_latex"`
	Reason   string `json:"reason"`
}

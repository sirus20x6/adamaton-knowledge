// Package reranker is skills-rae's HTTP client for the cross-encoder
// rerank sidecar (platform/reranker/, same model the research RAG path
// uses since T1.2). Stage-2 retrieval rerank: after bottom-up returns
// the bi-encoder top-N subunit hits, this client re-scores the (query,
// subunit) pairs with a cross-encoder so the final ordering reflects
// pair-level interactions the bi-encoder embeddings can't see.
//
// Behaviour mirrors r2g/internal/reranker:
//
//	{ "model": "cross-encoder/ms-marco-MiniLM-L-6-v2",
//	  "elapsed_ms": 47,
//	  "ranked": [{"index": 3, "score": 0.92}, ...] }
//
// We only need ``index`` + ``score``; the model identifier and timing
// fields are ignored on the Go side.
package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to the reranker over HTTP. An empty baseURL produces
// a client where Enabled() == false; callers should branch on that
// instead of unconditionally calling Rerank.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client. 30s timeout matches r2g's reranker
// usage -- the ms-marco-MiniLM-L-6-v2 model runs ~3s on Pi CPU for
// a 20-passage batch; the headroom guards against a cold-start
// tokenizer load or a transient sidecar pause.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Enabled reports whether a base URL was provided.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// RankedHit is one (index, score) pair returned by the sidecar.
// ``Index`` references the caller's original passage slice.
type RankedHit struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

type rerankReq struct {
	Query    string   `json:"query"`
	Passages []string `json:"passages"`
	TopK     *int     `json:"top_k,omitempty"`
}

type rerankResp struct {
	Model     string      `json:"model"`
	ElapsedMs int         `json:"elapsed_ms"`
	Ranked    []RankedHit `json:"ranked"`
}

// Rerank scores every (query, passage) pair and returns the ranked
// indices descending. ``topK`` caps the returned slice when > 0; pass
// 0 to receive every passage. ErrDisabled is returned when Enabled()
// would have returned false.
func (c *Client) Rerank(ctx context.Context, query string, passages []string, topK int) ([]RankedHit, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if query == "" {
		return nil, errors.New("reranker: empty query")
	}
	if len(passages) == 0 {
		return nil, nil
	}
	body := rerankReq{Query: query, Passages: passages}
	if topK > 0 {
		body.TopK = &topK
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	url := c.baseURL + "/rerank"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post rerank: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("rerank HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var out rerankResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return out.Ranked, nil
}

// ErrDisabled signals the client has no base URL configured.
var ErrDisabled = errors.New("reranker: client not configured (empty base URL)")

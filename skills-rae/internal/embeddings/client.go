// Package embeddings is a thin OpenAI-compatible /v1/embeddings client
// for the bge-m3 sidecar. Behaviour mirrors r2g/internal/embeddings:
// batch calls, 300s timeout (bge-m3 under load on the Pi can queue
// 60s+), float32 output. No retries — callers run inside Temporal
// activities which provide retries at the activity boundary.
package embeddings

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

// Client wraps the sidecar URL + model name. The HTTP client is owned
// by Client so a hot path can reuse the connection pool — re-creating
// it on every call defeats keep-alive.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// NewClient constructs a client. An empty baseURL is legal — the
// resulting client reports !Enabled() and callers can route around it.
// 300s timeout matches r2g; the embedder is shared and contention can
// push p99 over a minute on the Pi.
func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		model:   model,
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

// Enabled reports whether a base URL was set. Useful for guarding
// startup so a misconfigured deploy fails loudly instead of producing
// zero-vector embeddings.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// Model returns the configured model name, surfaced into logs.
func (c *Client) Model() string { return c.model }

type embedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed batches inputs into a single call. Returns one vector per
// input in the same order. Errors when the response shape diverges
// from the request (different result count) — that's a sidecar bug
// worth surfacing rather than silently truncating.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(embedReq{Model: c.model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	url := c.baseURL + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post embeddings: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("embeddings HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	var out embedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(out.Data) != len(inputs) {
		return nil, fmt.Errorf("embedder returned %d vectors for %d inputs", len(out.Data), len(inputs))
	}
	result := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		result[i] = d.Embedding
	}
	return result, nil
}

// EmbedOne is the single-input convenience. Wraps Embed.
func (c *Client) EmbedOne(ctx context.Context, input string) ([]float32, error) {
	out, err := c.Embed(ctx, []string{input})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("embedder returned no vector")
	}
	return out[0], nil
}

// ErrDisabled is returned when the client has no base URL configured.
var ErrDisabled = errors.New("embeddings: client not configured (empty base URL)")

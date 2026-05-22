// Package embeddings is the typed HTTP client r2g uses to talk to the
// BGE-M3 embedding sidecar (and, in a future phase, to the Octen
// retrieval sidecar). The sidecar speaks the OpenAI /v1/embeddings
// shape, so the same client could front any compatible model.
package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a small HTTP wrapper around an OpenAI-compatible embeddings
// sidecar. The sidecar URL and the model name are configured at
// construction; callers pass strings to Embed and get back 1024-d
// float32 vectors that go straight into pgvector.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// NewClient builds a client. baseURL must already include the scheme
// + host (e.g. http://sidecar:8089). model is the name the sidecar
// expects on its /v1/embeddings — for BGE-M3 that's typically "bge-m3".
//
// The default 300-second timeout is sized for batches that hit the
// BGE sidecar under heavy contention -- when reindex runs 8 concurrent
// ingests, BGE-M3 (CPU-only, OpenMP-fanned) can queue past 60s before
// returning. 300s keeps r2g forgiving rather than firing 502s that
// trigger reindex retry storms.
func NewClient(baseURL, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

// Embed runs one batch of inputs through /v1/embeddings and returns
// one []float32 per input in the same order. Vectors come back as
// float64 in the JSON payload; we narrow to float32 on the way out
// because pgvector stores f32 internally and the trip via f64 was
// wasted precision.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"input": inputs,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embed body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/embeddings: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("/v1/embeddings HTTP %d: %s", resp.StatusCode, tail(string(respBody), 200))
	}
	var raw struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, fmt.Errorf("parse embed response: %w (body: %s)", err, tail(string(respBody), 200))
	}
	if len(raw.Data) != len(inputs) {
		return nil, fmt.Errorf("embed response count mismatch: got %d, want %d", len(raw.Data), len(inputs))
	}
	out := make([][]float32, len(raw.Data))
	for i, d := range raw.Data {
		v := make([]float32, len(d.Embedding))
		for j, x := range d.Embedding {
			v[j] = float32(x)
		}
		out[i] = v
	}
	return out, nil
}

// Model returns the model name the client sends to the sidecar. Callers
// that persist or report which model produced a vector (e.g. the reindex
// ChunkEmbedPersist activity) read it from here so the model string stays
// single-sourced at construction.
func (c *Client) Model() string { return c.model }

// EmbedOne is the single-input convenience wrapper.
func (c *Client) EmbedOne(ctx context.Context, input string) ([]float32, error) {
	out, err := c.Embed(ctx, []string{input})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("/v1/embeddings returned zero vectors")
	}
	return out[0], nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

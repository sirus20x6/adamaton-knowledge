package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// compileRequest mirrors routes.CompileRequest. Lives here as a
// copy so the bench tool can talk to any skills-rae instance over
// pure HTTP — no shared types, no transitive deps.
type compileRequest struct {
	Query          string     `json:"query"`
	TaskID         string     `json:"task_id,omitempty"`
	TopK           int        `json:"top_k,omitempty"`
	TopDownN       int        `json:"top_down_n,omitempty"`
	BottomUpN      int        `json:"bottom_up_n,omitempty"`
	L1N            int        `json:"l1_n,omitempty"`
	BudgetTokens   int        `json:"budget_tokens,omitempty"`
	OutputContract string     `json:"output_contract,omitempty"`
	Weights        Weights    `json:"weights,omitempty"`
	AffWeights     AffWeights `json:"aff_weights,omitempty"`
}

// compileResponse mirrors routes.CompileResponse — only the fields
// we score against. Extras are tolerated by json.Decode.
type compileResponse struct {
	SelectedSkills []selectedSkill `json:"selected_skills"`
	Diagnostics    diagnostics     `json:"diagnostics"`
}

type selectedSkill struct {
	SkillID uuid.UUID `json:"skill_id"`
	Score   float64   `json:"score"`
}

type diagnostics struct {
	StageMs       map[string]int64 `json:"stage_ms"`
	TopDownLabels []string         `json:"top_down_labels"`
	BottomUpHits  int              `json:"bottom_up_hits"`
	L1Hits        int              `json:"l1_hits"`
}

// Client is a thin HTTP client over /v1/rae/compile (or /v1/rae/search,
// configured via Config.Endpoint).
type Client struct {
	BaseURL  string
	HTTP     *http.Client
	Endpoint string // "compile" (default) or "search"
}

// NewClient constructs a Client with the given timeout. Pass 0 for
// the default of 30s — the same ceiling skills-rae enforces in its
// /v1/rae/compile handler.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		HTTP:     &http.Client{Timeout: timeout},
		Endpoint: "compile",
	}
}

// callResult is the internal "I got something back" record. The
// runner translates this into a public QueryResult.
type callResult struct {
	SelectedIDs   []uuid.UUID
	BottomUpHits  int
	L1Hits        int
	TopDownLabels []string
	HTTPStatus    int
	ElapsedMs     int64
	Err           error
}

// call POSTs one request and decodes the response. Wall-clock latency
// (including HTTP overhead) is measured around the round-trip; the
// server-reported diagnostics still come back in the response.
func (c *Client) call(ctx context.Context, cfg Config, item Item) callResult {
	body := compileRequest{
		Query:          item.Query,
		TaskID:         item.TaskID,
		TopK:           cfg.TopK,
		TopDownN:       cfg.TopDownN,
		BottomUpN:      cfg.BottomUpN,
		L1N:            cfg.L1N,
		BudgetTokens:   cfg.BudgetTokens,
		OutputContract: cfg.OutputContract,
		Weights:        cfg.Weights,
		AffWeights:     cfg.AffWeights,
	}
	if body.TaskID == "" {
		body.TaskID = "skillsbench-" + item.ID
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return callResult{Err: fmt.Errorf("marshal: %w", err)}
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "compile"
	}
	url := c.BaseURL + "/v1/rae/" + endpoint

	t0 := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return callResult{Err: fmt.Errorf("new request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return callResult{ElapsedMs: time.Since(t0).Milliseconds(), Err: fmt.Errorf("http: %w", err)}
	}
	defer resp.Body.Close()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	elapsed := time.Since(t0).Milliseconds()
	if err != nil {
		return callResult{ElapsedMs: elapsed, HTTPStatus: resp.StatusCode, Err: fmt.Errorf("read body: %w", err)}
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(respBytes)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return callResult{
			ElapsedMs:  elapsed,
			HTTPStatus: resp.StatusCode,
			Err:        fmt.Errorf("%s: %s", resp.Status, snippet),
		}
	}
	var parsed compileResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return callResult{ElapsedMs: elapsed, HTTPStatus: resp.StatusCode, Err: fmt.Errorf("decode: %w", err)}
	}
	ids := make([]uuid.UUID, 0, len(parsed.SelectedSkills))
	for _, s := range parsed.SelectedSkills {
		ids = append(ids, s.SkillID)
	}
	return callResult{
		SelectedIDs:   ids,
		BottomUpHits:  parsed.Diagnostics.BottomUpHits,
		L1Hits:        parsed.Diagnostics.L1Hits,
		TopDownLabels: parsed.Diagnostics.TopDownLabels,
		HTTPStatus:    resp.StatusCode,
		ElapsedMs:     elapsed,
	}
}

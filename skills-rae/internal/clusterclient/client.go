// Package clusterclient is a thin HTTP client for the evo
// cluster-sidecar — a Python service that wraps UMAP + HDBSCAN and
// exposes /v1/cluster (bulk hierarchical clustering) and /v1/assign
// (single-skill nearest-community lookup).
//
// Pattern follows internal/embeddings/client.go: the Client owns the
// http.Client so callers share keep-alive; an empty baseURL renders
// the client !Enabled() so misconfigured deploys fail loudly instead
// of silently producing nonsense communities.
package clusterclient

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

// Client wraps the cluster-sidecar URL + an http.Client with a generous
// timeout. /v1/cluster on the full corpus can take 60s+ on the
// workstation (UMAP fit + HDBSCAN + hierarchy extraction); /v1/assign
// is sub-second.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a clusterclient that talks to the given sidecar
// URL. Empty baseURL is legal; callers should branch on Enabled().
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

// Enabled reports whether a base URL was supplied. Callers check this
// before invoking the workflow steps that depend on the sidecar.
func (c *Client) Enabled() bool { return c != nil && c.baseURL != "" }

// ErrDisabled is returned when a method is called on a client that
// has no baseURL configured.
var ErrDisabled = errors.New("clusterclient: sidecar URL not configured")

// --- /v1/cluster ---

// ClusterRequest mirrors the Python sidecar's ClusterRequest shape.
type ClusterRequest struct {
	Vectors  [][]float32 `json:"vectors"`
	SkillIDs []string    `json:"skill_ids"`
}

// CommunityOut is one community row returned by /v1/cluster. The
// caller (activities/cluster.go) walks the list and persists rows into
// evo.skill_communities, rewriting (TempID, ParentTempID) into real
// UUIDs as it inserts.
type CommunityOut struct {
	TempID                int      `json:"temp_id"`
	ParentTempID          *int     `json:"parent_temp_id"`
	Level                 int      `json:"level"`
	MemberSkillIDs        []string `json:"member_skill_ids"`
	RepresentativeIndices []int    `json:"representative_indices"`
}

// ClusterResponse is the full response from /v1/cluster.
type ClusterResponse struct {
	TreeID        string            `json:"tree_id"`
	Levels        int               `json:"levels"`
	Communities   []CommunityOut    `json:"communities"`
	NoiseSkillIDs []string          `json:"noise_skill_ids"`
	Diagnostics   map[string]any    `json:"diagnostics"`
}

// Cluster runs the bulk clustering pipeline. Errors when the sidecar
// returns a non-2xx or the response is malformed; activity-level
// retries upstream cover transient failures.
func (c *Client) Cluster(ctx context.Context, req ClusterRequest) (*ClusterResponse, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(req.Vectors) != len(req.SkillIDs) {
		return nil, fmt.Errorf("clusterclient: vectors/skill_ids length mismatch: %d vs %d",
			len(req.Vectors), len(req.SkillIDs))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/cluster", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out ClusterResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// --- /v1/assign ---

// AssignCommunity is one community candidate in an AssignRequest.
type AssignCommunity struct {
	ID        string    `json:"id"`
	Embedding []float32 `json:"embedding"`
}

// AssignRequest is the input to /v1/assign.
type AssignRequest struct {
	Vector      []float32         `json:"vector"`
	Communities []AssignCommunity `json:"communities"`
}

// AssignResponse mirrors the sidecar's output: either a community_id
// + similarity, or noise=true.
type AssignResponse struct {
	CommunityID *string `json:"community_id"`
	Similarity  float64 `json:"similarity"`
	Noise       bool    `json:"noise"`
}

// Assign asks the sidecar for the nearest community to a single new
// skill embedding. Used by activities/assign.go on the IndexSkillWorkflow
// hot path.
func (c *Client) Assign(ctx context.Context, req AssignRequest) (*AssignResponse, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/assign", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out AssignResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

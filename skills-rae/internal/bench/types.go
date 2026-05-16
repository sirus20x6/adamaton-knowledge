// Package bench implements SkillsBench (SkillRAE Phase G): an offline
// eval harness that scores /v1/rae/compile retrieval quality on a
// labelled dataset. The harness is dataset-driven — callers author
// {query, gold_skill_ids} items in JSONL, point the runner at a
// running skills-rae instance, and get back per-item metrics plus
// aggregate Recall@K / MRR / nDCG / latency.
//
// The harness has no side effects on the DB: it only POSTs to the
// public /v1/rae/compile endpoint. That means it can be run from the
// workstation against the Pi-hosted service, or in CI against a local
// instance, or by hand during weight tuning.
package bench

import (
	"time"

	"github.com/google/uuid"
)

// Item is one labelled query. gold_skill_ids is the truth set against
// which the retriever's selected_skills is scored; gold_subunit_kinds
// optionally restricts subunit-recall to a stage (e.g. "procedure"
// when the user only cares whether procedural cues surfaced).
type Item struct {
	ID               string      `json:"id"`
	Query            string      `json:"query"`
	GoldSkillIDs     []uuid.UUID `json:"gold_skill_ids"`
	GoldSubunitKinds []string    `json:"gold_subunit_kinds,omitempty"`
	TaskID           string      `json:"task_id,omitempty"`
	Notes            string      `json:"notes,omitempty"`
}

// Dataset is a named bundle of Items. Loaded from JSONL via the
// loader package; the Name is conventionally the basename of the
// source file minus the .jsonl suffix.
type Dataset struct {
	Name  string `json:"name"`
	Items []Item `json:"items"`
}

// Config selects the SkillRAE knobs the harness will use for every
// item in a Run. Zero values are filled in by the runner with
// SkillRAE's own defaults so a bare Config still works.
type Config struct {
	BaseURL        string        `json:"base_url"`
	TopK           int           `json:"top_k"`
	TopDownN       int           `json:"top_down_n,omitempty"`
	BottomUpN      int           `json:"bottom_up_n,omitempty"`
	L1N            int           `json:"l1_n,omitempty"`
	BudgetTokens   int           `json:"budget_tokens,omitempty"`
	OutputContract string        `json:"output_contract,omitempty"`
	Weights        Weights       `json:"weights,omitempty"`
	AffWeights     AffWeights    `json:"aff_weights,omitempty"`
	Concurrency    int           `json:"concurrency,omitempty"`
	HTTPTimeout    time.Duration `json:"http_timeout,omitempty"`
	Endpoint       string        `json:"endpoint,omitempty"` // "compile" (default) or "search"
}

// Weights mirrors retrieval.Weights but lives in this package so the
// bench tool has no transitive Postgres / Temporal dependency. The
// HTTP client serialises them under the same JSON keys.
type Weights struct {
	Alpha  float64 `json:"alpha,omitempty"`
	Beta   float64 `json:"beta,omitempty"`
	Gamma  float64 `json:"gamma,omitempty"`
	Lambda float64 `json:"lambda,omitempty"`
}

// AffWeights mirrors compile.AffWeights. Same rationale as Weights.
type AffWeights struct {
	QRel    float64 `json:"q_rel,omitempty"`
	FSkill  float64 `json:"f_skill,omitempty"`
	RSel    float64 `json:"r_sel,omitempty"`
	GSame   float64 `json:"g_same,omitempty"`
	GActive float64 `json:"g_active,omitempty"`
}

// QueryResult is one row of the eval — what the retriever returned
// plus the scores we computed against gold.
type QueryResult struct {
	Item             Item        `json:"item"`
	ReturnedSkillIDs []uuid.UUID `json:"returned_skill_ids"`
	ElapsedMs        int64       `json:"elapsed_ms"`
	HTTPStatus       int         `json:"http_status"`
	Error            string      `json:"error,omitempty"`

	// Per-query metrics.
	HitAt1     int     `json:"hit_at_1"`     // 0 or 1
	HitAtK     int     `json:"hit_at_k"`     // 0 or 1
	RecallAtK  float64 `json:"recall_at_k"`  // |gold ∩ returned[:k]| / |gold|
	MRR        float64 `json:"mrr"`          // 1/rank of first gold; 0 if none
	NDCG       float64 `json:"ndcg"`         // nDCG@K with binary relevance
	AP         float64 `json:"average_precision"`

	// Top-line diagnostics — bubbled up so reports can correlate
	// metric quality with retrieval shape.
	BottomUpHits int      `json:"bottom_up_hits"`
	L1Hits       int      `json:"l1_hits"`
	CommLabels   []string `json:"top_down_labels,omitempty"`
}

// RunSummary is the aggregate view across one Dataset × Config.
type RunSummary struct {
	Dataset   string    `json:"dataset"`
	Config    Config    `json:"config"`
	N         int       `json:"n"`
	NErrors   int       `json:"n_errors"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	// Means across all non-error items.
	MeanRecallAtK float64 `json:"mean_recall_at_k"`
	HitRateAt1    float64 `json:"hit_rate_at_1"`
	HitRateAtK    float64 `json:"hit_rate_at_k"`
	MeanMRR       float64 `json:"mean_mrr"`
	MeanNDCG      float64 `json:"mean_ndcg"`
	MeanAP        float64 `json:"map"`

	// Latency in milliseconds.
	MeanLatencyMs int64 `json:"mean_latency_ms"`
	P50LatencyMs  int64 `json:"p50_latency_ms"`
	P95LatencyMs  int64 `json:"p95_latency_ms"`
	P99LatencyMs  int64 `json:"p99_latency_ms"`
}

// SweepResult is one cell of a weight-grid sweep.
type SweepResult struct {
	Weights    Weights    `json:"weights"`
	AffWeights AffWeights `json:"aff_weights"`
	Summary    RunSummary `json:"summary"`
}

// Package extract drives the LLM-based knowledge-graph extraction for
// a document. We ask the LLM for JSON structured output (enforced via
// vLLM's guided_json) and convert each entity / relationship to the
// same row shape R2R wrote into deepresearch.documents_entities /
// documents_relationships. The previous XML-tag prompt port worked
// against gpt-4o but qwen3.6-27b-nvfp8 routinely ignored it (emitting
// Markdown bullets or hallucinated tags) so we use schema-enforced
// JSON instead.
package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/llm"
)

// ErrSuspectEmpty is returned by Run when the LLM gave back valid but
// empty JSON for an input that clearly should have produced entities --
// e.g., a 12K-token batch of paper text. We saw this on 2026-05-15 when
// vLLM started returning `{"entities":[]}` for everything while
// degraded but before it tipped fully over into timeouts. Without this
// signal the caller would happily wipe prior entities + clear stage2_at,
// destroying real work because the upstream model went sideways.
//
// Callers (routes/extract.go) treat this as a transient failure: 502
// the request, skip the persist, skip the stage update. Reindex retries
// the doc later when (hopefully) the LLM is healthy again.
var ErrSuspectEmpty = errors.New("extract: substantive input produced empty result (likely upstream LLM issue)")

// SuspectEmptyInputFloor is the input-token threshold above which an
// empty parsed result is treated as suspect. Tuned for our chunk size
// (~1K tokens / chunk); the smallest reasonable batch is ~3 chunks.
// Below this floor an empty result is plausibly legitimate -- a
// paragraph might genuinely have no extractable entities.
const SuspectEmptyInputFloor = 1000

// Defaults sized for our deployment:
//   - 12K-token input batches leave room inside a 64K context for the
//     prompt scaffolding + a verbose schema-constrained JSON response.
//   - max_tokens 24K caps a single response. CoT is disabled
//     (DisableThinking=true in callLLM) so the full budget goes to the
//     structured JSON; the headroom is for entity-rich papers (LLM
//     survey, KG survey) where the JSON itself can run several thousand
//     tokens.
//   - Temperature 0.0 because we want deterministic structured output.
const (
	DefaultMaxBatchTokens = 12000
	DefaultMaxTokens      = 24576
)

// Entity is one row destined for deepresearch.documents_entities.
type Entity struct {
	Name        string
	Category    string
	Description string
}

// Relationship is one row destined for deepresearch.documents_relationships.
type Relationship struct {
	Subject     string
	Predicate   string
	Object      string
	Description string
	Weight      float64
}

// Result is what Run returns: the deduped entity + relationship sets
// across all LLM batches for the document.
type Result struct {
	Entities      []Entity
	Relationships []Relationship
	LLMCalls      int
	InputTokens   int
}

// TokenCounter estimates the token count of a string.
type TokenCounter func(string) int

// schemaForResponse describes the JSON shape we want from the LLM.
// vLLM enforces this with outlines/lm-format-enforcer under the hood.
func schemaForResponse() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entities": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string"},
						"type":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
					},
					"required":             []string{"name", "type", "description"},
					"additionalProperties": false,
				},
			},
			"relationships": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source":      map[string]any{"type": "string"},
						"target":      map[string]any{"type": "string"},
						"relation":    map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"weight":      map[string]any{"type": "number"},
					},
					"required":             []string{"source", "target", "relation", "description", "weight"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"entities", "relationships"},
		"additionalProperties": false,
	}
}

// llmResponse is the shape we ask the model to emit.
type llmResponse struct {
	Entities []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"entities"`
	Relationships []struct {
		Source      string  `json:"source"`
		Target      string  `json:"target"`
		Relation    string  `json:"relation"`
		Description string  `json:"description"`
		Weight      float64 `json:"weight"`
	} `json:"relationships"`
}

// Run orchestrates the extraction. It groups chunk texts into batches
// that fit inside maxBatchTokens, calls the LLM with a guided_json
// schema for each, and dedupes across batches.
//
// Dedup policy mirrors R2R:
//   - Entities are keyed by lowercased name. First-seen wins for
//     category + description.
//   - Relationships are keyed by (subject, predicate, object) tuple,
//     also lowercased; first-seen wins for description + weight.
func Run(
	ctx context.Context,
	client *llm.Client,
	chunks []string,
	tokens TokenCounter,
	maxBatchTokens int,
) (*Result, error) {
	if len(chunks) == 0 {
		return &Result{}, nil
	}
	if maxBatchTokens <= 0 {
		maxBatchTokens = DefaultMaxBatchTokens
	}

	batches := groupByTokenBudget(chunks, tokens, maxBatchTokens)
	res := &Result{}
	entByName := map[string]int{}
	relByTriple := map[string]int{}

	for _, batch := range batches {
		joined := strings.Join(batch, "\n\n")
		res.InputTokens += tokens(joined)

		body, err := callLLM(ctx, client, joined)
		if err != nil {
			return nil, fmt.Errorf("llm call: %w", err)
		}
		res.LLMCalls++

		// guided_json constrains output to valid JSON matching the
		// schema, so this should always parse. If it doesn't, the
		// model truncated mid-stream (hit max_tokens before closing
		// brackets); we treat that as a soft failure and skip the
		// batch rather than fail the whole extract.
		var parsed llmResponse
		// Trim any stray whitespace / leading newlines that some
		// backends emit before the opening brace.
		s := strings.TrimSpace(body)
		if !strings.HasPrefix(s, "{") {
			// Try to find the first { in case the model added text
			// before it.
			if i := strings.IndexByte(s, '{'); i >= 0 {
				s = s[i:]
			}
		}
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			// Swallow parse error; record nothing for this batch.
			continue
		}

		for _, e := range parsed.Entities {
			name := strings.TrimSpace(e.Name)
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if _, ok := entByName[key]; ok {
				continue
			}
			entByName[key] = len(res.Entities)
			res.Entities = append(res.Entities, Entity{
				Name:        name,
				Category:    strings.TrimSpace(e.Type),
				Description: strings.TrimSpace(e.Description),
			})
		}
		for _, r := range parsed.Relationships {
			s := strings.TrimSpace(r.Source)
			t := strings.TrimSpace(r.Target)
			rel := strings.TrimSpace(r.Relation)
			if s == "" || t == "" || rel == "" {
				continue
			}
			key := strings.ToLower(s + "|" + rel + "|" + t)
			if _, ok := relByTriple[key]; ok {
				continue
			}
			w := r.Weight
			if w < 0 {
				w = 0
			}
			if w > 10 {
				w = 10
			}
			relByTriple[key] = len(res.Relationships)
			res.Relationships = append(res.Relationships, Relationship{
				Subject:     s,
				Predicate:   rel,
				Object:      t,
				Description: strings.TrimSpace(r.Description),
				Weight:      w,
			})
		}
	}
	// Suspect-empty guard: substantive input + zero output is treated
	// as a transient LLM failure rather than truthful "this doc has no
	// graph." Keeps prior entities intact when vLLM hiccups.
	if res.InputTokens >= SuspectEmptyInputFloor &&
		len(res.Entities) == 0 && len(res.Relationships) == 0 {
		return nil, ErrSuspectEmpty
	}
	return res, nil
}

// groupByTokenBudget greedily packs chunks into batches whose total
// token count stays under budget. A single chunk that exceeds budget
// becomes its own batch.
func groupByTokenBudget(chunks []string, tokens TokenCounter, budget int) [][]string {
	var out [][]string
	var cur []string
	curBudget := 0
	for _, c := range chunks {
		t := tokens(c)
		if len(cur) > 0 && curBudget+t > budget {
			out = append(out, cur)
			cur = nil
			curBudget = 0
		}
		cur = append(cur, c)
		curBudget += t
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// callLLM fires one schema-enforced extraction round-trip. Thinking is
// disabled: entity/relation extraction is pattern recognition, not
// reasoning, and qwen3's CoT just burns the max_tokens budget before
// the guided_json tail can fit (see ErrSuspectEmpty doc comment).
func callLLM(ctx context.Context, client *llm.Client, body string) (string, error) {
	zero := 0.0
	prompt := buildPrompt(body)
	return client.Complete(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, llm.CompletionOptions{
		Temperature:     &zero,
		MaxTokens:       DefaultMaxTokens,
		GuidedJSON:      schemaForResponse(),
		DisableThinking: true,
	})
}

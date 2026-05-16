// Package llm is the typed HTTP client r2g uses to talk to the
// OpenAI-compatible chat-completions endpoint that fronts vLLM (or
// any other backend that speaks the API). The same client could
// front Anthropic, Gemini, or local Ollama via env-var swap.
package llm

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

// Client wraps an OpenAI-compatible API base + key + model name.
// One instance per process; thread-safe via the shared http.Client.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

// NewClient builds a client. baseURL must include the /v1 suffix
// (e.g. http://10.0.4.37:9080/v1). apiKey can be empty for backends
// that don't enforce it; we still send the header because some
// proxies care.
//
// Timeout 300s sized for a slow LLM under load. Per-request context
// from the caller bounds individual completions.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

// Message is the chat-completions message shape.
type Message struct {
	Role    string `json:"role"`    // system | user | assistant
	Content string `json:"content"`
}

// CompletionOptions tunes a single call. All fields optional.
type CompletionOptions struct {
	Temperature   *float64
	TopP          *float64
	MaxTokens     int
	StopSequences []string
	// GuidedJSON is a JSON Schema (as a Go map). When non-nil it
	// becomes the `guided_json` extra-body param vLLM uses to enforce
	// structured output. Non-vLLM backends ignore it. Crucial for
	// qwen3.x which otherwise emits Markdown/garbage when asked for
	// XML or JSON via prompt alone.
	GuidedJSON map[string]any
	// DisableThinking turns off qwen3's chain-of-thought for this call
	// (and any model whose chat template understands /no_think).
	// Belt-and-suspenders: sends BOTH
	//   - extra_body chat_template_kwargs={"enable_thinking": false}
	//   - a leading "/no_think" line on the first user message
	// so either wire-protocol channel is sufficient. Use for
	// pattern-recognition tasks (entity/relation extraction) where CoT
	// just burns the max_tokens budget before the structured JSON tail
	// fits -- the empirical bug we saw on qwen3.6-27b-nvfp8 with
	// 24K max_tokens producing "{\"entities\":[]}" because the CoT
	// consumed everything before the schema-constrained output.
	DisableThinking bool
}

// Complete fires a chat-completions request and returns the assistant's
// first-choice message content. Errors include both transport failures
// and HTTP-level errors (non-2xx).
func (c *Client) Complete(ctx context.Context, messages []Message, opts CompletionOptions) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("llm: empty messages")
	}
	req := map[string]any{
		"model":    c.model,
		"messages": messages,
		"stream":   false,
	}
	if opts.Temperature != nil {
		req["temperature"] = *opts.Temperature
	}
	if opts.TopP != nil {
		req["top_p"] = *opts.TopP
	}
	if opts.MaxTokens > 0 {
		req["max_tokens"] = opts.MaxTokens
	}
	if len(opts.StopSequences) > 0 {
		req["stop"] = opts.StopSequences
	}
	if opts.GuidedJSON != nil {
		// vLLM accepts guided_json as a top-level extra. Other backends
		// will see it as an unknown field; OpenAI ignores unknown
		// params, Anthropic doesn't ship through this client.
		req["guided_json"] = opts.GuidedJSON
	}
	if opts.DisableThinking {
		// vLLM passes chat_template_kwargs straight to the tokenizer's
		// chat template; qwen3's template reads enable_thinking and
		// suppresses the <think>...</think> block when false. Other
		// backends ignore the unknown field.
		req["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		// Belt: prepend "/no_think" to the first user message so a
		// build that ignores chat_template_kwargs still gets the hint
		// via the prompt. Qwen3 recognises this as a thinking switch.
		messages = withNoThinkPrefix(messages)
		req["messages"] = messages
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("POST /chat/completions: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("/chat/completions HTTP %d: %s", resp.StatusCode, tail(string(respBody), 200))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Reasoning string `json:"reasoning"` // vLLM qwen3 puts CoT here; sometimes answer too
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parse response: %w (body: %s)", err, tail(string(respBody), 200))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm returned zero choices")
	}
	msg := parsed.Choices[0].Message
	// Prefer content. If empty (e.g. qwen3 thinking-mode ran out of
	// tokens before emitting the post-CoT answer), fall back to the
	// reasoning field so the caller still has something to parse.
	if msg.Content != "" {
		return msg.Content, nil
	}
	return msg.Reasoning, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// withNoThinkPrefix prepends "/no_think\n" to the first non-system message
// so a qwen3 build that ignores chat_template_kwargs still gets the
// thinking-suppression hint via the prompt. Returns a fresh slice; the
// input is not mutated.
func withNoThinkPrefix(in []Message) []Message {
	out := make([]Message, len(in))
	copy(out, in)
	for i, m := range out {
		if m.Role == "system" {
			continue
		}
		if strings.HasPrefix(m.Content, "/no_think") {
			break
		}
		out[i].Content = "/no_think\n" + m.Content
		break
	}
	return out
}

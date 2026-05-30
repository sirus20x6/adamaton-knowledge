package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ptrFloat(f float64) *float64 { return &f }

// TestComplete_HappyPath verifies a normal 200 response returns the
// first-choice content and that the request body carries the expected
// fields.
func TestComplete_HappyPath(t *testing.T) {
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer secret" {
			t.Errorf("Authorization = %q, want Bearer secret", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hello world"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/", "secret", "qwen3")
	out, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, CompletionOptions{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "hello world" {
		t.Errorf("content = %q, want %q", out, "hello world")
	}
	if gotReq["model"] != "qwen3" {
		t.Errorf("model = %v, want qwen3", gotReq["model"])
	}
	if gotReq["stream"] != false {
		t.Errorf("stream = %v, want false", gotReq["stream"])
	}
}

// TestNewClient_TrimsTrailingSlash confirms the baseURL is normalized so
// the request path is well-formed.
func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("http://example.com/v1///", "", "m")
	if c.baseURL != "http://example.com/v1" {
		t.Errorf("baseURL = %q, want http://example.com/v1", c.baseURL)
	}
}

// TestComplete_EmptyMessages returns an error without making a request.
func TestComplete_EmptyMessages(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "", "m")
	_, err := c.Complete(context.Background(), nil, CompletionOptions{})
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
	if !strings.Contains(err.Error(), "empty messages") {
		t.Errorf("error = %v, want it to mention empty messages", err)
	}
}

// TestComplete_NoAuthHeaderWhenKeyEmpty: an empty apiKey must not send an
// Authorization header.
func TestComplete_NoAuthHeaderWhenKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Errorf("Authorization header present but apiKey was empty")
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, CompletionOptions{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

// TestComplete_OptionsWireUp verifies optional knobs land in the request
// body with the right keys.
func TestComplete_OptionsWireUp(t *testing.T) {
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	opts := CompletionOptions{
		Temperature:   ptrFloat(0.2),
		TopP:          ptrFloat(0.9),
		MaxTokens:     128,
		StopSequences: []string{"<end>"},
		GuidedJSON:    map[string]any{"type": "object"},
	}
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, opts); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotReq["temperature"] != 0.2 {
		t.Errorf("temperature = %v, want 0.2", gotReq["temperature"])
	}
	if gotReq["top_p"] != 0.9 {
		t.Errorf("top_p = %v, want 0.9", gotReq["top_p"])
	}
	if gotReq["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v, want 128", gotReq["max_tokens"])
	}
	stop, ok := gotReq["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "<end>" {
		t.Errorf("stop = %v, want [<end>]", gotReq["stop"])
	}
	if _, ok := gotReq["guided_json"]; !ok {
		t.Errorf("guided_json missing from request")
	}
}

// TestComplete_DisableThinking checks both wire channels: the
// chat_template_kwargs extra-body AND the /no_think prefix on the first
// user message.
func TestComplete_DisableThinking(t *testing.T) {
	var gotReq map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	msgs := []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "question"}}
	if _, err := c.Complete(context.Background(), msgs, CompletionOptions{DisableThinking: true}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	ctk, ok := gotReq["chat_template_kwargs"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_kwargs missing/wrong type: %v", gotReq["chat_template_kwargs"])
	}
	if ctk["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false", ctk["enable_thinking"])
	}
	wireMsgs, ok := gotReq["messages"].([]any)
	if !ok || len(wireMsgs) != 2 {
		t.Fatalf("messages on wire = %v", gotReq["messages"])
	}
	// system message untouched
	sys := wireMsgs[0].(map[string]any)
	if sys["content"] != "sys" {
		t.Errorf("system content mutated: %v", sys["content"])
	}
	user := wireMsgs[1].(map[string]any)
	if !strings.HasPrefix(user["content"].(string), "/no_think\n") {
		t.Errorf("user content missing /no_think prefix: %q", user["content"])
	}
	// the caller's slice must not be mutated
	if msgs[1].Content != "question" {
		t.Errorf("caller messages mutated: %q", msgs[1].Content)
	}
}

// TestComplete_HTTPError surfaces non-2xx as an error including the status
// code and a body tail.
func TestComplete_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"backend exploded"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, CompletionOptions{})
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error = %v, want it to mention HTTP 500", err)
	}
	if !strings.Contains(err.Error(), "backend exploded") {
		t.Errorf("error = %v, want it to include body tail", err)
	}
}

// TestComplete_UnparseableBody returns a parse error when the body isn't
// valid JSON.
func TestComplete_UnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not json at all`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, CompletionOptions{})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Errorf("error = %v, want it to mention parse response", err)
	}
}

// TestComplete_ZeroChoices errors when the backend returns no choices.
func TestComplete_ZeroChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, CompletionOptions{})
	if err == nil {
		t.Fatal("expected error for zero choices")
	}
	if !strings.Contains(err.Error(), "zero choices") {
		t.Errorf("error = %v, want it to mention zero choices", err)
	}
}

// TestComplete_ReasoningFallback: when content is empty, the reasoning
// field is returned instead.
func TestComplete_ReasoningFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"","reasoning":"the CoT answer"}}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "", "m")
	out, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, CompletionOptions{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "the CoT answer" {
		t.Errorf("content = %q, want reasoning fallback", out)
	}
}

// TestComplete_TransportError: a dead server (immediately closed) yields a
// transport-level error.
func TestComplete_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // close before the request so Do() fails to connect

	c := NewClient(url, "", "m")
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "x"}}, CompletionOptions{})
	if err == nil {
		t.Fatal("expected transport error against closed server")
	}
	if !strings.Contains(err.Error(), "chat/completions") {
		t.Errorf("error = %v, want it to mention the endpoint", err)
	}
}

// TestComplete_ContextTimeout: a slow server is cancelled by a short
// context deadline.
func TestComplete_ContextTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test releases us
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL, "", "m")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Complete(ctx, []Message{{Role: "user", Content: "x"}}, CompletionOptions{})
	if err == nil {
		t.Fatal("expected context deadline error")
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error = %v, want a context/deadline error", err)
	}
}

func TestTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"shorter than n", "abc", 10, "abc"},
		{"equal to n", "abcde", 5, "abcde"},
		{"longer than n", "abcdefgh", 3, "...fgh"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tail(tc.in, tc.n); got != tc.want {
				t.Errorf("tail(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestWithNoThinkPrefix(t *testing.T) {
	t.Run("prefixes first non-system message", func(t *testing.T) {
		in := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}, {Role: "user", Content: "u2"}}
		out := withNoThinkPrefix(in)
		if out[0].Content != "s" {
			t.Errorf("system mutated: %q", out[0].Content)
		}
		if out[1].Content != "/no_think\nu" {
			t.Errorf("first user = %q, want /no_think\\nu", out[1].Content)
		}
		if out[2].Content != "u2" {
			t.Errorf("second user should be untouched: %q", out[2].Content)
		}
		// input not mutated
		if in[1].Content != "u" {
			t.Errorf("input mutated: %q", in[1].Content)
		}
	})
	t.Run("idempotent when already prefixed", func(t *testing.T) {
		in := []Message{{Role: "user", Content: "/no_think\nalready"}}
		out := withNoThinkPrefix(in)
		if out[0].Content != "/no_think\nalready" {
			t.Errorf("double-prefixed: %q", out[0].Content)
		}
	})
	t.Run("all system messages untouched", func(t *testing.T) {
		in := []Message{{Role: "system", Content: "a"}, {Role: "system", Content: "b"}}
		out := withNoThinkPrefix(in)
		for i, m := range out {
			if m.Content != in[i].Content {
				t.Errorf("system message %d mutated: %q", i, m.Content)
			}
		}
	})
}

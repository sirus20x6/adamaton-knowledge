package reranker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewClient_TrimsAndDetectsEnabled checks URL normalization and the
// Enabled() predicate.
func TestNewClient_TrimsAndDetectsEnabled(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantBase    string
		wantEnabled bool
	}{
		{"empty", "", "", false},
		{"whitespace only", "   ", "", false},
		{"trailing slash", "http://x:9000/", "http://x:9000", true},
		{"surrounding space", "  http://x:9000  ", "http://x:9000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient(tc.in)
			if c.baseURL != tc.wantBase {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.wantBase)
			}
			if c.Enabled() != tc.wantEnabled {
				t.Errorf("Enabled() = %v, want %v", c.Enabled(), tc.wantEnabled)
			}
		})
	}
}

// TestEnabled_NilReceiver: Enabled must be safe on a nil *Client.
func TestEnabled_NilReceiver(t *testing.T) {
	var c *Client
	if c.Enabled() {
		t.Error("nil client should report Enabled() == false")
	}
}

// TestRerank_Disabled returns ErrDisabled when no base URL is configured.
func TestRerank_Disabled(t *testing.T) {
	c := NewClient("")
	_, err := c.Rerank(context.Background(), "q", []string{"p"}, 0)
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

// TestRerank_EmptyQuery rejects an empty query before hitting the wire.
func TestRerank_EmptyQuery(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	_, err := c.Rerank(context.Background(), "", []string{"p"}, 0)
	if err == nil || !strings.Contains(err.Error(), "empty query") {
		t.Errorf("err = %v, want empty query error", err)
	}
}

// TestRerank_NoPassages returns (nil, nil) without making a request.
func TestRerank_NoPassages(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	hits, err := c.Rerank(context.Background(), "q", nil, 0)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if hits != nil {
		t.Errorf("hits = %v, want nil", hits)
	}
}

// TestRerank_HappyPath sends the right request shape and parses ranked
// hits out of the response.
func TestRerank_HappyPath(t *testing.T) {
	var gotReq rerankReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("path = %q, want /rerank", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		io.WriteString(w, `{"model":"bge","elapsed_ms":12,"ranked":[{"index":2,"score":0.9},{"index":0,"score":0.3}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	hits, err := c.Rerank(context.Background(), "the query", []string{"a", "b", "c"}, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(hits) != 2 || hits[0].Index != 2 || hits[0].Score != 0.9 {
		t.Errorf("hits = %+v, want [{2 0.9} {0 0.3}]", hits)
	}
	if gotReq.Query != "the query" {
		t.Errorf("query = %q, want the query", gotReq.Query)
	}
	if len(gotReq.Passages) != 3 {
		t.Errorf("passages = %v, want 3", gotReq.Passages)
	}
	if gotReq.TopK != nil {
		t.Errorf("topK should be omitted when 0, got %v", *gotReq.TopK)
	}
}

// TestRerank_TopKWired: a positive topK is serialized into the request.
func TestRerank_TopKWired(t *testing.T) {
	var gotReq rerankReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		io.WriteString(w, `{"ranked":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.Rerank(context.Background(), "q", []string{"p1", "p2"}, 5); err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if gotReq.TopK == nil || *gotReq.TopK != 5 {
		t.Errorf("topK = %v, want 5", gotReq.TopK)
	}
}

// TestRerank_HTTPError surfaces a non-2xx with the FastAPI detail body.
func TestRerank_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"detail":"passages too long"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Rerank(context.Background(), "q", []string{"p"}, 0)
	if err == nil {
		t.Fatal("expected error on HTTP 422")
	}
	if !strings.Contains(err.Error(), "HTTP 422") {
		t.Errorf("err = %v, want it to mention HTTP 422", err)
	}
	if !strings.Contains(err.Error(), "passages too long") {
		t.Errorf("err = %v, want it to include the detail body", err)
	}
}

// TestRerank_BadJSON returns a decode error for a malformed body.
func TestRerank_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{not valid`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Rerank(context.Background(), "q", []string{"p"}, 0)
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Errorf("err = %v, want decode response error", err)
	}
}

// TestRerank_TransportError: closed server yields a post error.
func TestRerank_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(url)
	_, err := c.Rerank(context.Background(), "q", []string{"p"}, 0)
	if err == nil || !strings.Contains(err.Error(), "post rerank") {
		t.Errorf("err = %v, want post rerank error", err)
	}
}

// TestRerank_ContextTimeout: a hung sidecar is cancelled by the context.
func TestRerank_ContextTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Rerank(ctx, "q", []string{"p"}, 0)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
	if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %v, want a context/deadline error", err)
	}
}

// TestRerank_EmptyRanked: a well-formed response with no hits returns an
// empty slice, not an error.
func TestRerank_EmptyRanked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"bge","elapsed_ms":1,"ranked":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	hits, err := c.Rerank(context.Background(), "q", []string{"p"}, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %v, want empty", hits)
	}
}

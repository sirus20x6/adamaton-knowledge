package bench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeServer returns a /v1/rae/compile-shaped response. The selected
// skills are derived from the query: tokens before the first space
// are parsed as a comma-separated list of UUIDs to return in order.
// Lets tests assemble deterministic returns per item.
func fakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/rae/compile") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		// Query format: "uuid1,uuid2,uuid3 actual text" — extract the IDs.
		head := body.Query
		if i := strings.Index(head, " "); i > 0 {
			head = head[:i]
		}
		type sel struct {
			SkillID uuid.UUID `json:"skill_id"`
			Score   float64   `json:"score"`
		}
		out := struct {
			Selected []sel       `json:"selected_skills"`
			Diag     map[string]any `json:"diagnostics"`
		}{Diag: map[string]any{"bottom_up_hits": 12, "l1_hits": 7, "top_down_labels": []string{"clusterA"}}}
		score := 1.0
		for _, s := range strings.Split(head, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			id, err := uuid.Parse(s)
			if err != nil {
				continue
			}
			out.Selected = append(out.Selected, sel{SkillID: id, Score: score})
			score -= 0.1
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func TestRunner_HappyPath(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()

	a, b, c := uuid.New(), uuid.New(), uuid.New()
	// item 1: gold = a, returned = a,b,c → hit@1=1, recall@5=1, mrr=1
	// item 2: gold = b, returned = c,a,b → hit@1=0, hit@3=1, mrr=1/3
	ds := &Dataset{
		Name: "test",
		Items: []Item{
			{ID: "i1", Query: a.String() + "," + b.String() + "," + c.String() + " find a", GoldSkillIDs: []uuid.UUID{a}},
			{ID: "i2", Query: c.String() + "," + a.String() + "," + b.String() + " find b", GoldSkillIDs: []uuid.UUID{b}},
		},
	}
	client := NewClient(srv.URL, 5*time.Second)
	cfg := Config{BaseURL: srv.URL, TopK: 3, Concurrency: 2}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	summary, results, err := Run(ctx, ds, cfg, client)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if summary.N != 2 || summary.NErrors != 0 {
		t.Fatalf("summary N/err = %d/%d, want 2/0", summary.N, summary.NErrors)
	}
	if summary.HitRateAt1 != 0.5 {
		t.Fatalf("hit@1 mean want 0.5 got %v", summary.HitRateAt1)
	}
	if summary.HitRateAtK != 1.0 {
		t.Fatalf("hit@K mean want 1 got %v", summary.HitRateAtK)
	}
	// mean MRR = (1 + 1/3) / 2 = 0.667
	if summary.MeanMRR < 0.66 || summary.MeanMRR > 0.67 {
		t.Fatalf("mean MRR want ~0.667 got %v", summary.MeanMRR)
	}
	if len(results) != 2 {
		t.Fatalf("results len want 2 got %d", len(results))
	}
}

func TestRunner_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ds := &Dataset{Name: "t", Items: []Item{{ID: "x", Query: "anything", GoldSkillIDs: []uuid.UUID{uuid.New()}}}}
	client := NewClient(srv.URL, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	summary, results, err := Run(ctx, ds, Config{BaseURL: srv.URL, TopK: 3}, client)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if summary.NErrors != 1 {
		t.Fatalf("NErrors want 1 got %d", summary.NErrors)
	}
	if results[0].Error == "" {
		t.Fatalf("result.Error should be set on 500")
	}
}

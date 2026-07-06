package routes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// newBadPool returns a lazily-created pool pointing at a port nothing
// listens on. pgxpool v5 does not dial at construction time, so the
// pool builds fine and every Ping/Query fails fast — exactly what the
// error-path tests need without a real Postgres.
func newBadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

func newRouter(t *testing.T, pool *pgxpool.Pool) *mux.Router {
	t.Helper()
	r := mux.NewRouter()
	logger := quietLogger()
	RegisterHealth(r, pool, logger)
	RegisterSearch(r, pool, nil, nil, logger)
	RegisterCompile(r, pool, nil, nil, logger)
	RegisterDebug(r, pool, logger)
	return r
}

func do(t *testing.T, r *mux.Router, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("error body is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	return m["error"]
}

func TestHealthz_DegradedWhenDBUnreachable(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "degraded" {
		t.Errorf("status field = %v, want degraded", body["status"])
	}
	if body["db"] != false {
		t.Errorf("db field = %v, want false", body["db"])
	}
	if body["service"] != "skills-rae" {
		t.Errorf("service field = %v, want skills-rae", body["service"])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestHealthz_MethodNotAllowed(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodPost, "/healthz", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestSearch_InvalidJSON(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodPost, "/v1/rae/search", "{not json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErr(t, rec); !strings.Contains(msg, "invalid json") {
		t.Errorf("error = %q, want invalid json prefix", msg)
	}
}

func TestSearch_MissingQuery(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodPost, "/v1/rae/search", `{"top_k": 5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErr(t, rec); msg != "query is required" {
		t.Errorf("error = %q, want %q", msg, "query is required")
	}
}

func TestSearch_GetNotAllowed(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodGet, "/v1/rae/search", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCompile_InvalidJSON(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodPost, "/v1/rae/compile", "]]")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErr(t, rec); !strings.Contains(msg, "invalid json") {
		t.Errorf("error = %q, want invalid json prefix", msg)
	}
}

func TestCompile_MissingQuery(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodPost, "/v1/rae/compile", `{"budget_tokens": 1500}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErr(t, rec); msg != "query is required" {
		t.Errorf("error = %q, want %q", msg, "query is required")
	}
}

func TestCompile_BodyTooLarge(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	// MaxBytesReader caps the body at 1 MiB; a bigger payload must be
	// rejected as a decode error (400), never a panic or a 200.
	big := `{"query": "` + strings.Repeat("x", 1<<20+100) + `"}`
	rec := do(t, r, http.MethodPost, "/v1/rae/compile", big)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDebug_CommunitySkills_InvalidUUID(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodGet, "/v1/rae/community/not-a-uuid/skills", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErr(t, rec); msg != "invalid community id" {
		t.Errorf("error = %q, want %q", msg, "invalid community id")
	}
}

func TestDebug_SkillSubunits_InvalidUUID(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodGet, "/v1/rae/skill/zzz/subunits", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if msg := decodeErr(t, rec); msg != "invalid skill id" {
		t.Errorf("error = %q, want %q", msg, "invalid skill id")
	}
}

func TestDebug_Communities_DBDown(t *testing.T) {
	r := newRouter(t, newBadPool(t))
	rec := do(t, r, http.MethodGet, "/v1/rae/communities", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if msg := decodeErr(t, rec); msg == "" {
		t.Error("expected non-empty error message")
	}
}

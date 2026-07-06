package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

// routes_db_test.go covers the debug endpoints' happy paths and the
// healthz ok path against a real Postgres (docker-gated; skips without
// a daemon or when GOGENTS_SKIP_DOCKER_TESTS is set).

const routesSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS evo;

CREATE TABLE evo.skill_communities (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    label               TEXT NOT NULL,
    representative_text TEXT NOT NULL,
    embedding           vector(4),
    member_count        INT  NOT NULL DEFAULT 0,
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE evo.skills (
    id             UUID PRIMARY KEY,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    origin         TEXT NOT NULL DEFAULT 'local',
    drift_detected BOOLEAN NOT NULL DEFAULT FALSE,
    community_id   UUID REFERENCES evo.skill_communities(id),
    rae_indexed_at TIMESTAMPTZ
);

CREATE TABLE evo.skill_subunits (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL UNIQUE,
    kind         TEXT NOT NULL,
    length       INT  NOT NULL,
    idf          REAL NOT NULL DEFAULT 0
);

CREATE TABLE evo.skill_subunit_edges (
    skill_id   UUID NOT NULL REFERENCES evo.skills(id)         ON DELETE CASCADE,
    subunit_id UUID NOT NULL REFERENCES evo.skill_subunits(id) ON DELETE CASCADE,
    line_no    INT,
    PRIMARY KEY (skill_id, subunit_id)
);
`

func dockerAvailableRoutes(t *testing.T) bool {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		return false
	}
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		return true
	}
	for _, sock := range []string{
		"/var/run/docker.sock",
		os.Getenv("XDG_RUNTIME_DIR") + "/docker.sock",
	} {
		if sock == "/docker.sock" {
			continue
		}
		if _, err := os.Stat(sock); err == nil {
			return true
		}
	}
	return false
}

func newRoutesTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if !dockerAvailableRoutes(t) {
		t.Skip("docker not available (or GOGENTS_SKIP_DOCKER_TESTS set); skipping routes DB test")
	}
	pool := pgutil.NewTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, routesSchemaDDL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func TestHealthz_OKWithLiveDB(t *testing.T) {
	pool := newRoutesTestDB(t)
	r := newRouter(t, pool)
	rec := do(t, r, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["db"] != true {
		t.Errorf("body = %v, want status=ok db=true", body)
	}
}

func TestDebug_EndToEnd(t *testing.T) {
	pool := newRoutesTestDB(t)
	ctx := context.Background()

	// Seed: one community, two member skills, one skill with subunits.
	commID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_communities (id, label, representative_text, embedding, member_count)
		VALUES ($1, 'refactoring', 'rep text', $2, 2)`,
		commID, pgvector.NewVector([]float32{1, 0, 0, 0})); err != nil {
		t.Fatal(err)
	}
	skillA := uuid.New()
	skillB := uuid.New()
	indexedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skills (id, name, description, origin, drift_detected, community_id, rae_indexed_at)
		VALUES ($1, 'alpha', 'first', 'local', false, $3, $4),
		       ($2, 'beta',  'second', 'github', true, $3, NULL)`,
		skillA, skillB, commID, indexedAt); err != nil {
		t.Fatal(err)
	}
	subID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_subunits (id, content, content_hash, kind, length, idf)
		VALUES ($1, 'step: do the thing', 'hash-1', 'step', 18, 0.7)`, subID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_subunit_edges (skill_id, subunit_id, line_no)
		VALUES ($1, $2, 42)`, skillA, subID); err != nil {
		t.Fatal(err)
	}

	r := newRouter(t, pool)

	t.Run("communities", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/rae/communities", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		var rows []CommunityRow
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d communities, want 1", len(rows))
		}
		c := rows[0]
		if c.ID != commID.String() || c.Label != "refactoring" || c.MemberCount != 2 {
			t.Errorf("row = %+v", c)
		}
		if _, err := time.Parse(time.RFC3339, c.ComputedAt); err != nil {
			t.Errorf("computed_at %q not RFC3339: %v", c.ComputedAt, err)
		}
	})

	t.Run("community skills", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/rae/community/"+commID.String()+"/skills", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		var rows []CommunitySkillRow
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("got %d skills, want 2", len(rows))
		}
		// Ordered by name: alpha then beta.
		if rows[0].Name != "alpha" || rows[1].Name != "beta" {
			t.Errorf("order = %s, %s; want alpha, beta", rows[0].Name, rows[1].Name)
		}
		if rows[0].RAEIndexedAt == "" {
			t.Error("alpha rae_indexed_at missing")
		}
		if rows[1].RAEIndexedAt != "" {
			t.Errorf("beta rae_indexed_at = %q, want empty (NULL column)", rows[1].RAEIndexedAt)
		}
		if !rows[1].DriftDetected || rows[1].Origin != "github" {
			t.Errorf("beta row = %+v", rows[1])
		}
	})

	t.Run("skill subunits", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/rae/skill/"+skillA.String()+"/subunits", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
		}
		var rows []SubunitRow
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d subunits, want 1", len(rows))
		}
		s := rows[0]
		if s.ID != subID.String() || s.Kind != "step" || s.LineNo != 42 || s.EdgeCount != 1 {
			t.Errorf("row = %+v", s)
		}
		if s.IDF < 0.69 || s.IDF > 0.71 {
			t.Errorf("idf = %v, want ~0.7", s.IDF)
		}
	})

	t.Run("skill with no subunits returns empty array", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/rae/skill/"+skillB.String()+"/subunits", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Body.String(); got != "[]\n" {
			t.Errorf("body = %q, want empty JSON array (not null)", got)
		}
	})

	t.Run("unknown community returns empty array", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/rae/community/"+uuid.NewString()+"/skills", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Body.String(); got != "[]\n" {
			t.Errorf("body = %q, want empty JSON array (not null)", got)
		}
	})

	// mux.Router registration sanity: unknown path is 404.
	t.Run("unknown path 404", func(t *testing.T) {
		rec := do(t, r, http.MethodGet, "/v1/rae/nope", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

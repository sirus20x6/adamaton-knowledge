package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/sirus20x6/adamaton-core/pgutil"
)

// store_db_test.go exercises the SQL layer against a real Postgres
// (paradedb container via core/pgutil.NewTestDB — bundles pgvector).
// Docker-gated: skips cleanly when GOGENTS_SKIP_DOCKER_TESTS is set or
// no Docker daemon is reachable.
//
// The schema below mirrors evolve/store/migrations/0006_skills_rae.up.sql
// (+ the later parent/level/tree columns ReplaceCommunities writes),
// with vector(4) instead of vector(1024) so fixtures stay readable.

const testSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS evo;

CREATE TABLE evo.skill_communities (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    label               TEXT NOT NULL,
    representative_text TEXT NOT NULL,
    embedding           vector(4),
    member_count        INT  NOT NULL DEFAULT 0,
    parent_community_id UUID REFERENCES evo.skill_communities(id),
    level               INT  NOT NULL DEFAULT 0,
    tree_id             UUID,
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE evo.skills (
    id                    UUID PRIMARY KEY,
    name                  TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    body                  TEXT NOT NULL DEFAULT '',
    origin                TEXT NOT NULL DEFAULT 'local',
    drift_detected        BOOLEAN NOT NULL DEFAULT FALSE,
    community_id          UUID REFERENCES evo.skill_communities(id),
    community_noise       BOOLEAN NOT NULL DEFAULT FALSE,
    skill_text            TEXT,
    skill_embedding       vector(4),
    description_embedding vector(4),
    rae_indexed_at        TIMESTAMPTZ
);

CREATE TABLE evo.skill_subunits (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content       TEXT NOT NULL,
    content_hash  TEXT NOT NULL UNIQUE,
    kind          TEXT NOT NULL,
    length        INT  NOT NULL,
    idf           REAL NOT NULL DEFAULT 0,
    embedding     vector(4),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE evo.skill_subunit_edges (
    skill_id   UUID NOT NULL REFERENCES evo.skills(id)         ON DELETE CASCADE,
    subunit_id UUID NOT NULL REFERENCES evo.skill_subunits(id) ON DELETE CASCADE,
    line_no    INT,
    PRIMARY KEY (skill_id, subunit_id)
);

CREATE TABLE evo.skill_rae_traces (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id         TEXT NOT NULL,
    query           TEXT NOT NULL,
    variant         TEXT NOT NULL,
    selected_skills UUID[] NOT NULL DEFAULT '{}',
    rescued_count   INT NOT NULL DEFAULT 0,
    elapsed_ms      INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

func dockerAvailable(t *testing.T) bool {
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

func newStoreTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if !dockerAvailable(t) {
		t.Skip("docker not available (or GOGENTS_SKIP_DOCKER_TESTS set); skipping store DB test")
	}
	pool := pgutil.NewTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, testSchemaDDL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func insertSkill(t *testing.T, pool *pgxpool.Pool, name, description, body string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO evo.skills (id, name, description, body) VALUES ($1, $2, $3, $4)`,
		id, name, description, body)
	if err != nil {
		t.Fatalf("insert skill: %v", err)
	}
	return id
}

func TestLoadSkill_AndNotFound(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	id := insertSkill(t, pool, "rename-func", "Rename a function safely", "1. grep\n2. edit")
	s, err := LoadSkill(ctx, pool, id)
	if err != nil {
		t.Fatalf("LoadSkill: %v", err)
	}
	if s.ID != id || s.Name != "rename-func" || s.Description != "Rename a function safely" || s.Body == "" {
		t.Errorf("unexpected row: %+v", s)
	}

	if _, err := LoadSkill(ctx, pool, uuid.New()); err == nil {
		t.Error("LoadSkill(missing) = nil error, want error")
	}
}

func TestListAllSkillIDs_Ordered(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	want := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		want[insertSkill(t, pool, "s", "", "")] = true
	}
	ids, err := ListAllSkillIDs(ctx, pool)
	if err != nil {
		t.Fatalf("ListAllSkillIDs: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("got %d ids, want 3", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1].String() >= ids[i].String() {
			t.Errorf("ids not ordered: %s >= %s", ids[i-1], ids[i])
		}
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id %s", id)
		}
	}
}

func TestStampEmbeddingsAndIndexed(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()
	id := insertSkill(t, pool, "s", "d", "b")

	if err := StampDescriptionEmbedding(ctx, pool, id, []float32{1, 0, 0, 0}); err != nil {
		t.Fatalf("StampDescriptionEmbedding: %v", err)
	}
	if err := StampSkillEmbedding(ctx, pool, id, "composite text", []float32{0, 1, 0, 0}); err != nil {
		t.Fatalf("StampSkillEmbedding: %v", err)
	}
	if err := StampRAEIndexed(ctx, pool, id); err != nil {
		t.Fatalf("StampRAEIndexed: %v", err)
	}

	var text *string
	var descVec, skillVec pgvector.Vector
	var indexed *time.Time
	err := pool.QueryRow(ctx, `
		SELECT skill_text, description_embedding, skill_embedding, rae_indexed_at
		  FROM evo.skills WHERE id = $1`, id).Scan(&text, &descVec, &skillVec, &indexed)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if text == nil || *text != "composite text" {
		t.Errorf("skill_text = %v, want composite text", text)
	}
	if got := descVec.Slice(); len(got) != 4 || got[0] != 1 {
		t.Errorf("description_embedding = %v", got)
	}
	if got := skillVec.Slice(); len(got) != 4 || got[1] != 1 {
		t.Errorf("skill_embedding = %v", got)
	}
	if indexed == nil {
		t.Error("rae_indexed_at not stamped")
	}
}

func TestLoadVectorForAssign_FallbackChain(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()
	id := insertSkill(t, pool, "s", "d", "b")

	// Neither column set → (nil, nil).
	vec, err := LoadVectorForAssign(ctx, pool, id)
	if err != nil {
		t.Fatalf("LoadVectorForAssign(bare): %v", err)
	}
	if vec != nil {
		t.Errorf("expected nil vector for unindexed skill, got %v", vec)
	}

	// Only description → falls back to description_embedding.
	if err := StampDescriptionEmbedding(ctx, pool, id, []float32{0, 0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	vec, err = LoadVectorForAssign(ctx, pool, id)
	if err != nil {
		t.Fatalf("LoadVectorForAssign(desc): %v", err)
	}
	if len(vec) != 4 || vec[2] != 1 {
		t.Errorf("fallback vector = %v, want description embedding", vec)
	}

	// Both set → prefers skill_embedding.
	if err := StampSkillEmbedding(ctx, pool, id, "t", []float32{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	vec, err = LoadVectorForAssign(ctx, pool, id)
	if err != nil {
		t.Fatalf("LoadVectorForAssign(both): %v", err)
	}
	if len(vec) != 4 || vec[3] != 1 {
		t.Errorf("vector = %v, want skill embedding preferred", vec)
	}

	// Missing skill → error.
	if _, err := LoadVectorForAssign(ctx, pool, uuid.New()); err == nil {
		t.Error("LoadVectorForAssign(missing) = nil error, want error")
	}
}

func TestUpsertSubunits_DedupAndEmbeddingBackfill(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	// First insert without embedding (embedder down scenario).
	rows := []SubunitRow{
		{Content: "step one", Hash: "hash-aaaaaaaaaaaa", Kind: "step", Length: 8},
		{Content: "constraint x", Hash: "hash-bbbbbbbbbbbb", Kind: "constraint", Length: 12, Vector: []float32{1, 2, 3, 4}},
	}
	ids1, err := UpsertSubunits(ctx, pool, rows)
	if err != nil {
		t.Fatalf("UpsertSubunits(1): %v", err)
	}
	if len(ids1) != 2 {
		t.Fatalf("got %d ids, want 2", len(ids1))
	}

	// Re-upsert the first with a vector: the NULL embedding must be
	// backfilled (COALESCE branch) and the id must be stable.
	rows2 := []SubunitRow{
		{Content: "step one", Hash: "hash-aaaaaaaaaaaa", Kind: "step", Length: 8, Vector: []float32{9, 9, 9, 9}},
		// And re-upsert the second with a DIFFERENT vector: existing
		// non-NULL embedding must be left alone.
		{Content: "constraint x", Hash: "hash-bbbbbbbbbbbb", Kind: "constraint", Length: 12, Vector: []float32{5, 5, 5, 5}},
	}
	ids2, err := UpsertSubunits(ctx, pool, rows2)
	if err != nil {
		t.Fatalf("UpsertSubunits(2): %v", err)
	}
	if ids2["hash-aaaaaaaaaaaa"] != ids1["hash-aaaaaaaaaaaa"] {
		t.Error("dedup broken: id changed on conflict for hash-aaaa")
	}
	if ids2["hash-bbbbbbbbbbbb"] != ids1["hash-bbbbbbbbbbbb"] {
		t.Error("dedup broken: id changed on conflict for hash-bbbb")
	}

	var vecA, vecB pgvector.Vector
	if err := pool.QueryRow(ctx, `SELECT embedding FROM evo.skill_subunits WHERE content_hash = 'hash-aaaaaaaaaaaa'`).Scan(&vecA); err != nil {
		t.Fatalf("readback A: %v", err)
	}
	if got := vecA.Slice(); got[0] != 9 {
		t.Errorf("NULL embedding not backfilled: %v", got)
	}
	if err := pool.QueryRow(ctx, `SELECT embedding FROM evo.skill_subunits WHERE content_hash = 'hash-bbbbbbbbbbbb'`).Scan(&vecB); err != nil {
		t.Fatalf("readback B: %v", err)
	}
	if got := vecB.Slice(); got[0] != 1 {
		t.Errorf("existing embedding overwritten: %v (want first-write 1,2,3,4)", got)
	}

	// Total row count stays 2 — no duplicates.
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM evo.skill_subunits`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("subunit rows = %d, want 2", n)
	}
}

func TestUpsertSubunits_Empty(t *testing.T) {
	pool := newStoreTestDB(t)
	out, err := UpsertSubunits(context.Background(), pool, nil)
	if err != nil {
		t.Fatalf("UpsertSubunits(nil): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %v", out)
	}
}

func TestEdges_UpsertDeleteAndLoad(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()
	skill := insertSkill(t, pool, "s", "d", "b")

	ids, err := UpsertSubunits(ctx, pool, []SubunitRow{
		{Content: "u1", Hash: "h1", Kind: "step", Length: 2, Vector: []float32{1, 0, 0, 0}},
		{Content: "u2", Hash: "h2", Kind: "constraint", Length: 2, Vector: []float32{0, 1, 0, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	subIDs := []uuid.UUID{ids["h1"], ids["h2"]}

	// Mismatched lengths must error before touching the DB.
	if err := UpsertEdges(ctx, pool, skill, subIDs, []int{1}); err == nil {
		t.Error("UpsertEdges(mismatch) = nil error, want error")
	}
	// Empty set is a no-op.
	if err := UpsertEdges(ctx, pool, skill, nil, nil); err != nil {
		t.Errorf("UpsertEdges(empty): %v", err)
	}

	if err := UpsertEdges(ctx, pool, skill, subIDs, []int{10, 20}); err != nil {
		t.Fatalf("UpsertEdges: %v", err)
	}
	// Idempotent: re-run with different line numbers → ON CONFLICT DO
	// NOTHING keeps the original line_no.
	if err := UpsertEdges(ctx, pool, skill, subIDs, []int{99, 98}); err != nil {
		t.Fatalf("UpsertEdges(again): %v", err)
	}
	var lineNo int
	if err := pool.QueryRow(ctx, `SELECT line_no FROM evo.skill_subunit_edges WHERE skill_id = $1 AND subunit_id = $2`, skill, ids["h1"]).Scan(&lineNo); err != nil {
		t.Fatal(err)
	}
	if lineNo != 10 {
		t.Errorf("line_no = %d, want original 10 (edges must not flap)", lineNo)
	}

	subs, err := LoadSubunitsForSkill(ctx, pool, skill)
	if err != nil {
		t.Fatalf("LoadSubunitsForSkill: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("got %d subunits, want 2", len(subs))
	}
	for _, s := range subs {
		if s.DegreeU != 1 {
			t.Errorf("subunit %s degree = %d, want 1", s.ID, s.DegreeU)
		}
	}

	if err := DeleteEdgesForSkill(ctx, pool, skill); err != nil {
		t.Fatalf("DeleteEdgesForSkill: %v", err)
	}
	subs, err = LoadSubunitsForSkill(ctx, pool, skill)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 0 {
		t.Errorf("edges remain after delete: %d", len(subs))
	}
	// Subunit rows themselves survive (global dedup).
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM evo.skill_subunits`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("subunits deleted along with edges: %d rows, want 2", n)
	}
}

func TestLoadSubunitVectors(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	ids, err := UpsertSubunits(ctx, pool, []SubunitRow{
		{Content: "with-vec", Hash: "hv", Kind: "step", Length: 1, Vector: []float32{1, 2, 3, 4}},
		{Content: "no-vec", Hash: "hn", Kind: "step", Length: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := LoadSubunitVectors(ctx, pool, []uuid.UUID{ids["hv"], ids["hn"], uuid.New()})
	if err != nil {
		t.Fatalf("LoadSubunitVectors: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d vectors, want 1 (NULL + missing rows silently absent)", len(out))
	}
	if v := out[ids["hv"]]; len(v) != 4 || v[0] != 1 || v[3] != 4 {
		t.Errorf("vector = %v", v)
	}

	empty, err := LoadSubunitVectors(ctx, pool, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("LoadSubunitVectors(nil) = %v, %v", empty, err)
	}
}

func TestRecomputeIDF(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	s1 := insertSkill(t, pool, "s1", "", "")
	s2 := insertSkill(t, pool, "s2", "", "")

	ids, err := UpsertSubunits(ctx, pool, []SubunitRow{
		{Content: "shared", Hash: "h-shared", Kind: "step", Length: 1},
		{Content: "unique", Hash: "h-unique", Kind: "step", Length: 1},
		{Content: "orphan", Hash: "h-orphan", Kind: "step", Length: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	// shared: attached to both skills (df=2, N=2 → idf = ln(1) = 0).
	// unique: attached to one skill (df=1, N=2 → idf = ln(2) ≈ 0.693).
	// orphan: zero edges → keeps DEFAULT idf = 0.
	if err := UpsertEdges(ctx, pool, s1, []uuid.UUID{ids["h-shared"], ids["h-unique"]}, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEdges(ctx, pool, s2, []uuid.UUID{ids["h-shared"]}, []int{1}); err != nil {
		t.Fatal(err)
	}

	if err := RecomputeIDF(ctx, pool); err != nil {
		t.Fatalf("RecomputeIDF: %v", err)
	}

	idf := func(hash string) float64 {
		var v float64
		if err := pool.QueryRow(ctx, `SELECT idf FROM evo.skill_subunits WHERE content_hash = $1`, hash).Scan(&v); err != nil {
			t.Fatalf("read idf %s: %v", hash, err)
		}
		return v
	}
	if got := idf("h-shared"); got != 0 {
		t.Errorf("idf(shared) = %v, want 0", got)
	}
	if got := idf("h-unique"); got < 0.69 || got > 0.70 {
		t.Errorf("idf(unique) = %v, want ~ln(2)", got)
	}
	if got := idf("h-orphan"); got != 0 {
		t.Errorf("idf(orphan) = %v, want default 0", got)
	}
}

func TestInsertTrace(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	if err := InsertTrace(ctx, nil, "t", "q", "rae", nil, 0, 0); err == nil {
		t.Error("InsertTrace(nil pool) = nil error, want error")
	}

	sel := []uuid.UUID{uuid.New(), uuid.New()}
	if err := InsertTrace(ctx, pool, "task-1", "how do I rename", "rae", sel, 2, 145); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}
	var (
		query, variant string
		rescued, ms    int
		selBack        []uuid.UUID
	)
	err := pool.QueryRow(ctx, `
		SELECT query, variant, selected_skills, rescued_count, elapsed_ms
		  FROM evo.skill_rae_traces WHERE task_id = 'task-1'`).
		Scan(&query, &variant, &selBack, &rescued, &ms)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if query != "how do I rename" || variant != "rae" || rescued != 2 || ms != 145 || len(selBack) != 2 {
		t.Errorf("trace row mismatch: %q %q %v %d %d", query, variant, selBack, rescued, ms)
	}
}

func TestReplaceCommunities_AtomicSwap(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	s1 := insertSkill(t, pool, "s1", "", "")
	s2 := insertSkill(t, pool, "s2", "", "")
	s3 := insertSkill(t, pool, "s3", "", "")

	// Seed an old community + assignment that the swap must clear.
	oldComm := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_communities (id, label, representative_text, embedding, member_count)
		VALUES ($1, 'old', 'old text', $2, 3)`, oldComm, pgvector.NewVector([]float32{0, 0, 0, 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE evo.skills SET community_id = $1 WHERE id = $2`, oldComm, s3); err != nil {
		t.Fatal(err)
	}

	// New tree: one root (level 0) + one leaf (level 1). Pass them
	// deliberately in child-first order to prove level-sorting works.
	root := uuid.New()
	leaf := uuid.New()
	tree := uuid.New()
	comms := []CommunityRow{
		{ID: leaf, ParentID: &root, Level: 1, TreeID: tree, Label: "leaf", RepresentativeText: "leaf text", Embedding: []float32{1, 0, 0, 0}, MemberCount: 2},
		{ID: root, Level: 0, TreeID: tree, Label: "root", RepresentativeText: "root text", Embedding: []float32{0, 1, 0, 0}, MemberCount: 2},
	}
	assignments := map[uuid.UUID]uuid.UUID{s1: leaf, s2: leaf}
	noise := []uuid.UUID{s3}

	if err := ReplaceCommunities(ctx, pool, comms, assignments, noise); err != nil {
		t.Fatalf("ReplaceCommunities: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM evo.skill_communities`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("communities = %d, want 2 (old set deleted)", n)
	}

	var cid *uuid.UUID
	var isNoise bool
	if err := pool.QueryRow(ctx, `SELECT community_id, community_noise FROM evo.skills WHERE id = $1`, s1).Scan(&cid, &isNoise); err != nil {
		t.Fatal(err)
	}
	if cid == nil || *cid != leaf || isNoise {
		t.Errorf("s1 assignment = %v noise=%v, want leaf %s noise=false", cid, isNoise, leaf)
	}
	if err := pool.QueryRow(ctx, `SELECT community_id, community_noise FROM evo.skills WHERE id = $1`, s3).Scan(&cid, &isNoise); err != nil {
		t.Fatal(err)
	}
	if cid != nil || !isNoise {
		t.Errorf("s3 = (%v, noise=%v), want (NULL, true)", cid, isNoise)
	}

	// Leaf reps: MAX(level)=1, so only the leaf qualifies.
	reps, err := ListLeafCommunityReps(ctx, pool)
	if err != nil {
		t.Fatalf("ListLeafCommunityReps: %v", err)
	}
	if len(reps) != 1 || reps[0].ID != leaf {
		t.Fatalf("leaf reps = %+v, want just %s", reps, leaf)
	}
	if len(reps[0].Embedding) != 4 || reps[0].Embedding[0] != 1 {
		t.Errorf("leaf rep embedding = %v", reps[0].Embedding)
	}
}

func TestStampCommunityAssignment(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	s := insertSkill(t, pool, "s", "", "")
	comm := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_communities (id, label, representative_text, embedding)
		VALUES ($1, 'c', 't', $2)`, comm, pgvector.NewVector([]float32{1, 1, 1, 1})); err != nil {
		t.Fatal(err)
	}

	if err := StampCommunityAssignment(ctx, pool, s, &comm, false); err != nil {
		t.Fatalf("StampCommunityAssignment(match): %v", err)
	}
	var cid *uuid.UUID
	var noise bool
	if err := pool.QueryRow(ctx, `SELECT community_id, community_noise FROM evo.skills WHERE id = $1`, s).Scan(&cid, &noise); err != nil {
		t.Fatal(err)
	}
	if cid == nil || *cid != comm || noise {
		t.Errorf("got (%v, %v), want (%s, false)", cid, noise, comm)
	}

	if err := StampCommunityAssignment(ctx, pool, s, nil, true); err != nil {
		t.Fatalf("StampCommunityAssignment(noise): %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT community_id, community_noise FROM evo.skills WHERE id = $1`, s).Scan(&cid, &noise); err != nil {
		t.Fatal(err)
	}
	if cid != nil || !noise {
		t.Errorf("got (%v, %v), want (NULL, true)", cid, noise)
	}
}

func TestAssignCommunity(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	s := insertSkill(t, pool, "s", "", "")
	comm := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_communities (id, label, representative_text, embedding)
		VALUES ($1, 'c', 't', $2)`, comm, pgvector.NewVector([]float32{1, 0, 0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := AssignCommunity(ctx, pool, s, comm); err != nil {
		t.Fatalf("AssignCommunity: %v", err)
	}
	var cid uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT community_id FROM evo.skills WHERE id = $1`, s).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if cid != comm {
		t.Errorf("community_id = %s, want %s", cid, comm)
	}
}

func TestSearchQueries(t *testing.T) {
	pool := newStoreTestDB(t)
	ctx := context.Background()

	// Two skills with orthogonal description embeddings.
	s1 := insertSkill(t, pool, "near", "close to query", "")
	s2 := insertSkill(t, pool, "far", "far from query", "")
	if err := StampDescriptionEmbedding(ctx, pool, s1, []float32{1, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := StampDescriptionEmbedding(ctx, pool, s2, []float32{0, 1, 0, 0}); err != nil {
		t.Fatal(err)
	}

	comm := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_communities (id, label, representative_text, embedding, member_count)
		VALUES ($1, 'c1', 't', $2, 1)`, comm, pgvector.NewVector([]float32{1, 0, 0, 0})); err != nil {
		t.Fatal(err)
	}
	if err := AssignCommunity(ctx, pool, s1, comm); err != nil {
		t.Fatal(err)
	}

	subIDs, err := UpsertSubunits(ctx, pool, []SubunitRow{
		{Content: "aligned subunit", Hash: "sh1", Kind: "step", Length: 3, Vector: []float32{1, 0, 0, 0}},
		{Content: "orthogonal subunit", Hash: "sh2", Kind: "step", Length: 3, Vector: []float32{0, 0, 1, 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := UpsertEdges(ctx, pool, s1, []uuid.UUID{subIDs["sh1"]}, []int{1}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertEdges(ctx, pool, s2, []uuid.UUID{subIDs["sh1"], subIDs["sh2"]}, []int{4, 5}); err != nil {
		t.Fatal(err)
	}

	query := []float32{1, 0, 0, 0}

	t.Run("TopDownCommunities", func(t *testing.T) {
		matches, err := TopDownCommunities(ctx, pool, query, 0) // n<=0 → default 2
		if err != nil {
			t.Fatalf("TopDownCommunities: %v", err)
		}
		if len(matches) != 1 {
			t.Fatalf("got %d matches, want 1", len(matches))
		}
		m := matches[0]
		if m.ID != comm || m.Similarity < 0.999 {
			t.Errorf("match = %+v, want community %s sim≈1", m, comm)
		}
		if len(m.Members) != 1 || m.Members[0] != s1 {
			t.Errorf("members = %v, want [%s]", m.Members, s1)
		}
	})

	t.Run("BottomUpSubunits", func(t *testing.T) {
		hits, err := BottomUpSubunits(ctx, pool, query, 0) // topN<=0 → default 50
		if err != nil {
			t.Fatalf("BottomUpSubunits: %v", err)
		}
		// sh1 has two edges → 2 rows; sh2 one edge → 1 row.
		if len(hits) != 3 {
			t.Fatalf("got %d hits, want 3 (edge expansion)", len(hits))
		}
		if hits[0].Similarity < hits[len(hits)-1].Similarity {
			t.Errorf("hits not ordered by similarity desc: first=%v last=%v",
				hits[0].Similarity, hits[len(hits)-1].Similarity)
		}
		degByID := map[uuid.UUID]int{}
		for _, h := range hits {
			degByID[h.ID] = h.Degree
		}
		if degByID[subIDs["sh1"]] != 2 {
			t.Errorf("deg(sh1) = %d, want 2", degByID[subIDs["sh1"]])
		}
		if degByID[subIDs["sh2"]] != 1 {
			t.Errorf("deg(sh2) = %d, want 1", degByID[subIDs["sh2"]])
		}
		if hits[0].ID != subIDs["sh1"] {
			t.Errorf("top hit = %s, want aligned subunit %s", hits[0].ID, subIDs["sh1"])
		}
	})

	t.Run("SkillL1", func(t *testing.T) {
		hits, err := SkillL1(ctx, pool, query, 0) // topN<=0 → default 50
		if err != nil {
			t.Fatalf("SkillL1: %v", err)
		}
		if len(hits) != 2 {
			t.Fatalf("got %d hits, want 2", len(hits))
		}
		if hits[0].ID != s1 || hits[0].L1 < 0.999 {
			t.Errorf("top L1 hit = %+v, want %s at sim≈1", hits[0], s1)
		}
		if hits[0].CommunityID == nil || *hits[0].CommunityID != comm {
			t.Errorf("top hit community = %v, want %s", hits[0].CommunityID, comm)
		}
		if hits[1].ID != s2 {
			t.Errorf("second hit = %s, want %s", hits[1].ID, s2)
		}
	})
}

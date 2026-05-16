package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// CommunityMatch is one top-down hit: a community + its cosine sim
// against the query embedding.
type CommunityMatch struct {
	ID         uuid.UUID
	Label      string
	Similarity float64
	Members    []uuid.UUID
}

// TopDownCommunities returns the highest-scoring N communities and
// their member skill IDs. ``n`` is typically small (paper uses 2).
// Cosine distance via pgvector's ``<=>`` operator; similarity = 1 -
// distance.
//
// Implementation: two queries instead of a single JOIN because at our
// corpus size the second query is one indexed-IN lookup per match
// and the planner picks better stats with split queries.
func TopDownCommunities(ctx context.Context, pool *pgxpool.Pool, queryVec []float32, n int) ([]CommunityMatch, error) {
	if n <= 0 {
		n = 2
	}
	rows, err := pool.Query(ctx, `
		SELECT id, label, 1 - (embedding <=> $1) AS sim
		  FROM evo.skill_communities
		 WHERE embedding IS NOT NULL
		 ORDER BY embedding <=> $1
		 LIMIT $2`, pgvector.NewVector(queryVec), n)
	if err != nil {
		return nil, fmt.Errorf("topdown communities: %w", err)
	}
	defer rows.Close()
	var out []CommunityMatch
	for rows.Next() {
		var m CommunityMatch
		if err := rows.Scan(&m.ID, &m.Label, &m.Similarity); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fetch members per community. Could batch but n is tiny.
	for i, m := range out {
		mrows, err := pool.Query(ctx, `SELECT id FROM evo.skills WHERE community_id = $1`, m.ID)
		if err != nil {
			return nil, fmt.Errorf("topdown members %s: %w", m.ID, err)
		}
		var members []uuid.UUID
		for mrows.Next() {
			var id uuid.UUID
			if err := mrows.Scan(&id); err != nil {
				mrows.Close()
				return nil, err
			}
			members = append(members, id)
		}
		mrows.Close()
		out[i].Members = members
	}
	return out, nil
}

// SubunitHit is one bottom-up subunit candidate carrying its source
// skill ID + similarity + degree (deg(u)).
type SubunitHit struct {
	ID         uuid.UUID
	SkillID    uuid.UUID
	Content    string
	Kind       string
	Similarity float64
	Degree     int
}

// BottomUpSubunits returns top-N subunits by similarity to the query
// vector. Returns one row per (subunit, skill) edge so the caller
// can group/project to skills. ``topN`` caps the subunit set BEFORE
// edge expansion — at deg(u)=2 avg, topN=50 produces ~100 rows.
//
// We join eagerly to expand edges in SQL rather than splitting into
// two queries: at our scale the JOIN is cheap and saves one round-
// trip. The composite (subunit_id, skill_id) index makes the JOIN an
// index-only scan.
func BottomUpSubunits(ctx context.Context, pool *pgxpool.Pool, queryVec []float32, topN int) ([]SubunitHit, error) {
	if topN <= 0 {
		topN = 50
	}
	rows, err := pool.Query(ctx, `
		WITH top_subunits AS (
		    SELECT id, content, kind, 1 - (embedding <=> $1) AS sim
		      FROM evo.skill_subunits
		     WHERE embedding IS NOT NULL
		     ORDER BY embedding <=> $1
		     LIMIT $2
		),
		degrees AS (
		    SELECT subunit_id, COUNT(*) AS deg
		      FROM evo.skill_subunit_edges
		     WHERE subunit_id IN (SELECT id FROM top_subunits)
		     GROUP BY subunit_id
		)
		SELECT ts.id, e.skill_id, ts.content, ts.kind, ts.sim, COALESCE(d.deg, 1)
		  FROM top_subunits ts
		  JOIN evo.skill_subunit_edges e ON e.subunit_id = ts.id
		  LEFT JOIN degrees d ON d.subunit_id = ts.id
		 ORDER BY ts.sim DESC`,
		pgvector.NewVector(queryVec), topN)
	if err != nil {
		return nil, fmt.Errorf("bottomup subunits: %w", err)
	}
	defer rows.Close()
	var out []SubunitHit
	for rows.Next() {
		var h SubunitHit
		if err := rows.Scan(&h.ID, &h.SkillID, &h.Content, &h.Kind, &h.Similarity, &h.Degree); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SkillL1Hit is one ℓ₁ result: per-skill cosine similarity between
// the query and the description_embedding.
type SkillL1Hit struct {
	ID          uuid.UUID
	Name        string
	Description string
	CommunityID *uuid.UUID
	L1          float64
}

// SkillL1 returns top-N skills by description-embedding cosine sim.
// Skills without a description_embedding (i.e. not yet indexed)
// don't show up here — the indexer must run first.
func SkillL1(ctx context.Context, pool *pgxpool.Pool, queryVec []float32, topN int) ([]SkillL1Hit, error) {
	if topN <= 0 {
		topN = 50
	}
	rows, err := pool.Query(ctx, `
		SELECT id, name, description, community_id, 1 - (description_embedding <=> $1) AS l1
		  FROM evo.skills
		 WHERE description_embedding IS NOT NULL
		 ORDER BY description_embedding <=> $1
		 LIMIT $2`, pgvector.NewVector(queryVec), topN)
	if err != nil {
		return nil, fmt.Errorf("skill l1: %w", err)
	}
	defer rows.Close()
	var out []SkillL1Hit
	for rows.Next() {
		var h SkillL1Hit
		if err := rows.Scan(&h.ID, &h.Name, &h.Description, &h.CommunityID, &h.L1); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

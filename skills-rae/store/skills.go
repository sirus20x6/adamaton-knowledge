package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// SkillRow is the load shape used by IndexSkillWorkflow. We only pull
// the columns the indexer needs — body for subunit extraction,
// description for the description_embedding. The full row (R2R refs,
// tags, etc.) is not relevant here.
type SkillRow struct {
	ID          uuid.UUID
	Name        string
	Description string
	Body        string
}

// LoadSkill fetches the indexable view of one skill.
func LoadSkill(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (*SkillRow, error) {
	row := pool.QueryRow(ctx, `
		SELECT id, name, description, body
		  FROM evo.skills
		 WHERE id = $1`, id)
	var s SkillRow
	if err := row.Scan(&s.ID, &s.Name, &s.Description, &s.Body); err != nil {
		return nil, fmt.Errorf("load skill %s: %w", id, err)
	}
	return &s, nil
}

// ListAllSkillIDs returns every skill UUID. Used by ReindexAll to
// build the work-set.
func ListAllSkillIDs(ctx context.Context, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM evo.skills ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list skill ids: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SkillForClustering is the per-skill view used by RecomputeCommunities.
// embedding is non-nil only when skill_embedding has been computed
// earlier in the same workflow run.
type SkillForClustering struct {
	ID          uuid.UUID
	Name        string
	Description string
	SkillText   string
	Embedding   []float32
}

// ListSkillsForClustering returns every skill plus its current
// skill_text (may be empty until first cluster pass). Used by
// RecomputeCommunities to rebuild skill_text + skill_embedding from
// scratch, so we discard the existing values and re-fetch description.
func ListSkillsForClustering(ctx context.Context, pool *pgxpool.Pool) ([]SkillForClustering, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, description
		  FROM evo.skills
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list skills for clustering: %w", err)
	}
	defer rows.Close()
	var out []SkillForClustering
	for rows.Next() {
		var s SkillForClustering
		if err := rows.Scan(&s.ID, &s.Name, &s.Description); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StampDescriptionEmbedding writes the description_embedding column
// for one skill. Used by IndexSkillWorkflow.
func StampDescriptionEmbedding(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, vec []float32) error {
	_, err := pool.Exec(ctx, `
		UPDATE evo.skills
		   SET description_embedding = $2
		 WHERE id = $1`, id, pgvector.NewVector(vec))
	if err != nil {
		return fmt.Errorf("stamp description_embedding %s: %w", id, err)
	}
	return nil
}

// StampSkillEmbedding writes skill_text + skill_embedding. Used by
// RecomputeCommunities once per skill.
func StampSkillEmbedding(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, text string, vec []float32) error {
	_, err := pool.Exec(ctx, `
		UPDATE evo.skills
		   SET skill_text = $2,
		       skill_embedding = $3
		 WHERE id = $1`, id, text, pgvector.NewVector(vec))
	if err != nil {
		return fmt.Errorf("stamp skill_embedding %s: %w", id, err)
	}
	return nil
}

// StampRAEIndexed marks a skill as indexed at NOW(). Final step of
// IndexSkillWorkflow.
func StampRAEIndexed(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) error {
	_, err := pool.Exec(ctx, `
		UPDATE evo.skills SET rae_indexed_at = $2 WHERE id = $1`,
		id, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("stamp rae_indexed_at %s: %w", id, err)
	}
	return nil
}

// AssignCommunity stamps the community_id on a skill.
func AssignCommunity(ctx context.Context, pool *pgxpool.Pool, skillID, communityID uuid.UUID) error {
	_, err := pool.Exec(ctx, `
		UPDATE evo.skills SET community_id = $2 WHERE id = $1`,
		skillID, communityID)
	if err != nil {
		return fmt.Errorf("assign community %s -> %s: %w", skillID, communityID, err)
	}
	return nil
}

// LoadVectorForAssign returns the vector AssignToNearestCommunity
// uses to cosine against community representatives. It prefers
// skill_embedding (the richer signal that includes top-IDF subunits)
// but falls back to description_embedding when skill_embedding is
// NULL — which is the common case at insert time, since
// IndexSkillWorkflow populates description_embedding but skill_embedding
// is only written by the periodic RecomputeCommunities pass.
//
// Returns (nil, nil) when both columns are NULL (skill not yet
// indexed at all). Callers treat that as "skip assignment" rather
// than an error.
func LoadVectorForAssign(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) ([]float32, error) {
	var skillVec, descVec *pgvector.Vector
	err := pool.QueryRow(ctx, `
		SELECT skill_embedding, description_embedding
		  FROM evo.skills
		 WHERE id = $1`, id).Scan(&skillVec, &descVec)
	if err != nil {
		return nil, fmt.Errorf("load assign vector %s: %w", id, err)
	}
	if skillVec != nil {
		return skillVec.Slice(), nil
	}
	if descVec != nil {
		return descVec.Slice(), nil
	}
	return nil, nil
}

// CommunityRep is one leaf-community representative used by the
// AssignToNearestCommunity activity. Loaded once per assign call.
type CommunityRep struct {
	ID        uuid.UUID
	Embedding []float32
}

// ListLeafCommunityReps returns every leaf-level community's id +
// embedding. "Leaf" is the maximum level in the current tree — we
// pick that off the table at query time so adding a fourth level later
// doesn't require code changes here.
func ListLeafCommunityReps(ctx context.Context, pool *pgxpool.Pool) ([]CommunityRep, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, embedding
		  FROM evo.skill_communities
		 WHERE level = (SELECT COALESCE(MAX(level), 0) FROM evo.skill_communities)
		   AND embedding IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list leaf communities: %w", err)
	}
	defer rows.Close()
	var out []CommunityRep
	for rows.Next() {
		var rep CommunityRep
		var vec pgvector.Vector
		if err := rows.Scan(&rep.ID, &vec); err != nil {
			return nil, err
		}
		rep.Embedding = vec.Slice()
		out = append(out, rep)
	}
	return out, rows.Err()
}

// StampCommunityAssignment writes community_id + community_noise in a
// single UPDATE. Pass communityID = nil + noise = true for a skill the
// assigner decided was noise; pass communityID = &id + noise = false
// for a confident match.
func StampCommunityAssignment(ctx context.Context, pool *pgxpool.Pool, skillID uuid.UUID, communityID *uuid.UUID, noise bool) error {
	_, err := pool.Exec(ctx, `
		UPDATE evo.skills
		   SET community_id    = $2,
		       community_noise = $3
		 WHERE id = $1`, skillID, communityID, noise)
	if err != nil {
		return fmt.Errorf("stamp community assignment %s: %w", skillID, err)
	}
	return nil
}

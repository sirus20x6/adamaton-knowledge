package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// CommunityRow is the insert payload for one freshly-computed
// community. Computed in-memory after the sidecar returns its
// hierarchy, then bulk-inserted inside a transaction that also nulls
// out every skill's existing community_id before reassigning.
//
// ParentID may be nil at the root level (level == 0). The caller must
// insert rows in level order (parents before children) so the FK on
// parent_community_id is satisfied — ReplaceCommunities does this.
type CommunityRow struct {
	ID                 uuid.UUID
	ParentID           *uuid.UUID
	Level              int
	TreeID             uuid.UUID
	Label              string
	RepresentativeText string
	Embedding          []float32
	MemberCount        int
}

// ReplaceCommunities atomically nulls every skill's community_id,
// deletes all communities, inserts the new set (in level order so
// parent FKs are satisfied), and stamps the new assignments + noise
// flags. Wrapped in a single tx so a retrieval call mid-rebuild
// never observes a partial state. The transaction is short — even
// at 100 communities + 10k skills it's a few hundred millis.
//
// noiseSkillIDs is the set of skills that the clusterer flagged as
// noise (HDBSCAN label == -1 at the leaf level). They get
// community_id = NULL and community_noise = TRUE; everyone else gets
// the assignments-map value and community_noise = FALSE.
func ReplaceCommunities(
	ctx context.Context, pool *pgxpool.Pool,
	communities []CommunityRow,
	assignments map[uuid.UUID]uuid.UUID,
	noiseSkillIDs []uuid.UUID,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `UPDATE evo.skills SET community_id = NULL, community_noise = FALSE`); err != nil {
		return fmt.Errorf("null community_id: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM evo.skill_communities`); err != nil {
		return fmt.Errorf("delete communities: %w", err)
	}

	// Insert in level order so parent_community_id FKs are satisfied.
	// communities is mutated to a sorted copy locally; caller's slice
	// order is preserved.
	if len(communities) > 0 {
		ordered := append([]CommunityRow(nil), communities...)
		for i := range ordered {
			for j := i + 1; j < len(ordered); j++ {
				if ordered[j].Level < ordered[i].Level {
					ordered[i], ordered[j] = ordered[j], ordered[i]
				}
			}
		}
		batch := &pgx.Batch{}
		for _, c := range ordered {
			batch.Queue(`
				INSERT INTO evo.skill_communities
				    (id, label, representative_text, embedding, member_count,
				     parent_community_id, level, tree_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				c.ID, c.Label, c.RepresentativeText,
				pgvector.NewVector(c.Embedding), c.MemberCount,
				c.ParentID, c.Level, c.TreeID)
		}
		br := tx.SendBatch(ctx, batch)
		for range ordered {
			if _, err := br.Exec(); err != nil {
				br.Close() //nolint:errcheck
				return fmt.Errorf("insert community: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("community batch close: %w", err)
		}
	}

	if len(assignments) > 0 {
		batch := &pgx.Batch{}
		for skillID, cid := range assignments {
			batch.Queue(`UPDATE evo.skills SET community_id = $2 WHERE id = $1`, skillID, cid)
		}
		br := tx.SendBatch(ctx, batch)
		for range assignments {
			if _, err := br.Exec(); err != nil {
				br.Close() //nolint:errcheck
				return fmt.Errorf("assign community: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("assign batch close: %w", err)
		}
	}

	if len(noiseSkillIDs) > 0 {
		batch := &pgx.Batch{}
		for _, sid := range noiseSkillIDs {
			batch.Queue(`UPDATE evo.skills SET community_noise = TRUE WHERE id = $1`, sid)
		}
		br := tx.SendBatch(ctx, batch)
		for range noiseSkillIDs {
			if _, err := br.Exec(); err != nil {
				br.Close() //nolint:errcheck
				return fmt.Errorf("mark noise: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("noise batch close: %w", err)
		}
	}

	return tx.Commit(ctx)
}

package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// SubunitRow is the per-row insert payload. ID is filled by the DB
// (on insert) OR fetched from the existing row (on conflict).
type SubunitRow struct {
	ID      uuid.UUID
	Content string
	Hash    string
	Kind    string
	Length  int
	LineNo  int
	Vector  []float32 // optional; nil = leave embedding NULL on insert
}

// UpsertSubunits inserts new subunits and returns the id for each
// input by hash. ON CONFLICT (content_hash) DO UPDATE on embedding
// when the existing row has NULL — that fixes the case where an
// earlier indexer run wrote a row without an embedding (e.g. the
// embedder was down). Otherwise existing rows are left alone.
//
// Returns ``map[hash]uuid`` so the caller (UpsertEdges) can build
// edge rows without a second SELECT.
func UpsertSubunits(ctx context.Context, pool *pgxpool.Pool, rows []SubunitRow) (map[string]uuid.UUID, error) {
	if len(rows) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Batched insert using a single statement per row through pgx
	// SendBatch. Cheaper than N round-trips for a 30-subunit skill.
	batch := &pgx.Batch{}
	for _, r := range rows {
		if r.Vector != nil {
			batch.Queue(`
				INSERT INTO evo.skill_subunits (content, content_hash, kind, length, embedding)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (content_hash) DO UPDATE
				   SET embedding = COALESCE(evo.skill_subunits.embedding, EXCLUDED.embedding)
				RETURNING id`,
				r.Content, r.Hash, r.Kind, r.Length, pgvector.NewVector(r.Vector))
		} else {
			batch.Queue(`
				INSERT INTO evo.skill_subunits (content, content_hash, kind, length)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (content_hash) DO UPDATE
				   SET content_hash = EXCLUDED.content_hash
				RETURNING id`,
				r.Content, r.Hash, r.Kind, r.Length)
		}
	}
	br := tx.SendBatch(ctx, batch)
	out := make(map[string]uuid.UUID, len(rows))
	for _, r := range rows {
		var id uuid.UUID
		if err := br.QueryRow().Scan(&id); err != nil {
			br.Close() //nolint:errcheck
			return nil, fmt.Errorf("upsert subunit %s: %w", r.Hash[:12], err)
		}
		out[r.Hash] = id
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("batch close: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// DeleteEdgesForSkill removes every edge belonging to a skill. Run
// before UpsertEdges during a reindex so removed subunits don't keep
// stale edges. We don't delete the subunit row itself — global dedup
// means it may still be referenced by other skills.
func DeleteEdgesForSkill(ctx context.Context, pool *pgxpool.Pool, skillID uuid.UUID) error {
	_, err := pool.Exec(ctx, `DELETE FROM evo.skill_subunit_edges WHERE skill_id = $1`, skillID)
	if err != nil {
		return fmt.Errorf("delete edges for skill %s: %w", skillID, err)
	}
	return nil
}

// UpsertEdges writes (skill_id, subunit_id, line_no) tuples. ON
// CONFLICT DO NOTHING because line_no is best-effort metadata; we
// don't want a re-index to flap line numbers on every run.
func UpsertEdges(ctx context.Context, pool *pgxpool.Pool, skillID uuid.UUID, subunitIDs []uuid.UUID, lineNos []int) error {
	if len(subunitIDs) != len(lineNos) {
		return fmt.Errorf("mismatched ids (%d) and lineNos (%d)", len(subunitIDs), len(lineNos))
	}
	if len(subunitIDs) == 0 {
		return nil
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	batch := &pgx.Batch{}
	for i, sid := range subunitIDs {
		batch.Queue(`
			INSERT INTO evo.skill_subunit_edges (skill_id, subunit_id, line_no)
			VALUES ($1, $2, $3)
			ON CONFLICT (skill_id, subunit_id) DO NOTHING`,
			skillID, sid, lineNos[i])
	}
	br := tx.SendBatch(ctx, batch)
	for range subunitIDs {
		if _, err := br.Exec(); err != nil {
			br.Close() //nolint:errcheck
			return fmt.Errorf("upsert edge: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("batch close: %w", err)
	}
	return tx.Commit(ctx)
}

// SubunitWithEdges is the per-subunit summary used to build a
// skill's skill_text. Returned ordered by IDF desc so the caller
// can take the top-K directly.
type SubunitWithEdges struct {
	ID      uuid.UUID
	Content string
	Kind    string
	IDF     float64
	DegreeU int
}

// LoadSubunitsForSkill returns the subunits attached to ``skillID``,
// each with its current global IDF. Ordered by IDF descending so the
// caller takes top-K to build the skill_text.
func LoadSubunitsForSkill(ctx context.Context, pool *pgxpool.Pool, skillID uuid.UUID) ([]SubunitWithEdges, error) {
	rows, err := pool.Query(ctx, `
		SELECT u.id, u.content, u.kind, u.idf,
		       (SELECT COUNT(*) FROM evo.skill_subunit_edges WHERE subunit_id = u.id)
		  FROM evo.skill_subunit_edges e
		  JOIN evo.skill_subunits u ON u.id = e.subunit_id
		 WHERE e.skill_id = $1
		 ORDER BY u.idf DESC, u.length ASC`, skillID)
	if err != nil {
		return nil, fmt.Errorf("load subunits for skill %s: %w", skillID, err)
	}
	defer rows.Close()
	var out []SubunitWithEdges
	for rows.Next() {
		var r SubunitWithEdges
		if err := rows.Scan(&r.ID, &r.Content, &r.Kind, &r.IDF, &r.DegreeU); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadSubunitVectors returns the bge-m3 vector for each subunit in
// ``ids``. Used by the compile pipeline to compute real cosine
// similarity for the Aff features and rescue-dedup, instead of the
// Jaccard token-overlap stand-in we shipped in v1.
//
// Missing rows are silently absent from the returned map -- the
// caller decides whether to skip those subunits or fall back to a
// token-based score.
func LoadSubunitVectors(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) (map[uuid.UUID][]float32, error) {
	out := make(map[uuid.UUID][]float32, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT id, embedding FROM evo.skill_subunits WHERE id = ANY($1) AND embedding IS NOT NULL`, ids)
	if err != nil {
		return nil, fmt.Errorf("load subunit vectors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var vec pgvector.Vector
		if err := rows.Scan(&id, &vec); err != nil {
			return nil, err
		}
		out[id] = vec.Slice()
	}
	return out, rows.Err()
}

// RecomputeIDF rewrites evo.skill_subunits.idf based on the current
// edge graph. IDF = log(N / df) where N is total skills and df is the
// number of distinct skills the subunit is attached to. Cheap (single
// UPDATE; runs against the edge counts).
//
// Postgres quirk: ``UPDATE t FROM x LEFT JOIN y ON y.col = t.col`` is
// rejected (the UPDATE target can't be referenced inside the FROM's
// JOIN ON clause). We use a single subquery in FROM and a WHERE join
// instead. Subunits with zero edges are skipped — they keep their
// DEFAULT idf=0, which is what we want anyway since a 0-edge subunit
// would have undefined IDF.
func RecomputeIDF(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		UPDATE evo.skill_subunits u
		   SET idf = GREATEST(0,
		             LN((SELECT COUNT(*)::float FROM evo.skills) /
		                NULLIF(d.df, 0)))::real
		  FROM (
		    SELECT subunit_id, COUNT(DISTINCT skill_id) AS df
		      FROM evo.skill_subunit_edges
		     GROUP BY subunit_id
		  ) d
		 WHERE d.subunit_id = u.id`)
	if err != nil {
		return fmt.Errorf("recompute idf: %w", err)
	}
	return nil
}

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SearchHit is one row in R2R's `chunk_search_results` array. Field
// names match R2R's wire format exactly so existing callers (the
// dashboard's skills_search_graph parser, the SPA's research view)
// don't need to change.
type SearchHit struct {
	ID            uuid.UUID       `json:"id"`
	DocumentID    uuid.UUID       `json:"document_id"`
	OwnerID       *uuid.UUID      `json:"owner_id"`
	CollectionIDs []uuid.UUID     `json:"collection_ids"`
	Text          string          `json:"text"`
	Metadata      json.RawMessage `json:"metadata"`
	Score         float64         `json:"score"`
}

// SearchInput names the knobs r2g supports. ``QueryText`` drives BM25
// over the chunks.fts tsvector column; ``QueryVec`` drives the pgvector
// kNN. HybridSearch needs both populated; the legacy SemanticSearch +
// FullTextSearch entry points use whichever they need. The
// CollectionIDs filter matches what every existing caller passes today.
type SearchInput struct {
	QueryVec      []float32
	QueryText     string
	Limit         int
	CollectionIDs []uuid.UUID
	DocumentID    *uuid.UUID
}

// SemanticSearch runs an ANN kNN over deepresearch.chunks.vec using
// pgvector's `<=>` (cosine distance) operator and returns hits in
// distance-ascending order. Score is computed as `1 - distance` so a
// higher score means closer match, matching R2R's convention.
//
// Filters:
//
//   - CollectionIDs: chunks whose collection_ids array overlaps the
//     given set. Implemented with the `&&` array operator so the GIN
//     index on collection_ids actually gets used.
//   - DocumentID: chunks belonging to that doc. Cheap on the existing
//     btree index.
//
// Limit caps the result set. Pgvector's IVFFlat/HNSW index decides
// recall; with default settings on the deepresearch chunks table we
// get >0.95 recall at LIMIT 50.
func SemanticSearch(ctx context.Context, pool *pgxpool.Pool, in SearchInput) ([]SearchHit, error) {
	if len(in.QueryVec) == 0 {
		return nil, fmt.Errorf("search: empty query vector")
	}
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 10
	}

	where := []string{"1=1"}
	args := []any{vectorParam(in.QueryVec)}
	if len(in.CollectionIDs) > 0 {
		args = append(args, in.CollectionIDs)
		where = append(where, fmt.Sprintf("collection_ids && $%d::uuid[]", len(args)))
	}
	if in.DocumentID != nil {
		args = append(args, *in.DocumentID)
		where = append(where, fmt.Sprintf("document_id = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	q := fmt.Sprintf(`
		SELECT
		  id, document_id, owner_id, collection_ids, text, metadata,
		  1 - (vec <=> $1::vector) AS score
		FROM deepresearch.chunks
		WHERE %s
		ORDER BY vec <=> $1::vector
		LIMIT %d
	`, clause, limit)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query semantic search: %w", err)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		h := SearchHit{}
		if err := rows.Scan(
			&h.ID, &h.DocumentID, &h.OwnerID, &h.CollectionIDs,
			&h.Text, &h.Metadata, &h.Score,
		); err != nil {
			return nil, fmt.Errorf("scan search row: %w", err)
		}
		if h.CollectionIDs == nil {
			h.CollectionIDs = []uuid.UUID{}
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate search rows: %w", err)
	}
	return out, nil
}

// FullTextSearch runs a BM25-ish query against deepresearch.chunks.fts
// (a stored tsvector generated from ``text`` with the english config).
// Score is Postgres's ``ts_rank_cd`` cover-density rank — it weights
// term proximity inside the chunk, which works better for paragraph-
// length chunks than the simpler ``ts_rank``. The plainto_tsquery
// helper coerces free-form user input into a safe tsquery (no operator
// parsing surprises like ``C++`` blowing up the planner).
//
// Why a separate function instead of a flag on SemanticSearch: ranking
// math is incommensurable across the two backends, so the fusion has
// to happen on top of two ranked lists, not inside one SQL statement.
// Pgvector + ts_rank_cd in a single SELECT would force one ordering or
// the other; RRF needs both rankings preserved.
func FullTextSearch(ctx context.Context, pool *pgxpool.Pool, in SearchInput) ([]SearchHit, error) {
	if strings.TrimSpace(in.QueryText) == "" {
		return nil, fmt.Errorf("search: empty query text")
	}
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 10
	}

	where := []string{"fts @@ plainto_tsquery('english', $1)"}
	args := []any{in.QueryText}
	if len(in.CollectionIDs) > 0 {
		args = append(args, in.CollectionIDs)
		where = append(where, fmt.Sprintf("collection_ids && $%d::uuid[]", len(args)))
	}
	if in.DocumentID != nil {
		args = append(args, *in.DocumentID)
		where = append(where, fmt.Sprintf("document_id = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	q := fmt.Sprintf(`
		SELECT
		  id, document_id, owner_id, collection_ids, text, metadata,
		  ts_rank_cd(fts, plainto_tsquery('english', $1)) AS score
		FROM deepresearch.chunks
		WHERE %s
		ORDER BY ts_rank_cd(fts, plainto_tsquery('english', $1)) DESC
		LIMIT %d
	`, clause, limit)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query fts search: %w", err)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		h := SearchHit{}
		if err := rows.Scan(
			&h.ID, &h.DocumentID, &h.OwnerID, &h.CollectionIDs,
			&h.Text, &h.Metadata, &h.Score,
		); err != nil {
			return nil, fmt.Errorf("scan fts row: %w", err)
		}
		if h.CollectionIDs == nil {
			h.CollectionIDs = []uuid.UUID{}
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fts rows: %w", err)
	}
	return out, nil
}

// HybridSearch fuses pgvector kNN + BM25 (ts_rank_cd) with Reciprocal
// Rank Fusion. RRF is the standard cheap fuser (Cormack et al. 2009);
// score(d) = Σ 1/(k + rank_i(d)) across retrievers, with k=60 the
// universally-cited default. RRF doesn't need score-calibration across
// retrievers, which is what makes it survive the vector-vs-tf-idf
// scale mismatch that ruins naive weighted averaging.
//
// Each backend pulls ``2*Limit`` candidates so the fusion has a real
// chance to reorder; the final result is trimmed back to ``Limit``.
// Both backends run in parallel goroutines because they hit different
// indexes and are independent.
//
// Degraded modes:
//   - If only QueryVec is set, callers shouldn't be here; we still
//     defer to SemanticSearch as a safety net.
//   - If a backend errors, the other one's results are returned
//     unchanged. The error from the failed half is logged via the
//     returned ``warning`` map (one entry per failed source).
//   - If both error, we return the joined error.
func HybridSearch(ctx context.Context, pool *pgxpool.Pool, in SearchInput) ([]SearchHit, map[string]string, error) {
	if len(in.QueryVec) == 0 {
		return nil, nil, fmt.Errorf("hybrid: empty query vector")
	}
	if strings.TrimSpace(in.QueryText) == "" {
		// No text to BM25-rank; fall back to pure semantic so callers
		// aren't worse off with hybrid enabled.
		hits, err := SemanticSearch(ctx, pool, in)
		return hits, nil, err
	}

	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 10
	}
	// Pull a deeper candidate set per backend than the final limit so
	// the fusion has slack to reorder. ``poolSize`` of 2x is the lower
	// bound; we floor at 20 so tiny limits (1-2) still get a useful pool.
	poolSize := limit * 2
	if poolSize < 20 {
		poolSize = 20
	}
	deepIn := in
	deepIn.Limit = poolSize

	var (
		semHits, ftsHits []SearchHit
		semErr, ftsErr   error
		wg               sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); semHits, semErr = SemanticSearch(ctx, pool, deepIn) }()
	go func() { defer wg.Done(); ftsHits, ftsErr = FullTextSearch(ctx, pool, deepIn) }()
	wg.Wait()

	warnings := map[string]string{}
	if semErr != nil {
		warnings["semantic"] = semErr.Error()
	}
	if ftsErr != nil {
		warnings["fts"] = ftsErr.Error()
	}
	if semErr != nil && ftsErr != nil {
		return nil, warnings, errors.Join(semErr, ftsErr)
	}

	// RRF: rank starts at 1 for the top hit in each list.
	const k = 60.0
	scores := make(map[uuid.UUID]float64, len(semHits)+len(ftsHits))
	byID := make(map[uuid.UUID]SearchHit, len(semHits)+len(ftsHits))
	for rank, h := range semHits {
		scores[h.ID] += 1.0 / (k + float64(rank+1))
		if _, exists := byID[h.ID]; !exists {
			byID[h.ID] = h
		}
	}
	for rank, h := range ftsHits {
		scores[h.ID] += 1.0 / (k + float64(rank+1))
		if _, exists := byID[h.ID]; !exists {
			byID[h.ID] = h
		}
	}

	type scored struct {
		hit   SearchHit
		score float64
	}
	sorted := make([]scored, 0, len(scores))
	for id, s := range scores {
		h := byID[id]
		h.Score = s // overwrite the per-backend score with the RRF score
		sorted = append(sorted, scored{h, s})
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].score != sorted[j].score {
			return sorted[i].score > sorted[j].score
		}
		// Tie-break on id so reloads are deterministic.
		return sorted[i].hit.ID.String() < sorted[j].hit.ID.String()
	})
	if len(sorted) > limit {
		sorted = sorted[:limit]
	}
	out := make([]SearchHit, len(sorted))
	for i, s := range sorted {
		out[i] = s.hit
	}
	if len(warnings) == 0 {
		warnings = nil
	}
	return out, warnings, nil
}

// vectorParam renders a []float32 as pgvector's text input format.
// pgx will then pass it through as text and pgvector will parse it
// server-side. Slightly slower than the binary protocol but avoids a
// pgvector codec registration dance.
func vectorParam(v []float32) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("%g", x)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// DeleteDocument removes a document and everything that hangs off it:
//
//	platform.document_figures  (no FK; manual)
//	deepresearch.chunks        (no FK; manual; cascades to
//	                           platform.chunks_octen_embed)
//	deepresearch.documents     (FK cascades documents_entities +
//	                           documents_relationships)
//
// Runs in a single transaction so a partial failure doesn't leave
// orphans. Returns the count of affected document rows (0 means the
// document didn't exist; handlers map that to HTTP 404).
func DeleteDocument(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM platform.document_figures WHERE document_id = $1`, id); err != nil {
		return 0, fmt.Errorf("delete figures: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM deepresearch.chunks WHERE document_id = $1`, id); err != nil {
		return 0, fmt.Errorf("delete chunks: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM deepresearch.documents WHERE id = $1`, id)
	if err != nil {
		return 0, fmt.Errorf("delete document: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

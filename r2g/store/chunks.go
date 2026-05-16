package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Chunk mirrors the on-the-wire R2R chunk object. JSON tags match
// R2R's /v3/chunks response shape (see r2g/testdata/golden/).
//
// `vec` postgres column → `vector` JSON field. R2R always emits the
// embedding even on list responses; we do the same so byte-equality
// holds.
type Chunk struct {
	ID            uuid.UUID       `json:"id"`
	DocumentID    uuid.UUID       `json:"document_id"`
	OwnerID       *uuid.UUID      `json:"owner_id"`
	CollectionIDs []uuid.UUID     `json:"collection_ids"`
	Text          string          `json:"text"`
	Metadata      json.RawMessage `json:"metadata"`
	Vector        []float32       `json:"vector"`
}

// ListChunksInput names the filter knobs. document_id is the typical
// caller filter; the others are pass-through for completeness.
type ListChunksInput struct {
	Limit      int
	Offset     int
	DocumentID *uuid.UUID
}

// ListChunks returns (rows, total). Total counts all matching rows
// ignoring limit/offset, matching R2R's total_entries field.
//
// Ordering: by `metadata->>'chunk_order'` ascending when document_id
// is set (preserves the reader's expected sequence); otherwise by id.
func ListChunks(ctx context.Context, pool *pgxpool.Pool, in ListChunksInput) ([]Chunk, int, error) {
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if in.Offset < 0 {
		in.Offset = 0
	}

	where := []string{"1=1"}
	args := []any{}
	orderBy := "id"
	if in.DocumentID != nil {
		args = append(args, *in.DocumentID)
		where = append(where, fmt.Sprintf("document_id = $%d", len(args)))
		// chunk_order is integer; cast for the sort.
		orderBy = "COALESCE((metadata->>'chunk_order')::int, 0)"
	}
	clause := strings.Join(where, " AND ")

	q := fmt.Sprintf(`
		SELECT
		  id, document_id, owner_id, collection_ids, text, metadata, vec::text,
		  COUNT(*) OVER()
		FROM deepresearch.chunks
		WHERE %s
		ORDER BY %s
		LIMIT %d OFFSET %d
	`, clause, orderBy, limit, in.Offset)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query chunks: %w", err)
	}
	defer rows.Close()

	var out []Chunk
	var total int
	for rows.Next() {
		c := Chunk{}
		var vecText *string
		if err := rows.Scan(
			&c.ID, &c.DocumentID, &c.OwnerID, &c.CollectionIDs,
			&c.Text, &c.Metadata, &vecText, &total,
		); err != nil {
			return nil, 0, fmt.Errorf("scan chunk row: %w", err)
		}
		c.Vector = parsePgvector(vecText)
		if c.CollectionIDs == nil {
			c.CollectionIDs = []uuid.UUID{}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate chunk rows: %w", err)
	}
	return out, total, nil
}

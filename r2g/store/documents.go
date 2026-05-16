package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a SELECT by id finds zero rows. Handler
// code maps this to HTTP 404.
var ErrNotFound = errors.New("not found")

// Document mirrors the on-the-wire R2R document object, plus the
// fields r2g reads off deepresearch.documents. JSON tags match R2R's
// /v3/documents response shape exactly (see r2g/testdata/golden/).
//
// Two field-name remaps vs. the postgres column names:
//   - `type` (postgres) → `document_type` (JSON)
//   - `vec`  (postgres, on chunks) → `vector`  (JSON, on chunks)
//
// `Chunks` is always emitted as JSON null on single-doc GETs (matches
// R2R; chunks are a separate endpoint). `SummaryEmbedding` is the raw
// 1024-d vector returned by pgvector.
type Document struct {
	ID                     uuid.UUID       `json:"id"`
	CollectionIDs          []uuid.UUID     `json:"collection_ids"`
	OwnerID                *uuid.UUID      `json:"owner_id"`
	DocumentType           *string         `json:"document_type"`
	Metadata               json.RawMessage `json:"metadata"`
	Title                  *string         `json:"title"`
	Version                *string         `json:"version"`
	SizeInBytes            *int            `json:"size_in_bytes"`
	IngestionStatus        *string         `json:"ingestion_status"`
	ExtractionStatus       *string         `json:"extraction_status"`
	CreatedAt              r2rTime         `json:"created_at"`
	UpdatedAt              r2rTime         `json:"updated_at"`
	IngestionAttemptNumber *int            `json:"ingestion_attempt_number"`
	Summary                *string         `json:"summary"`
	SummaryEmbedding       []float32       `json:"summary_embedding"`
	TotalTokens            *int            `json:"total_tokens"`
	Chunks                 *[]any          `json:"chunks"`
}

// GetDocument returns the row for id, or ErrNotFound if absent.
// summary_embedding is decoded via pgvector's text format ("[a,b,c,...]").
func GetDocument(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (*Document, error) {
	const q = `
		SELECT
		  id, collection_ids, owner_id, type, metadata, title, version,
		  size_in_bytes, ingestion_status, extraction_status,
		  created_at, updated_at, ingestion_attempt_number, total_tokens,
		  summary, summary_embedding::text
		FROM deepresearch.documents
		WHERE id = $1
	`
	row := pool.QueryRow(ctx, q, id)
	d := &Document{}
	var embText *string
	var createdAt, updatedAt time.Time
	if err := row.Scan(
		&d.ID, &d.CollectionIDs, &d.OwnerID, &d.DocumentType, &d.Metadata,
		&d.Title, &d.Version, &d.SizeInBytes, &d.IngestionStatus, &d.ExtractionStatus,
		&createdAt, &updatedAt, &d.IngestionAttemptNumber, &d.TotalTokens,
		&d.Summary, &embText,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("select document: %w", err)
	}
	d.CreatedAt = r2rTime{t: createdAt}
	d.UpdatedAt = r2rTime{t: updatedAt}
	d.SummaryEmbedding = parsePgvector(embText)
	if d.CollectionIDs == nil {
		d.CollectionIDs = []uuid.UUID{}
	}
	return d, nil
}

// ListDocumentsInput names the filter knobs. Empty values mean "no
// filter". Limit/Offset are paged at the SQL layer.
type ListDocumentsInput struct {
	Limit         int
	Offset        int
	CollectionID  *uuid.UUID
	IngestionStat *string
}

// ListDocuments returns (rows, total). total is the COUNT(*) ignoring
// limit/offset — R2R's response surface includes it as total_entries.
func ListDocuments(ctx context.Context, pool *pgxpool.Pool, in ListDocumentsInput) ([]Document, int, error) {
	limit := in.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if in.Offset < 0 {
		in.Offset = 0
	}

	where := []string{"1=1"}
	args := []any{}
	if in.CollectionID != nil {
		args = append(args, *in.CollectionID)
		where = append(where, fmt.Sprintf("$%d = ANY(collection_ids)", len(args)))
	}
	if in.IngestionStat != nil {
		args = append(args, *in.IngestionStat)
		where = append(where, fmt.Sprintf("ingestion_status = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	// One round-trip with COUNT(*) OVER() so total_entries comes back
	// without a second query. Cheaper than COUNT(*) on a 2954-row table
	// at the cost of being run per page.
	q := fmt.Sprintf(`
		SELECT
		  id, collection_ids, owner_id, type, metadata, title, version,
		  size_in_bytes, ingestion_status, extraction_status,
		  created_at, updated_at, ingestion_attempt_number, total_tokens,
		  summary, summary_embedding::text,
		  COUNT(*) OVER()
		FROM deepresearch.documents
		WHERE %s
		ORDER BY created_at DESC
		LIMIT %d OFFSET %d
	`, clause, limit, in.Offset)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query documents: %w", err)
	}
	defer rows.Close()

	var out []Document
	var total int
	for rows.Next() {
		d := Document{}
		var embText *string
		var createdAt, updatedAt time.Time
		if err := rows.Scan(
			&d.ID, &d.CollectionIDs, &d.OwnerID, &d.DocumentType, &d.Metadata,
			&d.Title, &d.Version, &d.SizeInBytes, &d.IngestionStatus, &d.ExtractionStatus,
			&createdAt, &updatedAt, &d.IngestionAttemptNumber, &d.TotalTokens,
			&d.Summary, &embText, &total,
		); err != nil {
			return nil, 0, fmt.Errorf("scan document row: %w", err)
		}
		d.CreatedAt = r2rTime{t: createdAt}
		d.UpdatedAt = r2rTime{t: updatedAt}
		d.SummaryEmbedding = parsePgvector(embText)
		if d.CollectionIDs == nil {
			d.CollectionIDs = []uuid.UUID{}
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate document rows: %w", err)
	}
	return out, total, nil
}

// r2rTime serializes timestamps in R2R's Python-default format:
//
//	"2026-05-14T18:43:35.391034Z" — microsecond precision, trailing Z.
//
// Go's default time.Time.MarshalJSON uses RFC3339Nano which emits
// nanosecond precision and a "+00:00" offset, which doesn't match
// R2R's bytes. We override to keep byte-equality during the
// strangler-fig phase.
type r2rTime struct{ t time.Time }

func (r r2rTime) MarshalJSON() ([]byte, error) {
	if r.t.IsZero() {
		return []byte(`null`), nil
	}
	// .000000Z is microsecond precision (6 fractional digits) like Python.
	s := r.t.UTC().Format("2006-01-02T15:04:05.000000Z")
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	out = append(out, s...)
	out = append(out, '"')
	return out, nil
}

// parsePgvector decodes pgvector's text format "[v1,v2,...,vN]" into
// []float32. Returns nil on empty / NULL. Matches R2R's behaviour of
// emitting summary_embedding as a JSON array of numbers.
func parsePgvector(s *string) []float32 {
	if s == nil || *s == "" {
		return nil
	}
	t := strings.TrimSpace(*s)
	t = strings.TrimPrefix(t, "[")
	t = strings.TrimSuffix(t, "]")
	if t == "" {
		return nil
	}
	parts := strings.Split(t, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		var v float32
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%f", &v); err != nil {
			return nil
		}
		out = append(out, v)
	}
	return out
}

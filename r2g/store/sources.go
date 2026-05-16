package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoSuchSource is returned when GetSource doesn't find a row.
var ErrNoSuchSource = errors.New("store: no such source")

// AllowedSourceKinds matches the platform.sources.kind CHECK constraint
// (alembic 0001). Surfaced to the HTTP layer so the /kinds endpoint and
// the kind-filter validation can use one canonical list.
var AllowedSourceKinds = []string{
	"journal",
	"preprint",
	"repo",
	"forum",
	"wiki",
	"web",
	"book",
}

// Source mirrors a row of platform.sources. metadata is decoded into a
// map; the platform.sources.metadata column is jsonb-with-default-'{}'
// so this is never nil after Get.
type Source struct {
	ID            int64
	Name          string
	Kind          string
	Domain        string
	DefaultWeight *float64
	PeerReviewed  bool
	H5Index       *int
	Metadata      map[string]any
}

// SourceFilter narrows ListSources. Pointer-valued fields mean "filter
// not applied"; non-nil with empty values is treated as the literal
// (e.g. PeerReviewed pointing at false filters to non-peer-reviewed
// rows).
type SourceFilter struct {
	Kind         *string
	PeerReviewed *bool
	// QSubstring matches case-insensitively against both name and
	// domain ("arxiv" hits the name; "ieee" hits the domain).
	QSubstring *string
	Limit      int
	Offset     int
}

// ListSources returns matching rows + the total UNFILTERED count of the
// table so the UI can render "5 of 32" without a second round-trip,
// preserving the legacy /platform/sources response contract.
func ListSources(ctx context.Context, pool *pgxpool.Pool, f SourceFilter) ([]Source, int, error) {
	where := []string{}
	args := []any{}
	idx := 1
	if f.Kind != nil {
		where = append(where, fmt.Sprintf("kind = $%d", idx))
		args = append(args, *f.Kind)
		idx++
	}
	if f.PeerReviewed != nil {
		where = append(where, fmt.Sprintf("peer_reviewed = $%d", idx))
		args = append(args, *f.PeerReviewed)
		idx++
	}
	if f.QSubstring != nil && *f.QSubstring != "" {
		where = append(where, fmt.Sprintf("(name ILIKE $%d OR domain ILIKE $%d)", idx, idx))
		args = append(args, "%"+*f.QSubstring+"%")
		idx++
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	listSQL := fmt.Sprintf(`
		SELECT id, name,
		       COALESCE(kind, ''), COALESCE(domain, ''),
		       default_weight, COALESCE(peer_reviewed, false),
		       h5_index, COALESCE(metadata, '{}'::jsonb)
		FROM platform.sources
		%s
		ORDER BY default_weight DESC NULLS LAST,
		         peer_reviewed DESC,
		         name ASC
		LIMIT $%d OFFSET $%d
	`, whereSQL, idx, idx+1)
	args = append(args, limit, offset)

	rows, err := pool.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	out := []Source{}
	for rows.Next() {
		var (
			s       Source
			metaRaw []byte
		)
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Kind, &s.Domain,
			&s.DefaultWeight, &s.PeerReviewed, &s.H5Index, &metaRaw,
		); err != nil {
			return nil, 0, fmt.Errorf("scan source: %w", err)
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &s.Metadata)
		}
		if s.Metadata == nil {
			s.Metadata = map[string]any{}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iter sources: %w", err)
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform.sources`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count sources: %w", err)
	}
	return out, total, nil
}

// GetSource returns one row by id. ErrNoSuchSource if missing.
func GetSource(ctx context.Context, pool *pgxpool.Pool, id int64) (*Source, error) {
	var (
		s       Source
		metaRaw []byte
	)
	err := pool.QueryRow(ctx, `
		SELECT id, name,
		       COALESCE(kind, ''), COALESCE(domain, ''),
		       default_weight, COALESCE(peer_reviewed, false),
		       h5_index, COALESCE(metadata, '{}'::jsonb)
		FROM platform.sources
		WHERE id = $1
	`, id).Scan(
		&s.ID, &s.Name, &s.Kind, &s.Domain,
		&s.DefaultWeight, &s.PeerReviewed, &s.H5Index, &metaRaw,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSuchSource
		}
		return nil, fmt.Errorf("get source %d: %w", id, err)
	}
	if len(metaRaw) > 0 {
		_ = json.Unmarshal(metaRaw, &s.Metadata)
	}
	if s.Metadata == nil {
		s.Metadata = map[string]any{}
	}
	return &s, nil
}

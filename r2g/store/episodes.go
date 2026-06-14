package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Episode is a raw, append-only memory capture row. The consolidator
// worker (cut 2b) promotes batches of these into deepresearch.documents
// and stamps ConsolidatedInto. See r2g/store/migrations/0002_episodes.
type Episode struct {
	ID               uuid.UUID      `json:"id"`
	Kind             string         `json:"kind"`
	Content          string         `json:"content"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	OccurredAt       time.Time      `json:"occurred_at"`
	ForesightUntil   *time.Time     `json:"foresight_until,omitempty"`
	ConsolidatedInto *uuid.UUID     `json:"consolidated_into,omitempty"`
}

// InsertEpisodeInput is the write shape. Embedding is optional: when nil
// the row stores NULL and the consolidator embeds it later.
type InsertEpisodeInput struct {
	Kind           string
	Content        string
	Metadata       map[string]any
	Embedding      []float32
	OccurredAt     *time.Time
	ForesightUntil *time.Time
}

// InsertEpisode writes one episode and returns its generated id.
func InsertEpisode(ctx context.Context, pool *pgxpool.Pool, in InsertEpisodeInput) (uuid.UUID, error) {
	meta := in.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal metadata: %w", err)
	}

	var embed *string
	if len(in.Embedding) > 0 {
		s := vectorParam(in.Embedding)
		embed = &s
	}

	id := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO deepresearch.episodes
		  (id, kind, content, embedding, metadata, occurred_at, foresight_until)
		VALUES ($1, $2, $3, $4::vector, $5::jsonb, COALESCE($6, now()), $7)
	`,
		id, in.Kind, in.Content, embed, string(metaBytes),
		in.OccurredAt, in.ForesightUntil,
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert episode: %w", err)
	}
	return id, nil
}

// ListEpisodesFilter scopes a read. Zero values mean "no filter".
type ListEpisodesFilter struct {
	Kind            string
	Since           *time.Time
	ForesightActive bool
	Limit           int
}

// ListEpisodes returns episodes newest-first, excluding tombstoned rows.
func ListEpisodes(ctx context.Context, pool *pgxpool.Pool, f ListEpisodesFilter) ([]Episode, error) {
	var conds []string
	var args []any
	conds = append(conds, "tombstoned_at IS NULL")

	if f.Kind != "" {
		args = append(args, f.Kind)
		conds = append(conds, fmt.Sprintf("kind = $%d", len(args)))
	}
	if f.Since != nil {
		args = append(args, *f.Since)
		conds = append(conds, fmt.Sprintf("occurred_at >= $%d", len(args)))
	}
	if f.ForesightActive {
		conds = append(conds, "foresight_until IS NOT NULL AND foresight_until >= now()")
	}

	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args = append(args, limit)

	q := fmt.Sprintf(`
		SELECT id, kind, content, metadata, occurred_at, foresight_until, consolidated_into
		  FROM deepresearch.episodes
		 WHERE %s
		 ORDER BY occurred_at DESC
		 LIMIT $%d
	`, strings.Join(conds, " AND "), len(args))

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query episodes: %w", err)
	}
	defer rows.Close()

	var out []Episode
	for rows.Next() {
		var e Episode
		var metaBytes []byte
		if err := rows.Scan(&e.ID, &e.Kind, &e.Content, &metaBytes,
			&e.OccurredAt, &e.ForesightUntil, &e.ConsolidatedInto); err != nil {
			return nil, fmt.Errorf("scan episode: %w", err)
		}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &e.Metadata)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate episodes: %w", err)
	}
	return out, nil
}

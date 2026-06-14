package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/r2g/internal/extract"
)

// KGSource identifies the row an extraction came from, for mention
// + evidence provenance. Kind is "episode" or "document".
type KGSource struct {
	Kind string
	ID   uuid.UUID
}

const (
	SourceEpisode  = "episode"
	SourceDocument = "document"
)

// canonicalName matches the Go-side normalizeName in extract.go and the
// lower(name) used by the cut-1 backfill: ASCII-lowercased surface form.
func canonicalName(s string) string {
	return strings.ToLower(s)
}

// PersistEntityGraph writes an extraction Result into the canonical
// entity graph: find-or-create entities by canonical name, attach a
// mention row back to the source, and upsert bi-temporal relations with
// naive dedup (match active edge on subject/predicate_norm/object, append
// evidence; otherwise insert). No invalidation — that lands with the
// Graphiti port (cut 5). Returns (#entities touched, #relations touched).
//
// Idempotent per source for mentions (delete-then-insert). Re-running over
// the same source appends to relation evidence without duplicating it.
func PersistEntityGraph(
	ctx context.Context,
	pool *pgxpool.Pool,
	src KGSource,
	res *extract.Result,
	recordedAt time.Time,
) (int, int, error) {
	if res == nil || (len(res.Entities) == 0 && len(res.Relationships) == 0) {
		return 0, 0, nil
	}
	if src.Kind != SourceEpisode && src.Kind != SourceDocument {
		return 0, 0, fmt.Errorf("invalid entity source kind %q", src.Kind)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Resolve every entity name to a canonical id (find-or-create).
	ids := map[string]uuid.UUID{}
	for _, e := range res.Entities {
		cn := canonicalName(e.Name)
		if cn == "" {
			continue
		}
		id, err := upsertEntity(ctx, tx, e, cn)
		if err != nil {
			return 0, 0, err
		}
		ids[cn] = id
	}
	// Relationship endpoints may name entities the LLM didn't list
	// separately; create bare canonical rows for them too.
	for _, r := range res.Relationships {
		for _, n := range []string{r.Subject, r.Object} {
			cn := canonicalName(n)
			if cn == "" {
				continue
			}
			if _, ok := ids[cn]; !ok {
				id, err := upsertEntity(ctx, tx, extract.Entity{Name: n}, cn)
				if err != nil {
					return 0, 0, err
				}
				ids[cn] = id
			}
		}
	}

	// Mentions: delete-then-insert for this source so re-extraction is
	// idempotent.
	mentionTable, mentionFK := "deepresearch.document_mentions", "document_id"
	if src.Kind == SourceEpisode {
		mentionTable, mentionFK = "deepresearch.episode_mentions", "episode_id"
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, mentionTable, mentionFK), src.ID,
	); err != nil {
		return 0, 0, fmt.Errorf("clear mentions: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %s (entity_id, %s) VALUES ($1, $2)`, mentionTable, mentionFK),
			id, src.ID,
		); err != nil {
			return 0, 0, fmt.Errorf("insert mention: %w", err)
		}
	}

	// Relations: naive dedup against the active edge, append evidence.
	relCount := 0
	for _, r := range res.Relationships {
		sid, sok := ids[canonicalName(r.Subject)]
		oid, ook := ids[canonicalName(r.Object)]
		if !sok || !ook || strings.TrimSpace(r.Predicate) == "" {
			continue
		}
		if err := upsertRelation(ctx, tx, src, sid, r.Predicate, oid, r.Weight, recordedAt); err != nil {
			return 0, 0, err
		}
		relCount++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit entity graph: %w", err)
	}
	return len(ids), relCount, nil
}

// upsertEntity finds or creates a canonical entity, returning its id.
// mention_count is set on insert only; cut 5 recomputes it properly.
func upsertEntity(ctx context.Context, tx pgx.Tx, e extract.Entity, cn string) (uuid.UUID, error) {
	var typ *string
	if t := strings.TrimSpace(e.Category); t != "" {
		typ = &t
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO deepresearch.entities (name, canonical_name, type, mention_count)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (canonical_name) WHERE merged_into_id IS NULL
		DO UPDATE SET last_seen = now()
		RETURNING id
	`, e.Name, cn, typ).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("upsert entity %q: %w", e.Name, err)
	}
	return id, nil
}

// upsertRelation appends evidence to the matching active edge or inserts a
// new one. evidenceCol depends on whether the source is an episode or doc.
func upsertRelation(
	ctx context.Context, tx pgx.Tx, src KGSource,
	sid uuid.UUID, predicate string, oid uuid.UUID, weight float64, recordedAt time.Time,
) error {
	evidenceCol := "evidence_doc_ids"
	if src.Kind == SourceEpisode {
		evidenceCol = "evidence_episode_ids"
	}

	tag, err := tx.Exec(ctx, fmt.Sprintf(`
		UPDATE deepresearch.entity_relations
		   SET %[1]s = CASE WHEN $4::uuid = ANY(COALESCE(%[1]s, '{}'))
		                    THEN %[1]s
		                    ELSE array_append(COALESCE(%[1]s, '{}'), $4) END,
		       confidence = GREATEST(COALESCE(confidence, 0), $5)
		 WHERE subject_id = $1 AND predicate_norm = lower(btrim($2)) AND object_id = $3
		   AND invalidated_at IS NULL
	`, evidenceCol), sid, predicate, oid, src.ID, weight)
	if err != nil {
		return fmt.Errorf("update relation evidence: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO deepresearch.entity_relations
		  (subject_id, predicate, object_id, recorded_at, %s, confidence, provenance)
		VALUES ($1, $2, $3, $4, ARRAY[$5]::uuid[], $6, 'extracted')
	`, evidenceCol), sid, predicate, oid, recordedAt, src.ID, weight); err != nil {
		return fmt.Errorf("insert relation: %w", err)
	}
	return nil
}

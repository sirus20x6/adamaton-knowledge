package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queryStopWords is the inline stop-word set dropped during seed-entity
// tokenization. Intentionally small — we only need to keep common English
// connectives from ever hitting the ILIKE branch (e.g. "the" matches half
// the corpus). Mention-rank biases real terms above any noise that slips
// through anyway.
//
// Entries must match the tokenizer's post-split output: alphanumeric only,
// length >= 3, lowercase. Contractions like "it's" are unreachable here —
// the apostrophe is a splitter, so the contraction never appears as a
// single token. Filter the surviving fragment instead (e.g. "won" for
// "won't") if needed.
var queryStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "that": {}, "this": {},
	"from": {}, "has": {}, "have": {}, "had": {}, "are": {}, "was": {},
	"were": {}, "will": {}, "can": {}, "could": {}, "should": {}, "would": {},
	"what": {}, "when": {}, "where": {}, "who": {}, "why": {}, "how": {},
	"your": {}, "you": {}, "our": {}, "but": {}, "not": {}, "all": {},
	"any": {}, "its": {}, "into": {}, "over": {}, "also": {},
	"them": {}, "they": {}, "their": {}, "than": {}, "then": {}, "like": {},
	"more": {}, "most": {}, "less": {}, "very": {}, "much": {},
}

// ErrCorpusNotFound bubbles up from graphrag_* queries so the route layer
// can map to 404. The non-graphrag queries don't enforce corpus existence;
// when a corpus_id is supplied they scope via platform.corpus_documents,
// and when it's empty they fall back to global (pre-scope) behaviour.
var ErrCorpusNotFound = errors.New("store: corpus not found")

// corpusScope returns an EXISTS predicate that ties `<alias>.parent_id` to
// platform.corpus_documents for the given corpus, plus the args to append.
// alias is the SQL alias of the documents_entities or documents_relationships
// table inside the calling statement. paramIdx is the next $N placeholder
// the caller intends to bind. Returns ("", nil) when corpusID is empty so
// callers can splice an unscoped query.
func corpusScope(alias, corpusID string, paramIdx int) (string, []any) {
	if corpusID == "" {
		return "", nil
	}
	pred := fmt.Sprintf(`AND EXISTS (
			SELECT 1 FROM platform.corpus_documents cd
			WHERE cd.document_id = %s.parent_id
			  AND cd.corpus_id = $%d
		)`, alias, paramIdx)
	return pred, []any{corpusID}
}

// graphIdentRE guards the schema interpolation. R2R_PROJECT_NAME flows
// into the FROM clause unquoted; refuse anything that isn't a plain
// identifier so we don't accept "; DROP TABLE …" by accident.
var graphIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// r2rSchema picks the schema for the documents_* tables. R2R lets ops
// rename it via R2R_PROJECT_NAME; default matches the Pi deploy.
func r2rSchema() (string, error) {
	s := os.Getenv("R2R_PROJECT_NAME")
	if s == "" {
		s = "deepresearch"
	}
	if !graphIdentRE.MatchString(s) {
		return "", fmt.Errorf("unsafe R2R_PROJECT_NAME: %q", s)
	}
	return s, nil
}

// CategoryCount is a (category, mention_count) tuple from
// documents_entities. Ordered by count DESC at the SQL layer.
type CategoryCount struct {
	Category string
	Count    int
}

// EntityCount is (name, mention_count) inside one category.
type EntityCount struct {
	Name  string
	Count int
}

// EntityRelationship is an aggregated edge from documents_relationships.
// Weight is the mean across the (subject, object, predicate) group.
type EntityRelationship struct {
	Subject   string
	Object    string
	Weight    float64
	EdgeCount int
	Predicate string // empty when no row had a non-empty predicate
}

// CategoryBridge counts the rels that cross from cat_a's modal entities
// to cat_b's. Used for level-2 edge weights.
type CategoryBridge struct {
	SubCat string
	ObjCat string
	Count  int
}

// EntityMeta is the per-name aggregate displayed in the node-detail panel.
type EntityMeta struct {
	Name        string
	Category    string
	Mentions    int
	Description string // longest description seen across docs
}

// EntityNeighbour is one side of an incident edge. Direction is "out"
// when name was the subject, "in" when name was the object.
type EntityNeighbour struct {
	Other     string
	Weight    float64
	Direction string
	Predicate string
}

// EntitySource describes one document that mentioned the entity. Mirrors
// the Python /node payload's `sources[]` element shape.
type EntitySource struct {
	DocumentID uuid.UUID
	Title      string
	Category   string
	Mentions   int
	ChunkIDs   []uuid.UUID
	Metadata   map[string]any
	CreatedAt  *time.Time
}

// EntityMember is one row of the membership view returned to the route
// for taxonomic categories (SPECIES / FACTION / …).
type EntityMember struct {
	Name      string
	Predicate string
	Weight    float64
	Category  string
}

// EntitySearchHit is one row of /platform/graph/search.
type EntitySearchHit struct {
	Name     string
	Category string
	Mentions int
}

// QueryCategories returns the top-N categories ordered by row count.
// When corpusID is non-empty, only rows whose parent document belongs to
// that corpus contribute. Empty corpusID keeps the legacy global behaviour.
func QueryCategories(ctx context.Context, pool *pgxpool.Pool, limit int, corpusID string) ([]CategoryCount, error) {
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 is limit; corpus param (if present) comes after.
	scope, scopeArgs := corpusScope("e", corpusID, 2)
	args := []any{limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT e.category, COUNT(*) AS sz
		FROM %s.documents_entities e
		WHERE e.category IS NOT NULL AND e.category <> ''
		  %s
		GROUP BY e.category
		ORDER BY sz DESC
		LIMIT $1
	`, schema, scope)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query categories: %w", err)
	}
	defer rows.Close()
	out := []CategoryCount{}
	for rows.Next() {
		var c CategoryCount
		if err := rows.Scan(&c.Category, &c.Count); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// QueryCategoryEntities returns the top-N entities inside one category
// by mention count. Ordered by mentions DESC, name ASC for stability.
func QueryCategoryEntities(ctx context.Context, pool *pgxpool.Pool, category string, limit int, corpusID string) ([]EntityCount, error) {
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 category, $2 limit, $3 (if any) corpus.
	scope, scopeArgs := corpusScope("e", corpusID, 3)
	args := []any{category, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT e.name, COUNT(*) AS mentions
		FROM %s.documents_entities e
		WHERE e.category = $1 AND e.name IS NOT NULL AND e.name <> ''
		  %s
		GROUP BY e.name
		ORDER BY mentions DESC, name ASC
		LIMIT $2
	`, schema, scope)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query category entities: %w", err)
	}
	defer rows.Close()
	out := []EntityCount{}
	for rows.Next() {
		var e EntityCount
		if err := rows.Scan(&e.Name, &e.Count); err != nil {
			return nil, fmt.Errorf("scan entity count: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueryRelationshipsWithin returns edges where both endpoints are in
// names. Predicate is uppercased + group-keyed; empty predicates become
// "" in Go (NULL in SQL).
func QueryRelationshipsWithin(ctx context.Context, pool *pgxpool.Pool, names []string, limit int, corpusID string) ([]EntityRelationship, error) {
	if len(names) == 0 {
		return nil, nil
	}
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 names, $2 limit, $3 (if any) corpus.
	scope, scopeArgs := corpusScope("r", corpusID, 3)
	args := []any{names, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT r.subject, r.object,
		       AVG(r.weight) AS w, COUNT(*) AS n,
		       UPPER(NULLIF(r.predicate, '')) AS predicate
		FROM %s.documents_relationships r
		WHERE r.subject = ANY($1) AND r.object = ANY($1)
		  AND r.subject <> r.object
		  %s
		GROUP BY r.subject, r.object, UPPER(NULLIF(r.predicate, ''))
		ORDER BY n DESC
		LIMIT $2
	`, schema, scope)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query rels within: %w", err)
	}
	defer rows.Close()
	out := []EntityRelationship{}
	for rows.Next() {
		var (
			r         EntityRelationship
			weight    *float64
			predicate *string
		)
		if err := rows.Scan(&r.Subject, &r.Object, &weight, &r.EdgeCount, &predicate); err != nil {
			return nil, fmt.Errorf("scan rel: %w", err)
		}
		if weight != nil {
			r.Weight = *weight
		}
		if predicate != nil {
			r.Predicate = *predicate
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryCategoryBridges returns the per-pair count of cross-category
// relationships, where each entity's "category" is its modal value in
// documents_entities. Mirrors the Python CTE. When corpusID is set, BOTH
// the name->category resolution (CTE) and the relationship edges must be
// corpus-bound, otherwise a name that only appears in another corpus could
// re-route the bridge count through the wrong taxonomy.
func QueryCategoryBridges(ctx context.Context, pool *pgxpool.Pool, categories []string, limit int, corpusID string) ([]CategoryBridge, error) {
	if len(categories) == 0 {
		return nil, nil
	}
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 categories, $2 limit, $3 (if any) corpus — same param shared by
	// both EXISTS clauses since pgx accepts the same placeholder twice.
	scopeEntity, scopeArgs := corpusScope("e", corpusID, 3)
	scopeRel, _ := corpusScope("r", corpusID, 3)
	args := []any{categories, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		WITH name_cat AS (
		    SELECT e.name, MODE() WITHIN GROUP (ORDER BY e.category) AS cat
		    FROM %s.documents_entities e
		    WHERE e.category = ANY($1)
		      %s
		    GROUP BY e.name
		)
		SELECT a.cat AS sub_cat, b.cat AS obj_cat, COUNT(*) AS n
		FROM %s.documents_relationships r
		JOIN name_cat a ON a.name = r.subject
		JOIN name_cat b ON b.name = r.object
		WHERE a.cat <> b.cat
		  %s
		GROUP BY a.cat, b.cat
		ORDER BY n DESC
		LIMIT $2
	`, schema, scopeEntity, schema, scopeRel)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query category bridges: %w", err)
	}
	defer rows.Close()
	out := []CategoryBridge{}
	for rows.Next() {
		var b CategoryBridge
		if err := rows.Scan(&b.SubCat, &b.ObjCat, &b.Count); err != nil {
			return nil, fmt.Errorf("scan bridge: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// QueryEntityMeta returns one entity's modal category + mention count +
// longest description. Nil when the name has no rows. When corpusID is
// set both the outer aggregate AND the inner description-pick must be
// corpus-bound — otherwise a longer description from a foreign corpus
// would leak into the node panel.
func QueryEntityMeta(ctx context.Context, pool *pgxpool.Pool, name string, corpusID string) (*EntityMeta, error) {
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 name, $2 (if any) corpus — reused across the inner and outer scopes.
	scopeInner, scopeArgs := corpusScope("d2", corpusID, 2)
	scopeOuter, _ := corpusScope("e", corpusID, 2)
	args := []any{name}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT e.name,
		       MODE() WITHIN GROUP (ORDER BY e.category) AS category,
		       COUNT(*) AS mentions,
		       (SELECT description
		          FROM %s.documents_entities d2
		         WHERE d2.name = $1
		           AND d2.description IS NOT NULL
		           %s
		         ORDER BY length(d2.description) DESC
		         LIMIT 1) AS description
		FROM %s.documents_entities e
		WHERE e.name = $1
		  %s
		GROUP BY e.name
	`, schema, scopeInner, schema, scopeOuter)
	var (
		m           EntityMeta
		category    *string
		description *string
	)
	if err := pool.QueryRow(ctx, q, args...).Scan(&m.Name, &category, &m.Mentions, &description); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query entity meta: %w", err)
	}
	if category != nil {
		m.Category = *category
	}
	if description != nil {
		m.Description = *description
	}
	return &m, nil
}

// QueryEntityNeighbours returns directed incident edges keyed by name.
// A (Booth, Lincoln, kills) pair surfaces twice: once from each end.
// Both legs of the UNION ALL get the corpus scope so an entity that
// happens to share a name across corpora can't leak neighbours from
// the wrong one.
func QueryEntityNeighbours(ctx context.Context, pool *pgxpool.Pool, name string, limit int, corpusID string) ([]EntityNeighbour, error) {
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 name, $2 limit, $3 (if any) corpus — reused on both legs.
	scopeOut, scopeArgs := corpusScope("r_obj", corpusID, 3)
	scopeIn, _ := corpusScope("r_sub", corpusID, 3)
	args := []any{name, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT other, direction, AVG(w) AS weight,
		       MODE() WITHIN GROUP (ORDER BY predicate) AS predicate
		FROM (
		    SELECT r_obj.object AS other,
		           'out' AS direction,
		           r_obj.weight AS w,
		           r_obj.predicate AS predicate
		      FROM %s.documents_relationships r_obj
		     WHERE r_obj.subject = $1 AND r_obj.object <> $1
		       %s
		    UNION ALL
		    SELECT r_sub.subject AS other,
		           'in' AS direction,
		           r_sub.weight AS w,
		           r_sub.predicate AS predicate
		      FROM %s.documents_relationships r_sub
		     WHERE r_sub.object = $1 AND r_sub.subject <> $1
		       %s
		) e
		GROUP BY other, direction
		ORDER BY weight DESC, other ASC, direction ASC
		LIMIT $2
	`, schema, scopeOut, schema, scopeIn)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query neighbours: %w", err)
	}
	defer rows.Close()
	out := []EntityNeighbour{}
	for rows.Next() {
		var (
			n         EntityNeighbour
			weight    *float64
			predicate *string
		)
		if err := rows.Scan(&n.Other, &n.Direction, &weight, &predicate); err != nil {
			return nil, fmt.Errorf("scan neighbour: %w", err)
		}
		if weight != nil {
			n.Weight = *weight
		}
		if predicate != nil {
			n.Predicate = *predicate
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// QueryEntityCategory returns the entity's modal category, or "" when
// the name has no rows.
func QueryEntityCategory(ctx context.Context, pool *pgxpool.Pool, name string, corpusID string) (string, error) {
	schema, err := r2rSchema()
	if err != nil {
		return "", err
	}
	// $1 name, $2 (if any) corpus.
	scope, scopeArgs := corpusScope("e", corpusID, 2)
	args := []any{name}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT e.category, COUNT(*) AS n
		FROM %s.documents_entities e
		WHERE e.name = $1 AND e.category IS NOT NULL AND e.category <> ''
		  %s
		GROUP BY e.category
		ORDER BY n DESC
		LIMIT 1
	`, schema, scope)
	var category string
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&category, &n); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("query entity category: %w", err)
	}
	return category, nil
}

// QueryEntityMembers returns the entities pointing AT name via the given
// predicates. Used to render SPECIES/FACTION/RELIGION/... as parent+members.
// Both the relationships outer query AND the inner category-pick get the
// corpus scope so members from a foreign corpus can't appear nor relabel.
func QueryEntityMembers(ctx context.Context, pool *pgxpool.Pool, name string, predicates []string, limit int, corpusID string) ([]EntityMember, error) {
	if len(predicates) == 0 {
		return nil, nil
	}
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	upper := make([]string, len(predicates))
	for i, p := range predicates {
		upper[i] = upperASCII(p)
	}
	// $1 name, $2 upper-predicates, $3 limit, $4 (if any) corpus.
	scopeInner, scopeArgs := corpusScope("ie", corpusID, 4)
	scopeOuter, _ := corpusScope("r", corpusID, 4)
	args := []any{name, upper, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT
		    r.subject AS member_name,
		    MODE() WITHIN GROUP (ORDER BY r.predicate) AS predicate,
		    AVG(r.weight) AS weight,
		    (
		        SELECT category FROM %s.documents_entities ie
		        WHERE ie.name = r.subject
		          %s
		        GROUP BY category
		        ORDER BY COUNT(*) DESC
		        LIMIT 1
		    ) AS member_category
		FROM %s.documents_relationships r
		WHERE r.object = $1
		  AND r.subject <> $1
		  AND UPPER(r.predicate) = ANY($2)
		  %s
		GROUP BY r.subject
		ORDER BY weight DESC NULLS LAST, member_name ASC
		LIMIT $3
	`, schema, scopeInner, schema, scopeOuter)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query entity members: %w", err)
	}
	defer rows.Close()
	out := []EntityMember{}
	for rows.Next() {
		var (
			m         EntityMember
			predicate *string
			weight    *float64
			category  *string
		)
		if err := rows.Scan(&m.Name, &predicate, &weight, &category); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		if predicate != nil {
			m.Predicate = *predicate
		}
		if weight != nil {
			m.Weight = *weight
		}
		if category != nil {
			m.Category = *category
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueryEntitySources returns the per-document mention rows joined with
// the documents table for title/metadata/created_at. The corpus scope
// goes inside the CTE — once mention rows are restricted to in-corpus
// documents, the outer documents JOIN naturally only sees those docs.
func QueryEntitySources(ctx context.Context, pool *pgxpool.Pool, name string, limit int, corpusID string) ([]EntitySource, error) {
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 name, $2 limit, $3 (if any) corpus.
	scope, scopeArgs := corpusScope("e", corpusID, 3)
	args := []any{name, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		WITH source_rows AS (
		    SELECT e.parent_id AS document_id,
		           MODE() WITHIN GROUP (ORDER BY e.category) AS category,
		           COUNT(*) AS mentions,
		           array_agg(DISTINCT chunk_id) FILTER (
		               WHERE chunk_id IS NOT NULL
		           ) AS chunk_ids
		    FROM %s.documents_entities e
		    LEFT JOIN LATERAL unnest(e.chunk_ids) AS c(chunk_id) ON true
		    WHERE e.name = $1
		      AND e.parent_id IS NOT NULL
		      %s
		    GROUP BY e.parent_id
		)
		SELECT s.document_id,
		       s.category,
		       s.mentions,
		       s.chunk_ids,
		       d.title,
		       d.metadata,
		       d.created_at
		FROM source_rows s
		LEFT JOIN %s.documents d ON d.id = s.document_id
		ORDER BY s.mentions DESC, d.created_at DESC NULLS LAST
		LIMIT $2
	`, schema, scope, schema)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query entity sources: %w", err)
	}
	defer rows.Close()
	out := []EntitySource{}
	for rows.Next() {
		var (
			s         EntitySource
			category  *string
			chunkIDs  []uuid.UUID
			title     *string
			metaRaw   []byte
			createdAt *time.Time
		)
		if err := rows.Scan(&s.DocumentID, &category, &s.Mentions, &chunkIDs,
			&title, &metaRaw, &createdAt); err != nil {
			return nil, fmt.Errorf("scan entity source: %w", err)
		}
		if category != nil {
			s.Category = *category
		}
		if title != nil {
			s.Title = *title
		}
		s.ChunkIDs = chunkIDs
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &s.Metadata)
		}
		if createdAt != nil {
			t := *createdAt
			s.CreatedAt = &t
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SearchEntities does a substring ILIKE over documents_entities.name and
// returns the per-name aggregate.
func SearchEntities(ctx context.Context, pool *pgxpool.Pool, needle string, limit int, corpusID string) ([]EntitySearchHit, error) {
	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 needle, $2 limit, $3 (if any) corpus.
	scope, scopeArgs := corpusScope("e", corpusID, 3)
	args := []any{"%" + needle + "%", limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		SELECT e.name,
		       MODE() WITHIN GROUP (ORDER BY e.category) AS category,
		       COUNT(*) AS mentions
		FROM %s.documents_entities e
		WHERE e.name ILIKE $1
		  %s
		GROUP BY e.name
		ORDER BY mentions DESC, name ASC
		LIMIT $2
	`, schema, scope)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("search entities: %w", err)
	}
	defer rows.Close()
	out := []EntitySearchHit{}
	for rows.Next() {
		var (
			h        EntitySearchHit
			category *string
		)
		if err := rows.Scan(&h.Name, &category, &h.Mentions); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		if category != nil {
			h.Category = *category
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────
// GraphRAG-backed reads. The platform.graphrag_* tables hold the lo
// worker's community-detection output and are currently feature-flagged
// off, so QueryGraphRAG* must tolerate missing-table errors (caller
// degrades to the empty payload).
// ─────────────────────────────────────────────────────────────────────────

// GraphRAGCounts mirrors the summary endpoint payload.
type GraphRAGCounts struct {
	Entities      int
	Relationships int
	Communities   int
	Reports       int
	TextUnits     int
	Levels        []int
}

// GraphRAGCommunityRow is one row of graphrag/{corpus_id}/communities.
type GraphRAGCommunityRow struct {
	RowID         uuid.UUID
	CommunityID   *int
	Level         *int
	Title         string
	Size          int
	Parent        *int
	ReportSummary *string
	ReportRank    *float64
}

// GraphRAGEntityRow is one row of graphrag/{corpus_id}/entities.
type GraphRAGEntityRow struct {
	ID          uuid.UUID
	Title       string
	Type        *string
	Description *string
	Degree      *int
	Frequency   *int
}

// CorpusExists is the existence check the graphrag/* handlers run before
// touching the graphrag tables. Returns true/false; only DB-level errors
// propagate.
func CorpusExists(ctx context.Context, pool *pgxpool.Pool, corpus uuid.UUID) (bool, error) {
	var x int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM platform.corpora WHERE id = $1`, corpus).Scan(&x)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// QueryGraphRAGSummary returns counts + distinct community levels for a
// corpus. Empty zeroed struct + nil error when the tables exist but the
// corpus has no rows.
func QueryGraphRAGSummary(ctx context.Context, pool *pgxpool.Pool, corpus uuid.UUID) (*GraphRAGCounts, error) {
	row := pool.QueryRow(ctx, `
		SELECT
		    (SELECT COUNT(*) FROM platform.graphrag_entities
		      WHERE corpus_id = $1) AS entities,
		    (SELECT COUNT(*) FROM platform.graphrag_relationships
		      WHERE corpus_id = $1) AS relationships,
		    (SELECT COUNT(*) FROM platform.graphrag_communities
		      WHERE corpus_id = $1) AS communities,
		    (SELECT COUNT(*) FROM platform.graphrag_community_reports
		      WHERE corpus_id = $1) AS reports,
		    (SELECT COUNT(*) FROM platform.graphrag_text_units
		      WHERE corpus_id = $1) AS text_units
	`, corpus)
	var c GraphRAGCounts
	if err := row.Scan(&c.Entities, &c.Relationships, &c.Communities, &c.Reports, &c.TextUnits); err != nil {
		return nil, fmt.Errorf("graphrag counts: %w", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT (payload->>'level')::int AS level
		FROM platform.graphrag_communities
		WHERE corpus_id = $1
		  AND payload ? 'level'
		  AND payload->>'level' ~ '^-?[0-9]+$'
		ORDER BY level
	`, corpus)
	if err != nil {
		return nil, fmt.Errorf("graphrag levels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lvl int
		if err := rows.Scan(&lvl); err != nil {
			return nil, fmt.Errorf("scan level: %w", err)
		}
		c.Levels = append(c.Levels, lvl)
	}
	return &c, rows.Err()
}

// QueryGraphRAGCommunities returns one row per community at target level,
// joined with its report. If target level has zero rows, falls back to
// the deepest available level (matches the Python "still render
// something" hedge).
func QueryGraphRAGCommunities(ctx context.Context, pool *pgxpool.Pool, corpus uuid.UUID, level int, limit int) ([]GraphRAGCommunityRow, error) {
	target := level
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM platform.graphrag_communities
		WHERE corpus_id = $1 AND payload->>'level' = $2
	`, corpus, fmt.Sprintf("%d", target)).Scan(&count); err != nil {
		return nil, fmt.Errorf("graphrag count at level: %w", err)
	}
	if count == 0 {
		var deepest *int
		if err := pool.QueryRow(ctx, `
			SELECT MAX((payload->>'level')::int)
			FROM platform.graphrag_communities
			WHERE corpus_id = $1
			  AND payload ? 'level'
			  AND payload->>'level' ~ '^-?[0-9]+$'
		`, corpus).Scan(&deepest); err != nil {
			return nil, fmt.Errorf("graphrag deepest level: %w", err)
		}
		if deepest == nil {
			return nil, nil
		}
		target = *deepest
	}
	rows, err := pool.Query(ctx, `
		SELECT
		    c.id AS row_id,
		    (c.payload->>'community')::int AS community_id,
		    (c.payload->>'level')::int AS level,
		    COALESCE(c.payload->>'title', '') AS title,
		    COALESCE(NULLIF(c.payload->>'size', '')::int, 0) AS size,
		    CASE
		        WHEN c.payload->>'parent' ~ '^-?[0-9]+$'
		        THEN (c.payload->>'parent')::int
		        ELSE NULL
		    END AS parent,
		    r.payload->>'summary' AS report_summary,
		    CASE
		        WHEN r.payload->>'rank' ~ '^-?[0-9]+(\.[0-9]+)?$'
		        THEN (r.payload->>'rank')::float
		        ELSE NULL
		    END AS report_rank
		FROM platform.graphrag_communities c
		LEFT JOIN platform.graphrag_community_reports r
		  ON r.corpus_id = c.corpus_id
		 AND r.payload->>'community' = c.payload->>'community'
		WHERE c.corpus_id = $1
		  AND c.payload->>'level' = $2
		ORDER BY report_rank DESC NULLS LAST, size DESC
		LIMIT $3
	`, corpus, fmt.Sprintf("%d", target), limit)
	if err != nil {
		return nil, fmt.Errorf("graphrag communities: %w", err)
	}
	defer rows.Close()
	out := []GraphRAGCommunityRow{}
	for rows.Next() {
		var r GraphRAGCommunityRow
		if err := rows.Scan(&r.RowID, &r.CommunityID, &r.Level, &r.Title,
			&r.Size, &r.Parent, &r.ReportSummary, &r.ReportRank); err != nil {
			return nil, fmt.Errorf("scan graphrag community: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryGraphRAGCommunityDetail returns the raw payload + the matched
// report payload (if any) keyed by row UUID. Returns nil, nil when the
// row is missing — handler maps that to 404.
func QueryGraphRAGCommunityDetail(ctx context.Context, pool *pgxpool.Pool, rowID uuid.UUID) (map[string]any, map[string]any, error) {
	var (
		corpusID uuid.UUID
		payload  []byte
	)
	err := pool.QueryRow(ctx, `
		SELECT corpus_id, payload
		FROM platform.graphrag_communities
		WHERE id = $1
	`, rowID).Scan(&corpusID, &payload)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("graphrag community detail: %w", err)
	}
	var community map[string]any
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &community)
	}
	if community == nil {
		community = map[string]any{}
	}

	var report map[string]any
	if key, ok := community["community"]; ok && key != nil {
		var rpayload []byte
		if err := pool.QueryRow(ctx, `
			SELECT payload
			FROM platform.graphrag_community_reports
			WHERE corpus_id = $1
			  AND payload->>'community' = $2
			LIMIT 1
		`, corpusID, fmt.Sprintf("%v", key)).Scan(&rpayload); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, fmt.Errorf("graphrag report fetch: %w", err)
			}
		}
		if len(rpayload) > 0 {
			_ = json.Unmarshal(rpayload, &report)
		}
	}
	return community, report, nil
}

// QueryGraphRAGEntities returns the entity list for a corpus, optionally
// filtered by a title/description substring.
func QueryGraphRAGEntities(ctx context.Context, pool *pgxpool.Pool, corpus uuid.UUID, needle string, limit int) ([]GraphRAGEntityRow, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if needle != "" {
		rows, err = pool.Query(ctx, `
			SELECT
			    id,
			    payload->>'title' AS title,
			    payload->>'type' AS type,
			    payload->>'description' AS description,
			    CASE
			        WHEN payload->>'degree' ~ '^-?[0-9]+$'
			        THEN (payload->>'degree')::int
			        ELSE NULL
			    END AS degree,
			    CASE
			        WHEN payload->>'frequency' ~ '^-?[0-9]+$'
			        THEN (payload->>'frequency')::int
			        ELSE NULL
			    END AS frequency
			FROM platform.graphrag_entities
			WHERE corpus_id = $1
			  AND (payload->>'title' ILIKE $2 OR payload->>'description' ILIKE $2)
			ORDER BY degree DESC NULLS LAST
			LIMIT $3
		`, corpus, "%"+needle+"%", limit)
	} else {
		rows, err = pool.Query(ctx, `
			SELECT
			    id,
			    payload->>'title' AS title,
			    payload->>'type' AS type,
			    payload->>'description' AS description,
			    CASE
			        WHEN payload->>'degree' ~ '^-?[0-9]+$'
			        THEN (payload->>'degree')::int
			        ELSE NULL
			    END AS degree,
			    CASE
			        WHEN payload->>'frequency' ~ '^-?[0-9]+$'
			        THEN (payload->>'frequency')::int
			        ELSE NULL
			    END AS frequency
			FROM platform.graphrag_entities
			WHERE corpus_id = $1
			ORDER BY degree DESC NULLS LAST
			LIMIT $2
		`, corpus, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("graphrag entities: %w", err)
	}
	defer rows.Close()
	out := []GraphRAGEntityRow{}
	for rows.Next() {
		var (
			r     GraphRAGEntityRow
			title *string
		)
		if err := rows.Scan(&r.ID, &title, &r.Type, &r.Description, &r.Degree, &r.Frequency); err != nil {
			return nil, fmt.Errorf("scan graphrag entity: %w", err)
		}
		if title != nil {
			r.Title = *title
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// upperASCII uppercases a..z. Predicates extracted by the LLM are ASCII
// identifiers; we don't need locale-aware unicode upcasing here.
func upperASCII(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		b[i] = c
	}
	return string(b)
}

// SeedEntity is a query-derived candidate entity for TGS-RAG Graph-to-Text
// Global Voting. ID and Name come from documents_entities; Mentions is the
// total row count for that name across the (corpus-scoped) entity table.
type SeedEntity struct {
	ID       uuid.UUID
	Name     string
	Mentions int
}

// tokenizeQuery splits q into deduped lowercase tokens of length >= 3,
// dropping queryStopWords. Order is preserved by first-occurrence. The
// splitter treats any non-letter / non-digit rune as a separator, so
// "GPT-4o" → ["gpt", "4o"] before the length filter drops "4o".
//
// Returns nil for an empty input, an input with no letters/digits, or
// one whose every token is either too short or a stop-word. Extracted
// from ExtractQuerySeedEntities so it can be unit-tested without a DB.
func tokenizeQuery(q string) []string {
	raw := strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]struct{}{}
	tokens := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.ToLower(t)
		if len(t) < 3 {
			continue
		}
		if _, skip := queryStopWords[t]; skip {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		tokens = append(tokens, t)
	}
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// ExtractQuerySeedEntities tokenizes the query and looks up matching
// entity names in deepresearch.documents_entities, scoped to a corpus
// when corpusID != "". Used by TGS-RAG Graph-to-Text Global Voting at
// query time — ILIKE-based so we don't burn an LLM call on every search.
//
// Tokens shorter than 3 chars and an inline stop-word set are dropped.
// For each remaining token: prefer exact LOWER(name)=token; fall back
// to name ILIKE '%'||token||'%'. Returns up to `limit` distinct seeds,
// ordered by mentions DESC then name ASC.
//
// Returns an empty slice (nil error) when no seeds are found — callers
// treat that as "skip the graph vote backend in HybridSearch".
func ExtractQuerySeedEntities(ctx context.Context, pool *pgxpool.Pool, query string, corpusID string, limit int) ([]SeedEntity, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	tokens := tokenizeQuery(query)
	if len(tokens) == 0 {
		return nil, nil
	}

	schema, err := r2rSchema()
	if err != nil {
		return nil, err
	}
	// $1 tokens, $2 limit, $3 (if any) corpus.
	scope, scopeArgs := corpusScope("e", corpusID, 3)
	args := []any{tokens, limit}
	args = append(args, scopeArgs...)
	q := fmt.Sprintf(`
		WITH q AS (
		    SELECT DISTINCT lower(t) AS tok FROM unnest($1::text[]) AS t
		),
		hits AS (
		    SELECT DISTINCT ON (lower(e.name))
		           e.id,
		           e.name,
		           (SELECT COUNT(*)
		              FROM %s.documents_entities e2
		             WHERE lower(e2.name) = lower(e.name)) AS mentions
		    FROM %s.documents_entities e
		    JOIN q ON lower(e.name) = q.tok
		           OR e.name ILIKE '%%' || q.tok || '%%'
		    WHERE TRUE
		      %s
		)
		SELECT id, name, mentions
		FROM hits
		ORDER BY mentions DESC, name ASC
		LIMIT $2
	`, schema, schema, scope)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("extract query seed entities: %w", err)
	}
	defer rows.Close()
	out := []SeedEntity{}
	for rows.Next() {
		var s SeedEntity
		if err := rows.Scan(&s.ID, &s.Name, &s.Mentions); err != nil {
			return nil, fmt.Errorf("extract query seed entities: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("extract query seed entities: %w", err)
	}
	return out, nil
}

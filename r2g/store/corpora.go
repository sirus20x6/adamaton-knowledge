package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoSuchCorpus is returned when a corpus lookup or update misses.
var ErrNoSuchCorpus = errors.New("store: no such corpus")

// AllowedDBCorpusScopes matches the platform.corpora.scope CHECK
// constraint. ALlowedAPICorpusScopes additionally accepts "experiments",
// which the API layer translates to ("mixed", metadata.experiment=true).
var (
	AllowedDBCorpusScopes  = []string{"research", "book", "mixed"}
	AllowedAPICorpusScopes = []string{"research", "book", "mixed", "experiments"}
)

// Corpus mirrors a row of platform.corpora. DocumentCount is populated
// by Get/List via a join; it is not a column on the table.
type Corpus struct {
	ID            uuid.UUID
	Name          string
	Scope         string
	Metadata      map[string]any
	CreatedAt     time.Time
	DocumentCount int64
}

// CorpusDocument is one row of platform.corpus_documents joined with
// the R2R documents table. Pointer fields capture the LEFT JOIN nulls
// for docs that vanished from R2R between attach and list.
type CorpusDocument struct {
	DocumentID        uuid.UUID
	AddedAt           time.Time
	AddedBy           string
	Type              *string
	Title             *string
	Metadata          map[string]any
	SizeInBytes       *int64
	IngestionStatus   *string
	DocumentCreatedAt *time.Time
}

// StageCounts is the per-corpus aggregate the dashboard polls.
type StageCounts struct {
	Stage1 int64 `json:"stage1"`
	Stage2 int64 `json:"stage2"`
	Stage3 int64 `json:"stage3"`
	Stage4 int64 `json:"stage4"`
	Total  int64 `json:"total"`
}

// R2RDocumentsSchema resolves the schema R2R puts its documents table
// in. R2R tenants under <project_name>; defaults to "deepresearch".
func R2RDocumentsSchema() string {
	if v := os.Getenv("R2R_PROJECT_NAME"); v != "" {
		return v
	}
	return "deepresearch"
}

// ListCorpora returns all non-soft-deleted corpora with their counts.
func ListCorpora(ctx context.Context, pool *pgxpool.Pool) ([]Corpus, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.id, c.name, COALESCE(c.scope, ''),
		       COALESCE(c.metadata, '{}'::jsonb), c.created_at,
		       COUNT(cd.document_id) AS document_count
		FROM platform.corpora c
		LEFT JOIN platform.corpus_documents cd ON cd.corpus_id = c.id
		WHERE COALESCE(c.metadata->>'deleted_at', '') = ''
		GROUP BY c.id, c.name, c.scope, c.metadata, c.created_at
		ORDER BY c.created_at DESC NULLS LAST, c.name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list corpora: %w", err)
	}
	defer rows.Close()

	out := []Corpus{}
	for rows.Next() {
		c, err := scanCorpusWithCount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iter corpora: %w", err)
	}
	return out, nil
}

// GetCorpus returns one corpus row plus its document count. The Python
// version does not filter out soft-deleted rows here, so neither do we.
func GetCorpus(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) (*Corpus, error) {
	row := pool.QueryRow(ctx, `
		SELECT c.id, c.name, COALESCE(c.scope, ''),
		       COALESCE(c.metadata, '{}'::jsonb), c.created_at,
		       (SELECT COUNT(*) FROM platform.corpus_documents cd
		          WHERE cd.corpus_id = c.id) AS document_count
		FROM platform.corpora c
		WHERE c.id = $1
	`, id)
	c, err := scanCorpusWithCount(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSuchCorpus
		}
		return nil, err
	}
	return &c, nil
}

// CreateCorpus inserts and returns the new row. scope must be one of
// AllowedDBCorpusScopes (validate at the handler).
func CreateCorpus(ctx context.Context, pool *pgxpool.Pool, name, scope string, metadata map[string]any) (*Corpus, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	id := uuid.New()
	row := pool.QueryRow(ctx, `
		INSERT INTO platform.corpora (id, name, scope, metadata, created_at)
		VALUES ($1, $2, $3, CAST($4 AS jsonb), now())
		RETURNING id, name, COALESCE(scope, ''),
		          COALESCE(metadata, '{}'::jsonb), created_at, 0::bigint
	`, id, strings.TrimSpace(name), scope, string(metaJSON))
	c, err := scanCorpusWithCount(row)
	if err != nil {
		return nil, fmt.Errorf("create corpus: %w", err)
	}
	return &c, nil
}

// CorpusUpdate is the patch payload — nil fields are untouched. The
// Metadata map is merged into the existing JSONB via the || operator;
// callers stamp it with overlay keys (e.g. experiment=true/false) before
// passing it in.
type CorpusUpdate struct {
	Name     *string
	Scope    *string
	Metadata map[string]any
}

// UpdateCorpus applies a patch. Returns ErrNoSuchCorpus for a missing or
// already-soft-deleted row. Empty patch returns the row unchanged.
func UpdateCorpus(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, u CorpusUpdate) (*Corpus, error) {
	sets := []string{}
	args := []any{id}
	idx := 2
	if u.Name != nil {
		sets = append(sets, fmt.Sprintf("name = $%d", idx))
		args = append(args, *u.Name)
		idx++
	}
	if u.Scope != nil {
		sets = append(sets, fmt.Sprintf("scope = $%d", idx))
		args = append(args, *u.Scope)
		idx++
	}
	if len(u.Metadata) > 0 {
		raw, err := json.Marshal(u.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata patch: %w", err)
		}
		sets = append(sets, fmt.Sprintf(
			"metadata = COALESCE(metadata, '{}'::jsonb) || CAST($%d AS jsonb)", idx))
		args = append(args, string(raw))
		idx++
	}
	if len(sets) == 0 {
		return GetCorpus(ctx, pool, id)
	}
	q := fmt.Sprintf(`
		UPDATE platform.corpora
		SET %s
		WHERE id = $1
		  AND COALESCE(metadata->>'deleted_at', '') = ''
		RETURNING id, name, COALESCE(scope, ''),
		          COALESCE(metadata, '{}'::jsonb), created_at,
		          (SELECT COUNT(*) FROM platform.corpus_documents cd
		             WHERE cd.corpus_id = platform.corpora.id) AS document_count
	`, strings.Join(sets, ", "))
	row := pool.QueryRow(ctx, q, args...)
	c, err := scanCorpusWithCount(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSuchCorpus
		}
		return nil, fmt.Errorf("update corpus: %w", err)
	}
	return &c, nil
}

// SoftDeleteCorpus stamps metadata.deleted_at with an ISO timestamp.
// Cascade is intentionally not done here — the corpus_documents rows
// linger so a future un-delete (not yet implemented) could restore them.
func SoftDeleteCorpus(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) error {
	// jsonb_build_object's value-arg is `any`, which pgx can't infer
	// without an explicit cast — matches the comment at corpora.py:460.
	tag, err := pool.Exec(ctx, `
		UPDATE platform.corpora
		SET metadata = COALESCE(metadata, '{}'::jsonb)
		               || jsonb_build_object('deleted_at', CAST($2 AS text))
		WHERE id = $1
	`, id, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("soft-delete corpus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchCorpus
	}
	return nil
}

// AttachDocuments inserts (corpus_id, document_id, added_by, added_at)
// rows for each doc that exists in the R2R documents table. Returns
// (attached, skipped, missing) where missing = docs not in R2R yet.
// ON CONFLICT DO NOTHING makes the call idempotent.
func AttachDocuments(
	ctx context.Context,
	pool *pgxpool.Pool,
	corpusID uuid.UUID,
	docIDs []uuid.UUID,
	addedBy string,
) (attached, skipped, missing int, err error) {
	if err := corpusExists(ctx, pool, corpusID); err != nil {
		return 0, 0, 0, err
	}

	// Filter to docs that actually exist in R2R — silently drop unknown
	// UUIDs so callers don't see an FK violation on in-flight ingests.
	idStrs := make([]string, len(docIDs))
	for i, d := range docIDs {
		idStrs[i] = d.String()
	}
	schema := R2RDocumentsSchema()
	existQ := fmt.Sprintf(`SELECT id FROM %s.documents WHERE id = ANY($1)`, schema)
	rows, err := pool.Query(ctx, existQ, idStrs)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("check r2r docs: %w", err)
	}
	defer rows.Close()
	present := map[uuid.UUID]struct{}{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return 0, 0, 0, fmt.Errorf("scan r2r doc id: %w", err)
		}
		present[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("iter r2r docs: %w", err)
	}

	valid := make([]string, 0, len(docIDs))
	for _, d := range docIDs {
		if _, ok := present[d]; ok {
			valid = append(valid, d.String())
		}
	}
	missing = len(docIDs) - len(valid)
	if len(valid) == 0 {
		return 0, 0, missing, nil
	}

	tag, err := pool.Exec(ctx, `
		INSERT INTO platform.corpus_documents (corpus_id, document_id, added_by, added_at)
		SELECT $1, doc, $2, now()
		FROM unnest(CAST($3 AS uuid[])) AS doc
		ON CONFLICT (corpus_id, document_id) DO NOTHING
	`, corpusID, addedBy, valid)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("insert corpus_documents: %w", err)
	}
	attached = int(tag.RowsAffected())
	skipped = len(valid) - attached
	return attached, skipped, missing, nil
}

// ListCorpusDocuments returns the paged join of corpus_documents with
// the R2R documents table. Returns total alongside the page so callers
// can paginate without a second round-trip.
func ListCorpusDocuments(
	ctx context.Context,
	pool *pgxpool.Pool,
	corpusID uuid.UUID,
	limit, offset int,
) (corpusName string, total int64, items []CorpusDocument, err error) {
	if err := pool.QueryRow(ctx,
		`SELECT name FROM platform.corpora WHERE id = $1`, corpusID,
	).Scan(&corpusName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", 0, nil, ErrNoSuchCorpus
		}
		return "", 0, nil, fmt.Errorf("get corpus name: %w", err)
	}

	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM platform.corpus_documents WHERE corpus_id = $1
	`, corpusID).Scan(&total); err != nil {
		return "", 0, nil, fmt.Errorf("count corpus documents: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT cd.document_id, cd.added_at, COALESCE(cd.added_by, ''),
		       d.type, d.title, COALESCE(d.metadata, '{}'::jsonb),
		       d.size_in_bytes, d.ingestion_status, d.created_at
		FROM platform.corpus_documents cd
		LEFT JOIN %s.documents d ON d.id = cd.document_id
		WHERE cd.corpus_id = $1
		ORDER BY cd.added_at DESC
		LIMIT $2 OFFSET $3
	`, R2RDocumentsSchema())
	rows, err := pool.Query(ctx, q, corpusID, limit, offset)
	if err != nil {
		return "", 0, nil, fmt.Errorf("list corpus documents: %w", err)
	}
	defer rows.Close()

	items = []CorpusDocument{}
	for rows.Next() {
		var (
			cd      CorpusDocument
			metaRaw []byte
		)
		if err := rows.Scan(
			&cd.DocumentID, &cd.AddedAt, &cd.AddedBy,
			&cd.Type, &cd.Title, &metaRaw,
			&cd.SizeInBytes, &cd.IngestionStatus, &cd.DocumentCreatedAt,
		); err != nil {
			return "", 0, nil, fmt.Errorf("scan corpus document: %w", err)
		}
		if len(metaRaw) > 0 {
			_ = json.Unmarshal(metaRaw, &cd.Metadata)
		}
		if cd.Metadata == nil {
			cd.Metadata = map[string]any{}
		}
		items = append(items, cd)
	}
	if err := rows.Err(); err != nil {
		return "", 0, nil, fmt.Errorf("iter corpus documents: %w", err)
	}
	return corpusName, total, items, nil
}

// DetachDocument removes one doc from a corpus. Idempotent: returns
// (false, nil) if the row was not present. The corpus's own existence
// is not checked — matches the Python "DELETE...RETURNING" behaviour
// which returns rowcount=0 in both missing-corpus and missing-link
// cases.
func DetachDocument(ctx context.Context, pool *pgxpool.Pool, corpusID, docID uuid.UUID) (bool, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM platform.corpus_documents
		WHERE corpus_id = $1 AND document_id = $2
	`, corpusID, docID)
	if err != nil {
		return false, fmt.Errorf("detach document: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CountStages returns the per-stage document counters for one corpus.
// Returns ErrNoSuchCorpus if the corpus row is missing — the dashboard
// uses that to decide between rendering an error or a zeroed gauge.
func CountStages(ctx context.Context, pool *pgxpool.Pool, corpusID uuid.UUID) (StageCounts, error) {
	if err := corpusExists(ctx, pool, corpusID); err != nil {
		return StageCounts{}, err
	}
	var sc StageCounts
	err := pool.QueryRow(ctx, `
		SELECT
		    count(*) FILTER (WHERE ds.stage1_at IS NOT NULL) AS stage1,
		    count(*) FILTER (WHERE ds.stage2_at IS NOT NULL) AS stage2,
		    count(*) FILTER (WHERE ds.stage3_at IS NOT NULL) AS stage3,
		    count(*) FILTER (WHERE ds.stage4_at IS NOT NULL) AS stage4,
		    count(cd.document_id)                            AS total
		FROM platform.corpus_documents cd
		LEFT JOIN platform.document_stages ds USING (document_id)
		WHERE cd.corpus_id = $1
	`, corpusID).Scan(&sc.Stage1, &sc.Stage2, &sc.Stage3, &sc.Stage4, &sc.Total)
	if err != nil {
		return StageCounts{}, fmt.Errorf("count stages: %w", err)
	}
	return sc, nil
}

func corpusExists(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID) error {
	var one int
	err := pool.QueryRow(ctx,
		`SELECT 1 FROM platform.corpora WHERE id = $1`, id).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoSuchCorpus
		}
		return fmt.Errorf("check corpus exists: %w", err)
	}
	return nil
}

// scanCorpusWithCount accepts either pgx.Row or pgx.Rows (both
// implement Scan with the same six args). Keeps the SELECT lists in
// sync across List/Get/Create/Update.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanCorpusWithCount(r rowScanner) (Corpus, error) {
	var (
		c       Corpus
		metaRaw []byte
	)
	if err := r.Scan(&c.ID, &c.Name, &c.Scope, &metaRaw, &c.CreatedAt, &c.DocumentCount); err != nil {
		return Corpus{}, err
	}
	if len(metaRaw) > 0 {
		_ = json.Unmarshal(metaRaw, &c.Metadata)
	}
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	return c, nil
}

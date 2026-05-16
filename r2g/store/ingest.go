package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IngestInput names everything the POST /v3/documents handler needs to
// persist a freshly-parsed document plus its chunks in one transaction.
//
// Layout decisions:
//
//   - One Document row + N Chunk rows.
//   - Embeddings live on each chunk's `vec` column (pgvector).
//     summary_embedding on the document is left NULL in this phase --
//     summary generation is part of the (deferred) augment step.
//   - collection_ids stamp both the document and every chunk so the
//     existing search filter (`collection_ids && $1`) keeps working.
type IngestInput struct {
	DocumentID    uuid.UUID
	Title         string
	DocumentType  string // matches deepresearch.documents.type
	Version       string // R2R uses "v0" for the first ingest
	SizeInBytes   int
	Metadata      map[string]any
	CollectionIDs []uuid.UUID
	OwnerID       *uuid.UUID
	TotalTokens   int
	Chunks        []IngestChunk
}

// IngestChunk is one row destined for deepresearch.chunks.
type IngestChunk struct {
	ID       uuid.UUID
	Text     string
	Vec      []float32
	Metadata map[string]any
}

// InsertDocumentWithChunks persists the ingest result atomically.
// Returns a conflict-aware error: if a document with the same id
// already exists with ingestion_status='success', the handler should
// translate this to HTTP 409 (R2R does the same). Other failures roll
// the transaction back cleanly.
//
// The platform.corpus_documents row is NOT touched here -- the
// platform wrapper owns that side of the join. This function only
// writes into deepresearch.* tables, matching R2R's responsibility
// boundary.
func InsertDocumentWithChunks(ctx context.Context, pool *pgxpool.Pool, in IngestInput) error {
	if in.DocumentID == uuid.Nil {
		return errors.New("ingest: document_id required")
	}

	metaBytes, err := json.Marshal(in.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Conflict check: if a doc with this id already exists in
	// ingestion_status='success', surface as ErrAlreadyIngested. We
	// don't want to silently overwrite an established document.
	var existingStatus *string
	err = tx.QueryRow(ctx,
		`SELECT ingestion_status FROM deepresearch.documents WHERE id = $1`,
		in.DocumentID,
	).Scan(&existingStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("conflict check: %w", err)
	}
	if existingStatus != nil && *existingStatus == "success" {
		return ErrAlreadyIngested
	}

	// Upsert document.
	if _, err := tx.Exec(ctx, `
		INSERT INTO deepresearch.documents
		  (id, collection_ids, owner_id, type, metadata, title, version,
		   size_in_bytes, ingestion_status, extraction_status, total_tokens,
		   created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, 'success', 'pending', $9,
		        now(), now())
		ON CONFLICT (id) DO UPDATE SET
		  collection_ids = EXCLUDED.collection_ids,
		  metadata       = EXCLUDED.metadata,
		  title          = EXCLUDED.title,
		  version        = EXCLUDED.version,
		  size_in_bytes  = EXCLUDED.size_in_bytes,
		  ingestion_status = 'success',
		  extraction_status = 'pending',
		  total_tokens   = EXCLUDED.total_tokens,
		  updated_at     = now()
	`,
		in.DocumentID, in.CollectionIDs, in.OwnerID, in.DocumentType, string(metaBytes),
		in.Title, in.Version, in.SizeInBytes, in.TotalTokens,
	); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}

	// Re-ingest: wipe any prior chunks for this doc so we don't end up
	// with a mix of old and new. CASCADE on platform.chunks_octen_embed
	// takes care of the octen mirror.
	if _, err := tx.Exec(ctx,
		`DELETE FROM deepresearch.chunks WHERE document_id = $1`, in.DocumentID); err != nil {
		return fmt.Errorf("clear stale chunks: %w", err)
	}

	// Bulk insert via multi-row INSERT. We initially used COPY through
	// pgx.CopyFrom but COPY's TEXT format treats commas as column
	// separators -- which collides with pgvector's "[a,b,c,...]" text
	// representation and Postgres rejects the row with "vector cannot
	// have more than 16000 dimensions". Multi-row INSERT with the
	// vector cast `$N::vector` parses correctly; throughput is still
	// well under our per-chunk latency budget (median ingest doc =
	// ~50 chunks, ~5 ms each).
	for _, c := range in.Chunks {
		cMeta, err := json.Marshal(c.Metadata)
		if err != nil {
			return fmt.Errorf("marshal chunk metadata: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO deepresearch.chunks
			  (id, document_id, owner_id, collection_ids, vec, text, metadata)
			VALUES ($1, $2, $3, $4, $5::vector, $6, $7::jsonb)
		`,
			c.ID, in.DocumentID, in.OwnerID, in.CollectionIDs,
			vectorParam(c.Vec), c.Text, string(cMeta),
		); err != nil {
			return fmt.Errorf("insert chunk %s: %w", c.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ingest tx: %w", err)
	}
	return nil
}

// ErrAlreadyIngested is returned when an ingest attempt hits an
// existing document with ingestion_status='success'. The route layer
// maps this to HTTP 409.
var ErrAlreadyIngested = errors.New("document already ingested")

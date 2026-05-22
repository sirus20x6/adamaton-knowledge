package activities

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamaton-knowledge/r2g/chunking"
	"github.com/sirus20x6/adamaton-knowledge/r2g/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// embedBatchSize caps chunks per /v1/embeddings request. Matches the
// skills-rae EmbedBatch tuning; the BGE-M3 sidecar handles bigger batches
// but a smaller cap lets concurrent ingests interleave at batch boundaries
// instead of one document monopolising a sidecar slot.
const embedBatchSize = 64

// EmbedDeps carries the dependencies for the worker-side embedding
// activity. Unlike the HTTP-bound IngestActivities, this one does real
// compute on the node: it chunks, calls the embedding sidecar, and writes
// to the document/chunk tables directly. Embedder is nil when neither
// BGE_EMBED_URL nor BGE_SIDECAR_URL is configured, in which case
// ChunkEmbedPersist fails fast with a non-retryable error.
type EmbedDeps struct {
	Embedder *embeddings.Client
	Chunker  *chunking.Chunker
	Pool     *pgxpool.Pool
	Logger   *logrus.Logger
}

// ChunkEmbedPersistInput is the parsed document handed over by
// PrepareArxivDocument. Text is the parsed body (payload-safe — tens to a
// few hundred KB); the multi-MB source tarball never crosses this boundary.
type ChunkEmbedPersistInput struct {
	DocumentID string         `json:"document_id"`
	Title      string         `json:"title"`
	DocType    string         `json:"doctype"`
	Text       string         `json:"text"`
	SizeBytes  int64          `json:"size_bytes"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// ChunkEmbedPersistResult is a small summary — the vectors stay in-process
// and are written to Postgres within this activity, so nothing large is
// serialized back through Temporal.
type ChunkEmbedPersistResult struct {
	Chunks      int    `json:"chunks"`
	TotalTokens int    `json:"total_tokens"`
	Model       string `json:"model"`
}

// ChunkEmbedPersist is the activity that moved the embedding work onto the
// worker node. It chunks the parsed text, embeds each batch through the BGE
// sidecar (heartbeating per batch), and persists the document + chunk rows
// in one transaction. Re-running is safe: store.InsertDocumentWithChunks
// deletes any stale chunks for the document id before inserting, so a
// Temporal retry after a mid-flight crash converges to the same state.
func (d *EmbedDeps) ChunkEmbedPersist(ctx context.Context, in ChunkEmbedPersistInput) (*ChunkEmbedPersistResult, error) {
	log := d.logger()
	docID, err := uuid.Parse(in.DocumentID)
	if err != nil || docID == uuid.Nil {
		return nil, temporal.NewNonRetryableApplicationError(
			"chunkembed: invalid document_id", "BadInput", err)
	}
	if d.Embedder == nil {
		return nil, temporal.NewNonRetryableApplicationError(
			"chunkembed: no embedding sidecar configured (set BGE_EMBED_URL)", "NoEmbedder", nil)
	}
	if d.Chunker == nil {
		return nil, temporal.NewNonRetryableApplicationError(
			"chunkembed: chunker not initialised", "NoChunker", nil)
	}

	chunks, err := d.Chunker.Chunk(in.Text, chunking.DefaultChunkSize, chunking.DefaultChunkOverlap)
	if err != nil {
		return nil, fmt.Errorf("chunk: %w", err)
	}
	if len(chunks) == 0 {
		return nil, temporal.NewNonRetryableApplicationError(
			"chunkembed: text produced zero chunks", "ZeroChunks", nil)
	}
	totalTokens := d.Chunker.CountTokens(in.Text)

	stop := startHeartbeat(ctx, 15*time.Second, in.DocumentID)
	defer stop()

	vecs := make([][]float32, 0, len(chunks))
	for i := 0; i < len(chunks); i += embedBatchSize {
		end := i + embedBatchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch, err := d.Embedder.Embed(ctx, chunks[i:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", i, end, err)
		}
		vecs = append(vecs, batch...)
		activity.RecordHeartbeat(ctx, in.DocumentID, end)
	}

	ingestChunks := make([]store.IngestChunk, len(chunks))
	for i, txt := range chunks {
		ingestChunks[i] = store.IngestChunk{
			ID:       uuid.New(),
			Text:     txt,
			Vec:      vecs[i],
			Metadata: map[string]any{"chunk_order": i},
		}
	}

	// Inherit the default owner if configured, matching what r2g's inline
	// ingest path applied before this work moved onto the worker.
	var ownerID *uuid.UUID
	if v := os.Getenv("R2G_DEFAULT_OWNER_ID"); v != "" {
		if id, perr := uuid.Parse(v); perr == nil {
			ownerID = &id
		}
	}

	if err := store.InsertDocumentWithChunks(ctx, d.Pool, store.IngestInput{
		DocumentID:    docID,
		Title:         in.Title,
		DocumentType:  in.DocType,
		Version:       "v0",
		SizeInBytes:   int(in.SizeBytes),
		Metadata:      in.Metadata,
		CollectionIDs: []uuid.UUID{},
		OwnerID:       ownerID,
		TotalTokens:   totalTokens,
		Chunks:        ingestChunks,
	}); err != nil {
		if errors.Is(err, store.ErrAlreadyIngested) {
			return nil, temporal.NewNonRetryableApplicationError(
				"chunkembed: document already ingested", "AlreadyIngested", err)
		}
		return nil, fmt.Errorf("persist: %w", err)
	}

	if err := store.MarkStageIndexed(ctx, d.Pool, docID); err != nil {
		log.WithError(err).WithField("document_id", docID).
			Warn("stage1 mark failed (ingest still succeeded)")
	}

	log.WithFields(logrus.Fields{
		"document_id":  docID,
		"chunks":       len(chunks),
		"total_tokens": totalTokens,
	}).Info("ChunkEmbedPersist: persisted")

	return &ChunkEmbedPersistResult{
		Chunks:      len(chunks),
		TotalTokens: totalTokens,
		Model:       d.Embedder.Model(),
	}, nil
}

func (d *EmbedDeps) logger() *logrus.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return logrus.New()
}

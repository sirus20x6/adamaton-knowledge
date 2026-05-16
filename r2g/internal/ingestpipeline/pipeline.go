// Package ingestpipeline owns the end-to-end "bytes -> persisted document"
// flow that two HTTP entry points share: the external /v3/documents POST
// (documents POSTed by anything that knows R2R's shape -- the dashboard,
// reindex worker, swarm) and the new /platform/sources/ingest POST (which
// runs the arxiv-fetcher cascade inline and then funnels the result
// through this same pipeline).
//
// Before 2026-05-15 the platform's /platform/sources/ingest endpoint did
// its own HTTP POST to r2g:7272/v3/documents -- one TCP round-trip per
// ingest just to bridge two languages in the same compose network. Now
// the cascade lives in r2g/internal/arxiv and the handler calls Run()
// directly, killing both the HTTP self-call and the Python sources.py.
//
// Pipeline stages:
//
//   1. parsing.Parse(content_type, body)   -> text + doc_type + figures
//   2. chunker.Chunk(text)                 -> []string
//   3. embedder.Embed(chunks)              -> [][]float32
//   4. store.InsertDocumentWithChunks      -> row in platform.documents +
//                                              N rows in platform.chunks
//   5. store.MarkStageIndexed              -> stage1_at = now()
//   6. figures.ExtractAndPersist (eprint)  -> N rows in
//                                              platform.document_figures
//
// Steps 5 and 6 are best-effort -- failure logs a warning but does NOT
// roll back the ingest, since the chunks are already committed. That's a
// deliberate trade: a doc that's indexed-but-not-figure-extracted is
// recoverable (re-run the extract), but rolling back a successful chunk
// commit because the stage1 mark failed would force a costly re-embed.
package ingestpipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/chunking"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/embeddings"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/figures"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/parsing"
	"github.com/sirus20x6/adamomaton-knowledge/r2g/store"
)

// Sentinel errors so HTTP handlers can translate to status codes without
// caring about the underlying package. errors.Is(err, ErrUnsupportedType)
// in routes/ingest.go and routes/sources.go both work; the response body
// can still surface the wrapped message via err.Error().
var (
	ErrUnsupportedType = errors.New("ingestpipeline: unsupported content type")
	ErrParseFailed     = errors.New("ingestpipeline: parse failed")
	ErrZeroChunks      = errors.New("ingestpipeline: body produced zero chunks")
	ErrChunkFailed     = errors.New("ingestpipeline: chunk failed")
	ErrEmbedFailed     = errors.New("ingestpipeline: embed failed")
	ErrPersistFailed   = errors.New("ingestpipeline: persist failed")
	ErrAlreadyIngested = errors.New("ingestpipeline: document already ingested")
)

// DefaultEmbedBatchSize is the max chunks per /v1/embeddings request.
// The BGE-M3 sidecar handles much larger batches, but we cap here so a
// single ingest doesn't tie up a sidecar slot for the whole document --
// other ingests can interleave at the batch boundary.
const DefaultEmbedBatchSize = 32

// Input describes one document to ingest. DocumentID must be non-zero
// (callers generate one if the request didn't pin it). ContentType drives
// the parser dispatch in parsing.Parse -- markdown/plain pass-through,
// PDF -> pdftotext, gzip tarball -> latex2text.
//
// CollectionIDs defaults to []uuid.UUID{} when nil; downstream pydantic
// validation in R2R's read endpoints rejects NULL even though the SQL
// column is nullable, so we coerce.
type Input struct {
	DocumentID    uuid.UUID
	Title         string
	ContentType   string
	Body          []byte
	Metadata      map[string]any
	CollectionIDs []uuid.UUID
	OwnerID       *uuid.UUID
	// DocumentType is an optional override. When empty, the parser's
	// detected DocType ("md", "pdf", "eprint") is used.
	DocumentType string
	// EmbedBatchSize lets callers tune the batch size for big ingests.
	// Zero falls back to DefaultEmbedBatchSize.
	EmbedBatchSize int
}

// Result summarizes the persisted document. Used by HTTP handlers to
// build response bodies and by tests to assert on outcomes.
type Result struct {
	DocumentID  uuid.UUID
	Chunks      int
	TotalTokens int
	SizeBytes   int
	DocType     string
	// Figures is the number of rows written to platform.document_figures.
	// Always 0 for non-eprint inputs.
	Figures int
}

// Run executes the full pipeline against in. Any returned error wraps
// one of the sentinels above (errors.Is to classify). On success the
// result is a snapshot of what was persisted -- the caller decides how
// to render it as an HTTP body.
//
// figures.ExtractAndPersist and store.MarkStageIndexed are best-effort
// post-commit steps; a failure there logs at Warn level and is reflected
// in Result.Figures (set to 0 if extract failed) but does NOT return an
// error -- the document is already safely in the DB.
func Run(
	ctx context.Context,
	pool *pgxpool.Pool,
	embedder *embeddings.Client,
	chunker *chunking.Chunker,
	in Input,
	logger *logrus.Logger,
) (*Result, error) {
	if in.DocumentID == uuid.Nil {
		return nil, fmt.Errorf("%w: document id is required", ErrPersistFailed)
	}
	if len(in.Body) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrParseFailed)
	}

	parsed, err := parsing.Parse(ctx, in.ContentType, in.Title, in.Body)
	if err != nil {
		if errors.Is(err, parsing.ErrUnsupportedType) {
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, err)
		}
		return nil, fmt.Errorf("%w: %s", ErrParseFailed, err)
	}
	text := parsed.Text
	docType := in.DocumentType
	if docType == "" {
		docType = parsed.DocType
	}

	chunks, err := chunker.Chunk(text, chunking.DefaultChunkSize, chunking.DefaultChunkOverlap)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrChunkFailed, err)
	}
	if len(chunks) == 0 {
		return nil, ErrZeroChunks
	}
	totalTokens := chunker.CountTokens(text)

	batch := in.EmbedBatchSize
	if batch <= 0 {
		batch = DefaultEmbedBatchSize
	}
	vecs, err := embedAll(ctx, embedder, chunks, batch)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrEmbedFailed, err)
	}

	ingestChunks := make([]store.IngestChunk, len(chunks))
	for i, txt := range chunks {
		ingestChunks[i] = store.IngestChunk{
			ID:   uuid.New(),
			Text: txt,
			Vec:  vecs[i],
			Metadata: map[string]any{
				"chunk_order": i,
			},
		}
	}
	collectionIDs := in.CollectionIDs
	if collectionIDs == nil {
		collectionIDs = []uuid.UUID{}
	}

	persistInput := store.IngestInput{
		DocumentID:    in.DocumentID,
		Title:         in.Title,
		DocumentType:  docType,
		Version:       "v0",
		SizeInBytes:   len(in.Body),
		Metadata:      in.Metadata,
		CollectionIDs: collectionIDs,
		OwnerID:       in.OwnerID,
		TotalTokens:   totalTokens,
		Chunks:        ingestChunks,
	}
	if err := store.InsertDocumentWithChunks(ctx, pool, persistInput); err != nil {
		if errors.Is(err, store.ErrAlreadyIngested) {
			return nil, ErrAlreadyIngested
		}
		return nil, fmt.Errorf("%w: %s", ErrPersistFailed, err)
	}

	if err := store.MarkStageIndexed(ctx, pool, in.DocumentID); err != nil {
		logger.WithError(err).WithField("document_id", in.DocumentID).
			Warn("stage1 mark failed (ingest still succeeded)")
	}

	figuresPersisted := 0
	if docType == "eprint" && len(parsed.Figures) > 0 {
		metas := make([]figures.FigureMeta, 0, len(parsed.Figures))
		for _, f := range parsed.Figures {
			if len(f.SrcPaths) == 0 {
				continue
			}
			metas = append(metas, figures.FigureMeta{
				Label:    f.Label,
				Caption:  f.Caption,
				SrcPaths: f.SrcPaths,
			})
		}
		if len(metas) > 0 {
			n, err := figures.ExtractAndPersist(
				ctx, pool, in.DocumentID, in.Body, metas,
				logger, store.InsertFigures,
			)
			if err != nil {
				logger.WithError(err).WithField("document_id", in.DocumentID).
					Warn("figure extract failed (ingest still succeeded)")
			} else {
				figuresPersisted = n
				if n > 0 {
					logger.WithFields(logrus.Fields{
						"document_id": in.DocumentID,
						"figures":     n,
					}).Info("figures persisted")
				}
			}
		}
	}

	logger.WithFields(logrus.Fields{
		"document_id":  in.DocumentID,
		"chunks":       len(chunks),
		"total_tokens": totalTokens,
		"size_bytes":   len(in.Body),
		"figures":      figuresPersisted,
	}).Info("ingest complete")

	return &Result{
		DocumentID:  in.DocumentID,
		Chunks:      len(chunks),
		TotalTokens: totalTokens,
		SizeBytes:   len(in.Body),
		DocType:     docType,
		Figures:     figuresPersisted,
	}, nil
}

// embedAll runs the embedder over chunks in batches and returns one
// vector per chunk in input order. Batch-level failures abort the
// whole call; callers retry the entire ingest on transient sidecar
// trouble.
func embedAll(ctx context.Context, embedder *embeddings.Client, chunks []string, batch int) ([][]float32, error) {
	out := make([][]float32, 0, len(chunks))
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		vecs, err := embedder.Embed(ctx, chunks[i:end])
		if err != nil {
			return nil, fmt.Errorf("embed batch [%d:%d]: %w", i, end, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

package activities

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/embeddings"
)

// EmbedDeps wraps the embedder client plus the postgres pool.
type EmbedDeps struct {
	Embedder *embeddings.Client
	Pool     *pgxpool.Pool
}

// EmbedBatchInput is a list of texts to embed.
type EmbedBatchInput struct {
	Texts []string `json:"texts"`
}

// EmbedBatchResult is the parallel list of float32 vectors. Lengths
// match the input — anything else is an error from the sidecar
// already caught inside Embed.
type EmbedBatchResult struct {
	Vectors [][]float32 `json:"vectors"`
	Model   string      `json:"model"`
}

// EmbedBatch runs one batched call to the bge-m3 sidecar. Splits
// large batches into 64-input chunks because the sidecar enforces a
// per-request token budget (and over-large batches push it into
// queueing on the Pi). 64 was the size we landed on for r2g; same
// here for consistency.
const embedBatchSize = 64

func (d *EmbedDeps) EmbedBatch(ctx context.Context, in EmbedBatchInput) (*EmbedBatchResult, error) {
	if len(in.Texts) == 0 {
		return &EmbedBatchResult{Model: d.Embedder.Model()}, nil
	}
	out := make([][]float32, 0, len(in.Texts))
	for i := 0; i < len(in.Texts); i += embedBatchSize {
		end := i + embedBatchSize
		if end > len(in.Texts) {
			end = len(in.Texts)
		}
		chunk, err := d.Embedder.Embed(ctx, in.Texts[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return &EmbedBatchResult{Vectors: out, Model: d.Embedder.Model()}, nil
}

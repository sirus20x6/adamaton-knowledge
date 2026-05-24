// Package chunking splits a document body into overlapping token-bounded
// chunks ready for embedding. r2g tokenizes with ztok (a fast Zig
// tokenizer, github.com/sirus20x6/ztok) loaded with the cl100k_base BPE
// vocab — the same tokenizer family R2R uses, so chunk-token-counts stay
// comparable across the strangler-fig boundary. ztok's cl100k output is
// byte-identical to tiktoken's, so swapping it in does not move chunk
// boundaries (no reindex required); it's just faster.
//
// The cl100k vocab is embedded into the binary via go:embed, so there's
// no network round-trip or writable cache dir at runtime. ztok loaders
// take a file path, so New() writes the embedded bytes to a temp file,
// loads it (the vocab is fully materialized into ztok's in-memory SoA
// table at load), then removes the temp file.
package chunking

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	ztok "github.com/sirus20x6/ztok-go"
)

// cl100kVocab is the cl100k_base BPE vocab in tiktoken's base64
// one-token-per-line format, vendored from the ztok repo
// (demo/vocabs/cl100k_base.tiktoken). ~1.6 MB; loaded once per process.
//
//go:embed assets/cl100k_base.tiktoken
var cl100kVocab []byte

// Defaults match the rough shape of R2R's effective chunk distribution
// on the existing AI Papers corpus (median ~800 tokens, p95 ~1200). The
// 256-token overlap keeps section boundaries from cleaving mid-sentence
// when downstream retrieval mosaics across chunks.
const (
	DefaultChunkSize    = 1024
	DefaultChunkOverlap = 256
)

// Chunker wraps a single ztok pipeline. One instance per process: a ztok
// Pipeline is safe to share across goroutines for Encode/Decode (the
// underlying Zig pipeline is immutable after construction and allocates
// fresh per-call scratch), so the process-wide singleton can be hit
// concurrently by worker activities without a mutex.
type Chunker struct {
	pipe *ztok.Pipeline
}

var (
	defaultChunk  *Chunker
	defaultErr    error
	defaultOnce   sync.Once
	defaultMakeFn = func() (*Chunker, error) { return New("cl100k_base") }
)

// Default returns a process-wide singleton Chunker using cl100k_base.
// The first call pays the vocab-load cost; subsequent calls are free.
func Default() (*Chunker, error) {
	defaultOnce.Do(func() {
		defaultChunk, defaultErr = defaultMakeFn()
	})
	if defaultErr != nil {
		return nil, defaultErr
	}
	return defaultChunk, nil
}

// New builds a Chunker for the named encoding. Only cl100k_base is
// supported — it's the vocab embedded in the binary; the parameter is
// kept for API compatibility with the previous tiktoken-go-backed
// implementation and is validated against the embedded vocab.
func New(encodingName string) (*Chunker, error) {
	if encodingName != "" && encodingName != "cl100k_base" {
		return nil, fmt.Errorf("chunking: unsupported encoding %q (only cl100k_base is embedded)", encodingName)
	}

	// ztok's BPE loader takes a file path. Materialize the embedded vocab
	// to a temp file, load it, then remove it — the loader parses the
	// whole file into an in-memory SoA vocab table, so the file is not
	// needed after OpenTiktoken returns.
	f, err := os.CreateTemp("", "r2g-cl100k-*.tiktoken")
	if err != nil {
		return nil, fmt.Errorf("chunking: create temp vocab: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	if _, err := f.Write(cl100kVocab); err != nil {
		f.Close()
		return nil, fmt.Errorf("chunking: write temp vocab: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("chunking: close temp vocab: %w", err)
	}

	pipe, err := ztok.OpenTiktoken(tmpPath, &ztok.Config{CL100K: true})
	if err != nil {
		return nil, fmt.Errorf("chunking: load cl100k vocab: %w", err)
	}
	return &Chunker{pipe: pipe}, nil
}

// Chunk splits text into overlapping token windows.
//
// Algorithm: encode the entire body once, then slide a window of
// `chunkSize` tokens with `chunkOverlap` step-back. The final chunk
// is whatever remains, even if it's shorter than chunkSize.
//
// The token-window approach matches R2R's behaviour and means chunk
// boundaries do not respect markdown structure (a chunk may begin
// mid-sentence). That's intentional: token-bounded chunks give the
// embedding model a consistent context length, which is what cosine
// similarity scoring assumes.
//
// chunkSize <= 0 or chunkOverlap < 0 fall back to defaults.
// chunkOverlap >= chunkSize is clamped to chunkSize-1 to avoid an
// infinite loop.
func (c *Chunker) Chunk(text string, chunkSize, chunkOverlap int) ([]string, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	if chunkOverlap < 0 {
		chunkOverlap = DefaultChunkOverlap
	}
	if chunkOverlap >= chunkSize {
		chunkOverlap = chunkSize - 1
	}

	if strings.TrimSpace(text) == "" {
		return nil, nil
	}

	tokens, err := c.pipe.Encode(text)
	if err != nil {
		return nil, fmt.Errorf("chunking: encode: %w", err)
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	step := chunkSize - chunkOverlap
	var out []string
	for start := 0; start < len(tokens); start += step {
		end := start + chunkSize
		if end > len(tokens) {
			end = len(tokens)
		}
		window := tokens[start:end]
		// Decode handles surrogate pairs and partial multibyte codepoints
		// at the window edges; trim leading/trailing whitespace so chunk
		// text is presentation-ready.
		decoded, err := c.pipe.Decode(window)
		if err != nil {
			return nil, fmt.Errorf("chunking: decode window [%d:%d]: %w", start, end, err)
		}
		piece := strings.TrimSpace(decoded)
		if piece != "" {
			out = append(out, piece)
		}
		// Stop when the window reaches the end. Otherwise the next
		// iteration would re-emit the same tail.
		if end == len(tokens) {
			break
		}
	}
	return out, nil
}

// CountTokens returns the token count for a string. Useful for sanity-
// checking input sizes before doing the chunk pass, and for the
// document's total_tokens metadata field. ztok has no count-only call,
// so this encodes and returns the length; on encode error it returns 0.
func (c *Chunker) CountTokens(text string) int {
	ids, err := c.pipe.Encode(text)
	if err != nil {
		return 0
	}
	return len(ids)
}

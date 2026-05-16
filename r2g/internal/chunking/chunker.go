// Package chunking splits a document body into overlapping token-bounded
// chunks ready for embedding. r2g uses tiktoken-go with the cl100k_base
// BPE vocab — same tokenizer family R2R uses, so chunk-token-counts
// stay comparable across the strangler-fig boundary.
//
// The cl100k vocab is loaded from the offline asset bundled inside
// tiktoken-go-loader, so the binary doesn't need a network round-trip
// or a writable TIKTOKEN_CACHE_DIR. RSS cost is roughly 60 MB after
// the first GetEncoding call (the BPE map is the big tenant).
package chunking

import (
	"fmt"
	"strings"
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// Defaults match the rough shape of R2R's effective chunk distribution
// on the existing AI Papers corpus (median ~800 tokens, p95 ~1200). The
// 256-token overlap keeps section boundaries from cleaving mid-sentence
// when downstream retrieval mosaics across chunks.
const (
	DefaultChunkSize    = 1024
	DefaultChunkOverlap = 256
)

// Chunker is a thread-safe wrapper around a single tiktoken encoding.
// One instance per process; lookups are O(1) so there's no benefit to
// per-request reuse beyond eliminating the encoding-init cost.
type Chunker struct {
	enc *tiktoken.Tiktoken
}

var (
	loaderOnce    sync.Once
	loaderErr     error
	defaultChunk  *Chunker
	defaultOnce   sync.Once
	defaultMakeFn = func() (*Chunker, error) { return New("cl100k_base") }
)

// Default returns a process-wide singleton Chunker using cl100k_base.
// The first call pays the BPE-init cost (~60 MB RSS, ~80 ms wallclock);
// subsequent calls are free.
func Default() (*Chunker, error) {
	var err error
	defaultOnce.Do(func() {
		defaultChunk, err = defaultMakeFn()
	})
	if err != nil {
		return nil, err
	}
	return defaultChunk, nil
}

// New builds a Chunker for the named tiktoken encoding (cl100k_base,
// o200k_base, p50k_base, r50k_base). The offline loader is installed
// once at process start; subsequent New() calls are cheap.
func New(encodingName string) (*Chunker, error) {
	loaderOnce.Do(func() {
		// SetBpeLoader is global; calling it more than once is harmless
		// but races on the package-level variable, so we gate it behind
		// sync.Once. tiktoken-go-loader bundles cl100k/o200k/p50k/r50k
		// .tiktoken files via go:embed so no network is needed.
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	})
	if loaderErr != nil {
		return nil, loaderErr
	}
	enc, err := tiktoken.GetEncoding(encodingName)
	if err != nil {
		return nil, fmt.Errorf("get tiktoken encoding %q: %w", encodingName, err)
	}
	return &Chunker{enc: enc}, nil
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

	tokens := c.enc.Encode(text, nil, nil)
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
		piece := strings.TrimSpace(c.enc.Decode(window))
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
// document's total_tokens metadata field.
func (c *Chunker) CountTokens(text string) int {
	return len(c.enc.Encode(text, nil, nil))
}

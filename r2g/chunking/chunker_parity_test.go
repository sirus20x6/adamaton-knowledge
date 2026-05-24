package chunking

import (
	"strings"
	"testing"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

// parityCorpus mixes prose, code, unicode, whitespace, numbers, and a
// long repeated paragraph — the shapes the AI Papers corpus throws at the
// chunker.
var parityCorpus = []string{
	"",
	"   \n\t  ",
	"The quick brown fox jumps over the lazy dog.",
	"func main() {\n\tfmt.Println(\"hello, world\")\n}\n",
	"Embeddings rely on a consistent context length; cosine similarity assumes it.",
	"Unicode: café, naïve, 日本語, emoji 🚀🔥, and a combining é.",
	"   leading/trailing whitespace   \n\n\ttabs\tand   spaces  ",
	strings.Repeat("token ", 500),
	"Mixed 123,456.78 numbers + symbols !@#$%^&*() and URLs https://example.com/path?q=1",
	"A long paragraph. " + strings.Repeat("The strangler-fig migration keeps chunk-token-counts comparable across the boundary. ", 80),
}

// newTiktokenRef builds a tiktoken-go cl100k_base encoder to use as the
// ground-truth reference the ztok-backed Chunker must match.
func newTiktokenRef(t *testing.T) *tiktoken.Tiktoken {
	t.Helper()
	tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		t.Fatalf("tiktoken GetEncoding: %v", err)
	}
	return enc
}

// TestCountTokensMatchesTiktoken asserts ztok's token counts equal
// tiktoken-go's exactly. Equal counts across the corpus is the core
// guarantee that lets us swap tokenizers without a reindex.
func TestCountTokensMatchesTiktoken(t *testing.T) {
	c, err := New("cl100k_base")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := newTiktokenRef(t)

	for i, s := range parityCorpus {
		want := len(ref.Encode(s, nil, nil))
		got := c.CountTokens(s)
		if want != got {
			t.Errorf("sample %d: CountTokens=%d, tiktoken=%d", i, got, want)
		}
	}
}

// TestChunkMatchesTiktokenReference replicates the chunk algorithm using
// the tiktoken-go reference encoder and asserts the ztok-backed Chunker
// produces byte-identical chunk slices across a range of size/overlap
// settings. If this ever fails, chunk boundaries have moved and a
// reindex would be required.
func TestChunkMatchesTiktokenReference(t *testing.T) {
	c, err := New("cl100k_base")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := newTiktokenRef(t)

	settings := []struct{ size, overlap int }{
		{DefaultChunkSize, DefaultChunkOverlap},
		{64, 16},
		{128, 0},
		{32, 31}, // overlap clamps to size-1
		{0, -1},  // falls back to defaults
	}

	for _, st := range settings {
		for i, s := range parityCorpus {
			got, err := c.Chunk(s, st.size, st.overlap)
			if err != nil {
				t.Fatalf("sample %d size=%d ov=%d: Chunk err: %v", i, st.size, st.overlap, err)
			}
			want := referenceChunk(ref, s, st.size, st.overlap)
			if len(got) != len(want) {
				t.Errorf("sample %d size=%d ov=%d: %d chunks, want %d", i, st.size, st.overlap, len(got), len(want))
				continue
			}
			for j := range want {
				if got[j] != want[j] {
					t.Errorf("sample %d size=%d ov=%d: chunk %d differs\n got=%q\nwant=%q", i, st.size, st.overlap, j, got[j], want[j])
				}
			}
		}
	}
}

// referenceChunk mirrors Chunker.Chunk exactly but uses the tiktoken-go
// encoder, giving an independent ground truth.
func referenceChunk(enc *tiktoken.Tiktoken, text string, chunkSize, chunkOverlap int) []string {
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
		return nil
	}
	tokens := enc.Encode(text, nil, nil)
	if len(tokens) == 0 {
		return nil
	}
	step := chunkSize - chunkOverlap
	var out []string
	for start := 0; start < len(tokens); start += step {
		end := start + chunkSize
		if end > len(tokens) {
			end = len(tokens)
		}
		piece := strings.TrimSpace(enc.Decode(tokens[start:end]))
		if piece != "" {
			out = append(out, piece)
		}
		if end == len(tokens) {
			break
		}
	}
	return out
}

// TestUnsupportedEncoding guards the validation path.
func TestUnsupportedEncoding(t *testing.T) {
	if _, err := New("o200k_base"); err == nil {
		t.Fatal("expected error for unsupported encoding, got nil")
	}
	if _, err := New(""); err != nil {
		t.Fatalf("empty encoding should default to cl100k_base, got: %v", err)
	}
}

// TestConcurrentEncode exercises the shared-pipeline concurrency claim:
// the process-wide Default() chunker hit from many goroutines must not
// race or corrupt output.
func TestConcurrentEncode(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	const goroutines = 16
	errc := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < 50; i++ {
				if _, err := c.Chunk(parityCorpus[9], 256, 64); err != nil {
					errc <- err
					return
				}
			}
			errc <- nil
		}()
	}
	for g := 0; g < goroutines; g++ {
		if err := <-errc; err != nil {
			t.Fatalf("concurrent Chunk: %v", err)
		}
	}
}

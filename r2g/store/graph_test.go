package store

import (
	"reflect"
	"testing"
)

// TestTokenizeQuery_EmptyInput verifies the empty-query short-circuit
// (caller treats nil as "skip the graph-vote backend in HybridSearch").
func TestTokenizeQuery_EmptyInput(t *testing.T) {
	if got := tokenizeQuery(""); got != nil {
		t.Errorf("tokenizeQuery(\"\") = %v, want nil", got)
	}
}

// TestTokenizeQuery_StopWordsDropped covers the degenerate path where
// every token is filtered. "of" is also dropped — but by the < 3 char
// rule, not the stop-word set. Either way the result must be nil so
// the route layer skips the seed-extraction DB call.
func TestTokenizeQuery_StopWordsDropped(t *testing.T) {
	if got := tokenizeQuery("the and of"); got != nil {
		t.Errorf("tokenizeQuery(\"the and of\") = %v, want nil", got)
	}
}

// TestTokenizeQuery_PreservesContentTokens pins the happy path: two
// real terms survive in first-occurrence order.
func TestTokenizeQuery_PreservesContentTokens(t *testing.T) {
	got := tokenizeQuery("transformer architecture")
	want := []string{"transformer", "architecture"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenizeQuery(\"transformer architecture\") = %v, want %v", got, want)
	}
}

// TestTokenizeQuery_DedupeLowercase confirms repeats collapse and the
// canonical form is lowercase. Order of first occurrence is preserved
// (the dedup map only suppresses subsequent copies).
func TestTokenizeQuery_DedupeLowercase(t *testing.T) {
	got := tokenizeQuery("Transformer transformer TRANSFORMER")
	want := []string{"transformer"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenizeQuery dedup case = %v, want %v", got, want)
	}
}

// TestTokenizeQuery_TooShort confirms tokens shorter than 3 characters
// are dropped. "AI" lowercases to "ai" (len 2) and disappears, leaving
// no tokens to bind.
func TestTokenizeQuery_TooShort(t *testing.T) {
	if got := tokenizeQuery("AI"); got != nil {
		t.Errorf("tokenizeQuery(\"AI\") = %v, want nil", got)
	}
}

// TestTokenizeQuery_QuestionForm exercises a realistic /search query.
// Stop-words and punctuation drop out; "does" is NOT in queryStopWords,
// so it survives the filter — the test pins that intentionally so a
// future broadening of the stop-word set is caught here.
func TestTokenizeQuery_QuestionForm(t *testing.T) {
	got := tokenizeQuery("What does the paper say about attention?")
	want := []string{"does", "paper", "say", "about", "attention"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenizeQuery question form = %v, want %v", got, want)
	}
}

// TestTokenizeQuery_NonAlphanumericSplit pins the FieldsFunc splitter:
// "GPT-4o GPT-4o-mini" splits on '-' and ' ' to ["GPT","4o","GPT","4o","mini"].
// "4o" is lowercased then dropped by the 3-char floor; "gpt" dedups to
// one occurrence; "mini" passes through. The result is the alphanumeric
// kernel of the original — exactly what we want to bind against the
// entity table.
func TestTokenizeQuery_NonAlphanumericSplit(t *testing.T) {
	got := tokenizeQuery("GPT-4o GPT-4o-mini")
	want := []string{"gpt", "mini"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokenizeQuery(\"GPT-4o GPT-4o-mini\") = %v, want %v", got, want)
	}
}

// TestTokenizeQuery_TableDriven sweeps the boundary conditions in one
// place. Each row is a self-contained behavioural pin — adding a new
// row is cheaper than another top-level test function.
func TestTokenizeQuery_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"whitespace only", "   \t\n  ", nil},
		{"punctuation only", "!!! ??? ...", nil},
		{"single stop word", "the", nil},
		{"mixed stop + content", "the transformer", []string{"transformer"}},
		{"leading/trailing punctuation", "  transformer!  ", []string{"transformer"}},
		{"unicode letter", "café", []string{"café"}},
		// queryStopWords includes "it's" — but the apostrophe is a
		// non-alphanumeric splitter, so the token never gets a chance
		// to match. The stop-word entry for "it's" is therefore dead
		// code in practice; we pin the actual behaviour rather than
		// the intent.
		{"its apostrophe", "it's working", []string{"working"}},
		{"3-char boundary", "abc ab abcd", []string{"abc", "abcd"}},
		{"digits only token", "123 456 789", []string{"123", "456", "789"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenizeQuery(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("tokenizeQuery(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

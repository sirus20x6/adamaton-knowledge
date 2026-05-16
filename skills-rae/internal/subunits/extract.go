// Package subunits implements the deterministic subunit extractor from
// SkillRAE §3.2. Given a SKILL.md body, it returns a slice of
// normalized subunits classified into three kinds:
//
//   - procedural: an imperative step (\"Search Google Scholar for the
//     paper\"). Pulled from bullet lists or numbered steps where the
//     first token after the marker is a known imperative verb.
//   - element:    a structural reference (\"BibTeX entries live in
//     assets/bibtex_template.bib\"). Identified by a path-like
//     token of the form ``dir/file.ext`` or a bare directory like
//     ``scripts/``.
//   - constraint: a usage statement (\"This skill should be used when
//     you need to find papers\"). Identified by a small phrase
//     dictionary.
//
// All three detectors run over every line; a line can yield multiple
// subunits (rare but legal — a constraint sentence containing a path
// reference would emit both). Normalization happens at the end so the
// content stored in evo.skill_subunits is stable across re-extractions.
package subunits

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Subunit is one row destined for evo.skill_subunits + an edge to
// evo.skill_subunit_edges (line_no is the source location in the
// pre-normalized body, kept for jump-to-source in the UI).
type Subunit struct {
	Content string // normalized, post-Normalize
	Hash    string // sha256 hex of Content
	Kind    string // procedural | element | constraint
	Length  int    // len([]rune(Content))
	LineNo  int    // 1-based
}

// MinLength and MaxLength bound what we keep. Lines shorter than 20
// chars are too generic to be useful (\"do it\", \"see below\"), and
// lines longer than 200 are usually paragraphs that should be split
// into multiple subunits — but our split is sentence-naive, so we
// just drop them. This matches the paper's wording (\"filtered by
// length\").
const (
	MinLength = 20
	MaxLength = 200
)

// Extract walks ``body``, runs every detector against every line, and
// returns the deduplicated subunit set. Dedup is BY HASH and WITHIN the
// same body — global dedup across the corpus happens at INSERT time
// via the UNIQUE constraint on content_hash. The order of returned
// subunits is stable (by first-seen line_no) so test fixtures stay
// readable.
func Extract(body string) []Subunit {
	lines := strings.Split(body, "\n")
	seen := make(map[string]struct{}, len(lines))
	out := make([]Subunit, 0, len(lines))

	emit := func(raw, kind string, lineNo int) {
		content, ok := Normalize(raw)
		if !ok {
			return
		}
		sum := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(sum[:])
		if _, dup := seen[hash]; dup {
			return
		}
		seen[hash] = struct{}{}
		out = append(out, Subunit{
			Content: content,
			Hash:    hash,
			Kind:    kind,
			Length:  len([]rune(content)),
			LineNo:  lineNo,
		})
	}

	for i, line := range lines {
		lineNo := i + 1
		// Procedural runs first so a bullet-headed line emits as
		// ``procedural`` rather than getting captured by the element
		// detector when it happens to also mention a path.
		for _, raw := range extractProcedural(line) {
			emit(raw, KindProcedural, lineNo)
		}
		for _, raw := range extractElement(line) {
			emit(raw, KindElement, lineNo)
		}
		for _, raw := range extractConstraint(line) {
			emit(raw, KindConstraint, lineNo)
		}
	}
	return out
}

// Kind constants — kept here rather than as untyped strings so callers
// can grep for usage sites and the DB enum is one source of truth.
const (
	KindProcedural = "procedural"
	KindElement    = "element"
	KindConstraint = "constraint"
)

package subunits

import (
	"regexp"
	"strings"
)

// rePath matches a path-like token. Conservative: requires at least
// one ``/`` and either a bare-directory trailing slash (``assets/``)
// or a file extension (``foo.ext``). Excludes URLs (``http://``) and
// option flags (``--foo``).
//
// Examples that match:
//
//	assets/bibtex_template.bib
//	references/google_scholar_search.md
//	scripts/
//	platform/backend/app/api/skills.py
//
// Examples that DON'T match:
//
//	http://example.com    (caught by the http prefix guard)
//	--include-deps        (no slash)
//	C:\foo                (windows paths; out of scope)
var rePath = regexp.MustCompile(
	`(?:^|[^/\w])((?:[a-z_][a-z0-9_-]*/)+(?:[a-z0-9_.-]+(?:\.[a-z0-9]+)?)?)\b`,
)

// reBareDir matches a bare directory reference at the start of a line
// or in the middle of prose — ``In assets/, you'll find...``. This
// catches cases where the path regex would not because there's no
// file at the tail.
var reBareDir = regexp.MustCompile(
	`(?:^|\s)([a-z_][a-z0-9_-]+/)(?:[\s,.;:!?]|$)`,
)

// elementSentenceFromLine returns the line with bullet markers
// stripped, suitable for handing to Normalize. We keep the whole
// containing line as the subunit content rather than just the path
// itself — the rationale is that ``assets/bibtex_template.bib`` alone
// is uselessly short, but ``BibTeX templates live in
// assets/bibtex_template.bib`` carries the actual semantic content.
func extractElement(line string) []string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return nil
	}
	// Reject http/https URLs early — they would otherwise match
	// rePath as ``example.com/foo``.
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return nil
	}
	stripped := stripBulletMarker(line)
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return nil
	}
	if !rePath.MatchString(stripped) && !reBareDir.MatchString(" "+stripped) {
		return nil
	}
	// One subunit per matching line — even if a line contains two
	// paths, the full-line subunit captures both. Splitting would
	// produce redundant near-duplicates that the dedup step would
	// have to drop.
	return []string{stripped}
}

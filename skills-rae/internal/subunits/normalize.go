package subunits

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// markdown inline-mark strippers. We remove the markers but keep the
// surrounding text so semantic content survives. Code fences (```)
// are deleted entirely — keeping them in a one-line subunit produces
// garbage. Inline backticks are stripped but the content kept.
var (
	reBoldItalic = regexp.MustCompile(`\*+([^*]+)\*+`)
	reCode       = regexp.MustCompile("`+([^`]+)`+")
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	reHTMLTag    = regexp.MustCompile(`<[^>]+>`)
	reBullet     = regexp.MustCompile(`^[\s>]*(?:[-*+]|\d+\.)\s+`)
	reHeading    = regexp.MustCompile(`^#+\s+`)
	reWhitespace = regexp.MustCompile(`\s+`)
)

// Normalize trims markdown markers, lowercases, collapses whitespace,
// and length-filters. Returns ok=false when the result falls outside
// [MinLength, MaxLength] (the caller drops it). The normalized form
// is what gets hashed for global dedup, so it must be deterministic.
//
// Order matters here: link text rewrite has to happen before bold/
// italic stripping or `[**foo**](bar)` would lose the outer brackets
// before the link rewrite saw them. HTML tag strip runs last so any
// `<br>` injected by the markdown source disappears.
func Normalize(raw string) (string, bool) {
	s := raw
	s = reHeading.ReplaceAllString(s, "")
	s = reBullet.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1")
	s = reBoldItalic.ReplaceAllString(s, "$1")
	s = reCode.ReplaceAllString(s, "$1")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = reWhitespace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	if s == "" {
		return "", false
	}
	n := utf8.RuneCountInString(s)
	if n < MinLength || n > MaxLength {
		return "", false
	}
	return s, true
}

// stripBulletMarker removes the leading bullet/number marker from a
// line so the procedural detector can match against the verb directly.
// Public so element + constraint detectors can use it too.
func stripBulletMarker(line string) string {
	return reBullet.ReplaceAllString(line, "")
}

// stripHeading removes a leading ``#`` heading marker.
func stripHeading(line string) string {
	return reHeading.ReplaceAllString(line, "")
}

package subunits

import (
	"regexp"
	"strings"
)

// constraintPhrases enumerates the trigger phrases that flag a
// constraint-kind subunit. Lower-case, case-insensitive match. The
// list errs on the side of recall: any of these phrases in the line
// counts the line as a constraint.
//
// Why a phrase list rather than a single regex: each phrase is
// conceptually distinct (precondition vs prohibition vs trigger) and
// keeping them named in code makes future ablation cheaper than
// unpacking a fused regex group.
var constraintPhrases = []string{
	"should be used when",
	"when to use",
	"only use when",
	"only when",
	"do not use",
	"don't use",
	"never use",
	"avoid using",
	"must",
	"never ",
	"always ",
	"requires that",
	"requires a",
	"requires the",
	"assumes",
	"precondition",
	"prerequisite",
	"cannot be used",
	"do not ",
	"don't ",
	"this skill",
	"use this skill",
	"applicable when",
	"not applicable",
}

// reConstraint is a single OR over the dictionary, compiled once.
// Word boundaries aren't sufficient — phrases like \"should be used
// when\" contain internal spaces, so we just do a substring match.
var reConstraint = func() *regexp.Regexp {
	escaped := make([]string, len(constraintPhrases))
	for i, p := range constraintPhrases {
		escaped[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(escaped, "|") + `)`)
}()

// extractConstraint pulls constraint-kind subunits from one raw line.
// Returns the line itself when any of the trigger phrases is present.
// Headings are excluded — they're not constraint statements even if
// the heading text happens to include \"must\".
func extractConstraint(line string) []string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return nil
	}
	stripped := stripBulletMarker(line)
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return nil
	}
	if !reConstraint.MatchString(stripped) {
		return nil
	}
	return []string{stripped}
}

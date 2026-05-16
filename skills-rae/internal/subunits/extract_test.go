package subunits

import (
	"strings"
	"testing"
)

// citationManagementSKILL is a synthetic SKILL.md modeled on the
// citation-management figure in the SkillRAE paper (Fig. 1). Used to
// pin down the extractor's expected behaviour across all three
// kinds. Keep tests against this fixture deterministic — adjusting
// the body invalidates the assertions below.
const citationManagementSKILL = `---
name: "citation-management"
description: "Comprehensive citation management for academic research."
---

## Overview

This skill should be used when you need to find papers, verify citation
information, convert DOIs to BibTeX, or ensure reference accuracy in
scientific writing.

## When to use

- Use this skill when extracting metadata from a publisher landing page.
- Avoid using this skill for non-academic citation styles (Chicago, AP).

## Procedure

1. Search Google Scholar for the candidate paper using title + first author.
2. Validate the returned DOI against assets/known_doi_prefixes.csv.
3. Convert DOIs to BibTeX entries via references/google_scholar_search.md.
4. Generate properly formatted BibTeX entries using the template at
   assets/bibtex_template.bib.
5. Verify the abstract field is non-empty before persisting.

## References

- assets/bibtex_template.bib
- references/google_scholar_search.md
- scripts/search_google_scholar.py

## Constraints

- This skill requires a network connection to Google Scholar.
- Never store API tokens in plain text.
- Must reject DOIs that fail the checksum check.
`

func TestExtract_KindCoverage(t *testing.T) {
	out := Extract(citationManagementSKILL)
	if len(out) < 6 {
		t.Fatalf("expected >=6 subunits, got %d: %#v", len(out), out)
	}
	counts := map[string]int{}
	for _, s := range out {
		counts[s.Kind]++
	}
	for _, k := range []string{KindProcedural, KindElement, KindConstraint} {
		if counts[k] == 0 {
			t.Errorf("expected at least one %s subunit; got counts=%v", k, counts)
		}
	}
}

func TestExtract_ProceduralCapturesNumberedSteps(t *testing.T) {
	out := Extract(citationManagementSKILL)
	wantPrefixes := []string{
		"search google scholar",
		"validate the returned doi",
		"convert dois to bibtex",
		"generate properly formatted bibtex",
		"verify the abstract field",
	}
	hits := 0
	for _, want := range wantPrefixes {
		for _, s := range out {
			if s.Kind == KindProcedural && strings.Contains(s.Content, want) {
				hits++
				break
			}
		}
	}
	if hits < 4 {
		t.Errorf("expected >=4 procedural hits out of %d prefixes; got %d. subunits=%+v",
			len(wantPrefixes), hits, debugSubunits(out))
	}
}

func TestExtract_ElementCapturesPaths(t *testing.T) {
	out := Extract(citationManagementSKILL)
	wantFragments := []string{
		"assets/bibtex_template.bib",
		"references/google_scholar_search.md",
		"scripts/search_google_scholar.py",
	}
	hits := 0
	for _, want := range wantFragments {
		for _, s := range out {
			if strings.Contains(s.Content, want) {
				hits++
				break
			}
		}
	}
	if hits < 3 {
		t.Errorf("expected all 3 path fragments to appear in subunits; got %d", hits)
	}
}

func TestExtract_ConstraintCatchesWhenToUse(t *testing.T) {
	out := Extract(citationManagementSKILL)
	foundWhenToUse := false
	foundMust := false
	foundNever := false
	for _, s := range out {
		if s.Kind != KindConstraint {
			continue
		}
		if strings.Contains(s.Content, "should be used when") {
			foundWhenToUse = true
		}
		if strings.Contains(s.Content, "must reject") {
			foundMust = true
		}
		if strings.Contains(s.Content, "never store") {
			foundNever = true
		}
	}
	if !foundWhenToUse {
		t.Error(`expected constraint subunit containing "should be used when"`)
	}
	if !foundMust {
		t.Error(`expected constraint subunit containing "must reject"`)
	}
	if !foundNever {
		t.Error(`expected constraint subunit containing "never store"`)
	}
}

func TestExtract_GlobalDedupWithinBody(t *testing.T) {
	doubled := citationManagementSKILL + "\n" + citationManagementSKILL
	out := Extract(doubled)
	hashes := map[string]struct{}{}
	for _, s := range out {
		if _, dup := hashes[s.Hash]; dup {
			t.Fatalf("duplicate hash within single body: %q (%s)", s.Content, s.Hash)
		}
		hashes[s.Hash] = struct{}{}
	}
}

func TestExtract_LengthFilterDropsShortAndLong(t *testing.T) {
	body := `- ok
- this line is too short
- ` + strings.Repeat("x ", MaxLength) + `
- search the index for the paper using the doi field as the lookup key`
	out := Extract(body)
	for _, s := range out {
		if s.Length < MinLength {
			t.Errorf("subunit shorter than MinLength: %d %q", s.Length, s.Content)
		}
		if s.Length > MaxLength {
			t.Errorf("subunit longer than MaxLength: %d %q", s.Length, s.Content)
		}
	}
}

func TestNormalize_StripsMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**Search** Google Scholar for the candidate paper using title", "search google scholar for the candidate paper using title"},
		{"Validate the DOI against `assets/known_doi.csv` before persisting", "validate the doi against assets/known_doi.csv before persisting"},
		{"See [the template](https://example.com/x) for the BibTeX layout", "see the template for the bibtex layout"},
	}
	for _, c := range cases {
		got, ok := Normalize(c.in)
		if !ok {
			t.Errorf("Normalize(%q) returned !ok", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtract_HeadingsAreNotSubunits(t *testing.T) {
	body := `# Title
## Subtitle
### Search the index when the user asks`
	out := Extract(body)
	if len(out) != 0 {
		t.Errorf("headings should not become subunits; got %+v", out)
	}
}

// debugSubunits flattens an extraction result for failure messages.
func debugSubunits(in []Subunit) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.Kind + ": " + s.Content
	}
	return out
}

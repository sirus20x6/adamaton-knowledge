package compile

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/retrieval"
)

func TestCompile_RendersHeadingPerSelected(t *testing.T) {
	skillA := uuid.New()
	out := Compile(Request{
		Query: "rename a function in models/user.go",
		RetrieveResp: &retrieval.Response{
			Selected: []retrieval.SelectedSkill{{
				SkillID: skillA, Name: "rename-variable", Description: "Pick a better identifier",
				Score: 0.7, Boosted: false,
			}},
			Highlights: map[uuid.UUID][]retrieval.Highlight{
				skillA: {{
					Content: "Prefer a noun-phrase form when the variable holds a domain object",
					Kind:    "procedural", Similarity: 0.8,
				}},
			},
		},
	})
	if !strings.Contains(out.Context, "# RELEVANT SKILLS") {
		t.Error("missing header")
	}
	if !strings.Contains(out.Context, "## rename-variable") {
		t.Errorf("missing skill heading; got: %s", out.Context)
	}
	if !strings.Contains(out.Context, "Highlights") {
		t.Errorf("missing Highlights section; got: %s", out.Context)
	}
}

func TestCompile_FilterRescueByRelevance(t *testing.T) {
	skillA := uuid.New()
	hostSkill := uuid.New()
	rescue := []retrieval.RescueRecord{
		{SubunitID: uuid.New(), SourceSkillID: skillA, Content: "search the index for a paper by doi using google scholar lookup", Kind: "procedural", Similarity: 0.6},
		{SubunitID: uuid.New(), SourceSkillID: skillA, Content: "short", Kind: "procedural", Similarity: 0.8}, // too short
		{SubunitID: uuid.New(), SourceSkillID: skillA, Content: "this content is too long but mostly irrelevant noise filler", Kind: "procedural", Similarity: 0.1}, // sim too low
	}
	out := Compile(Request{
		Query: "lookup paper by doi",
		RetrieveResp: &retrieval.Response{
			Selected: []retrieval.SelectedSkill{{
				SkillID: hostSkill, Name: "citation-management", Description: "Find papers",
				Score: 0.8, CommunityID: nil,
			}},
			Rescue: rescue,
		},
	})
	// First one should pass: sim 0.6 > 0.35 and long enough.
	if len(out.RescueAttached) == 0 {
		t.Errorf("expected rescue attached; got 0. skipped=%+v", out.Skipped)
	}
	for _, c := range out.RescueAttached {
		if c.HostSkillID != hostSkill {
			t.Errorf("attached to wrong host skill: %s", c.HostSkillID)
		}
	}
}

func TestCompile_OutputContractRendered(t *testing.T) {
	skillA := uuid.New()
	out := Compile(Request{
		Query:          "x",
		OutputContract: "- Return one diff per file\n- Do not modify generated code",
		RetrieveResp: &retrieval.Response{
			Selected: []retrieval.SelectedSkill{{
				SkillID: skillA, Name: "n", Description: "d", Score: 0.5,
			}},
		},
	})
	if !strings.Contains(out.Context, "## Output contract") {
		t.Error("missing Output contract heading")
	}
	if !strings.Contains(out.Context, "one diff per file") {
		t.Error("output contract content not rendered")
	}
}

func TestJaccardTokens_NoOverlap(t *testing.T) {
	if got := jaccardTokens("rename a function", "deploy pipeline aws"); got != 0 {
		t.Errorf("expected zero; got %.3f", got)
	}
	if got := jaccardTokens("rename a function", "rename a method now"); got <= 0 {
		t.Errorf("expected positive; got %.3f", got)
	}
}

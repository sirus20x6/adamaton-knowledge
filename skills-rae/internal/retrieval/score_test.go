package retrieval

import (
	"testing"

	"github.com/google/uuid"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/store"
)

func TestProjectSubunitsToSkills_DegreeNormalization(t *testing.T) {
	// Two skills, one shared subunit (deg=2) and one private to A.
	// Shared subunit at sim=1.0 should contribute 0.5 to each skill;
	// private subunit at sim=0.8 contributes 0.8 to A only.
	skillA := uuid.New()
	skillB := uuid.New()
	uShared := uuid.New()
	uPrivate := uuid.New()
	hits := []store.SubunitHit{
		{ID: uShared, SkillID: skillA, Similarity: 1.0, Degree: 2},
		{ID: uShared, SkillID: skillB, Similarity: 1.0, Degree: 2},
		{ID: uPrivate, SkillID: skillA, Similarity: 0.8, Degree: 1},
	}
	out := projectSubunitsToSkills(hits)
	if got, want := out[skillA], 0.5+0.8; absDiff(got, want) > 1e-9 {
		t.Errorf("skillA l0 = %.6f, want %.6f", got, want)
	}
	if got, want := out[skillB], 0.5; absDiff(got, want) > 1e-9 {
		t.Errorf("skillB l0 = %.6f, want %.6f", got, want)
	}
}

func TestMinMaxNormalize(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	in := map[uuid.UUID]float64{a: 1.0, b: 2.0, c: 5.0}
	out := minMaxNormalize(in)
	if out[a] != 0.0 {
		t.Errorf("min should normalize to 0; got %.3f", out[a])
	}
	if out[c] != 1.0 {
		t.Errorf("max should normalize to 1; got %.3f", out[c])
	}
	if absDiff(out[b], 0.25) > 1e-9 {
		t.Errorf("mid expected 0.25; got %.4f", out[b])
	}
}

func TestMinMaxNormalize_AllEqual(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	in := map[uuid.UUID]float64{a: 0.7, b: 0.7}
	out := minMaxNormalize(in)
	if out[a] != 1.0 || out[b] != 1.0 {
		t.Errorf("all-equal input should map to 1.0; got %v", out)
	}
}

func TestNameScore_TokenOverlap(t *testing.T) {
	if got := nameScore("rename a function", "rename-variable"); got <= 0 {
		t.Errorf("expected nonzero name score for query/name with overlap; got %.3f", got)
	}
	if got := nameScore("rename a function", "deploy-pipeline"); got != 0 {
		t.Errorf("expected zero name score for no overlap; got %.3f", got)
	}
	if got := nameScore("", "anything"); got != 0 {
		t.Errorf("empty query should score 0; got %.3f", got)
	}
}

func TestUnionCandidates_KeepsL1Hits(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	l1 := []store.SkillL1Hit{
		{ID: a, Name: "alpha", Description: "first", L1: 0.9},
		{ID: b, Name: "beta", Description: "second", L1: 0.5},
	}
	l0 := map[uuid.UUID]float64{a: 0.7}
	cands := unionCandidates(l1, l0, nil)
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

package rollup

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

type fakeLeafSource struct{ leaves []Leaf }

func (f fakeLeafSource) Load(context.Context, string) ([]Leaf, error) { return f.leaves, nil }

type fakeClusterer struct{ tree *ClusterTree }

func (f fakeClusterer) Cluster(context.Context, []Leaf) (*ClusterTree, error) { return f.tree, nil }

type fakeSummarizer struct {
	inputsByLevel map[int][][]string
}

func (f *fakeSummarizer) Summarize(_ context.Context, _ string, level int, inputs []string) (string, error) {
	if f.inputsByLevel == nil {
		f.inputsByLevel = map[int][][]string{}
	}
	f.inputsByLevel[level] = append(f.inputsByLevel[level], inputs)
	return fmt.Sprintf("summary-L%d-of-%d", level, len(inputs)), nil
}

type insertCall struct {
	level   int
	members []uuid.UUID
}
type parentCall struct {
	children []uuid.UUID
	parent   uuid.UUID
}
type fakeWriter struct {
	inserts []insertCall
	parents []parentCall
}

func (w *fakeWriter) InsertSummary(_ context.Context, _ string, level int, _ string, members []uuid.UUID) (uuid.UUID, error) {
	w.inserts = append(w.inserts, insertCall{level: level, members: members})
	return uuid.New(), nil
}
func (w *fakeWriter) SetParent(_ context.Context, children []uuid.UUID, parent uuid.UUID) error {
	w.parents = append(w.parents, parentCall{children: children, parent: parent})
	return nil
}

// TestBuild_TwoLevelTree checks bottom-up summarization, level inversion
// (sidecar 0=broadest -> documents level increasing toward root), and
// parent wiring (leaf-most communities parent their leaves; the broad
// community parents the two child summaries and summarizes their
// summaries, not the raw leaves).
func TestBuild_TwoLevelTree(t *testing.T) {
	l1, l2, l3, l4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	leaves := []Leaf{
		{ID: l1, Content: "alpha"}, {ID: l2, Content: "beta"},
		{ID: l3, Content: "gamma"}, {ID: l4, Content: "delta"},
	}
	zero := 0
	tree := &ClusterTree{
		Levels: 2,
		Communities: []Community{
			{TempID: 0, ParentTempID: nil, Level: 0, MemberIDs: []uuid.UUID{l1, l2, l3, l4}},
			{TempID: 1, ParentTempID: &zero, Level: 1, MemberIDs: []uuid.UUID{l1, l2}},
			{TempID: 2, ParentTempID: &zero, Level: 1, MemberIDs: []uuid.UUID{l3, l4}},
		},
	}
	sum := &fakeSummarizer{}
	w := &fakeWriter{}

	stats, err := Build(context.Background(), "chunk",
		fakeLeafSource{leaves}, fakeClusterer{tree}, sum, w)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if stats.Summaries != 3 {
		t.Fatalf("summaries = %d, want 3", stats.Summaries)
	}
	// Two leaf-most summaries at level 1, one root at level 2.
	if got := len(sum.inputsByLevel[1]); got != 2 {
		t.Fatalf("level-1 summaries = %d, want 2", got)
	}
	if got := len(sum.inputsByLevel[2]); got != 1 {
		t.Fatalf("level-2 summaries = %d, want 1", got)
	}
	// The root summarizes two child summaries, not four raw leaves.
	rootInputs := sum.inputsByLevel[2][0]
	if len(rootInputs) != 2 {
		t.Fatalf("root summarized %d inputs, want 2 (child summaries)", len(rootInputs))
	}
	for _, in := range rootInputs {
		if in != "summary-L1-of-2" {
			t.Fatalf("root input = %q, want a level-1 summary", in)
		}
	}
	// Leaf-most summaries each abstract two raw leaves.
	for _, in := range sum.inputsByLevel[1] {
		if len(in) != 2 {
			t.Fatalf("leaf-most summarized %d inputs, want 2", len(in))
		}
	}

	// Parent wiring: 3 SetParent calls — two parenting 2 leaves each, one
	// parenting the 2 child summaries.
	if len(w.parents) != 3 {
		t.Fatalf("SetParent calls = %d, want 3", len(w.parents))
	}
	var leafParenting, summaryParenting int
	for _, p := range w.parents {
		if len(p.children) != 2 {
			t.Fatalf("SetParent children = %d, want 2", len(p.children))
		}
		isLeaf := false
		for _, id := range p.children {
			if id == l1 || id == l2 || id == l3 || id == l4 {
				isLeaf = true
			}
		}
		if isLeaf {
			leafParenting++
		} else {
			summaryParenting++
		}
	}
	if leafParenting != 2 || summaryParenting != 1 {
		t.Fatalf("parenting split: leaf=%d summary=%d, want 2/1", leafParenting, summaryParenting)
	}
}

// TestBuild_Empty returns zero stats with no leaves.
func TestBuild_Empty(t *testing.T) {
	stats, err := Build(context.Background(), "chunk",
		fakeLeafSource{nil}, fakeClusterer{&ClusterTree{}}, &fakeSummarizer{}, &fakeWriter{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if stats.Summaries != 0 || stats.Leaves != 0 {
		t.Fatalf("non-zero stats on empty: %+v", stats)
	}
}

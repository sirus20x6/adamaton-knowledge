// Package rollup builds an H-Mem summary tree over a set of leaf nodes
// using the RAPTOR pattern: cluster the leaves into a multi-level
// community hierarchy, then summarize bottom-up so each parent node
// abstracts its children. Clustering, summarization, leaf loading, and
// tree persistence are all injected as interfaces, so the orchestration
// here is pure and unit-testable, and the concrete strategies (which
// embedder/chunker, which clustering algorithm) stay swappable.
//
// The clustering hierarchy is produced in a single pass (the deployed
// UMAP+HDBSCAN cluster sidecar cuts its condensed tree at several
// epsilons), so unlike vanilla RAPTOR we do not re-embed and re-cluster
// per level — we summarize over the fixed community hierarchy.
package rollup

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// Leaf is one level-0 node to be clustered and summarized.
type Leaf struct {
	ID        uuid.UUID
	Content   string
	Embedding []float32
}

// Community is one node of the clustering hierarchy. Level follows the
// sidecar convention: 0 = broadest, max = leaf-most. MemberIDs are the
// transitive leaf ids beneath this community.
type Community struct {
	TempID       int
	ParentTempID *int
	Level        int
	MemberIDs    []uuid.UUID
}

// ClusterTree is the full hierarchy plus the outliers HDBSCAN rejected.
type ClusterTree struct {
	Communities []Community
	Levels      int
	NoiseIDs    []uuid.UUID
}

// LeafSource loads the level-0 nodes for a kind. The documents-backed
// implementation reads summary_embedding + summary; a future chunk- or
// late-chunking-backed implementation satisfies the same contract.
type LeafSource interface {
	Load(ctx context.Context, kind string) ([]Leaf, error)
}

// Clusterer groups leaves into a hierarchy. Backed by the cluster
// sidecar (clusterclient); a Leiden/GMM backend would slot in here.
type Clusterer interface {
	Cluster(ctx context.Context, leaves []Leaf) (*ClusterTree, error)
}

// Summarizer abstracts a set of child texts into one parent summary.
type Summarizer interface {
	Summarize(ctx context.Context, kind string, level int, inputs []string) (string, error)
}

// Writer persists summary nodes and wires the parent_id tree. Summary
// nodes are documents rows at level >= 1; SetParent points children
// (leaf documents or lower summaries) at their parent summary.
type Writer interface {
	InsertSummary(ctx context.Context, kind string, level int, content string, memberLeafIDs []uuid.UUID) (uuid.UUID, error)
	SetParent(ctx context.Context, childIDs []uuid.UUID, parentID uuid.UUID) error
}

// Stats reports what a Build run produced.
type Stats struct {
	Leaves      int
	Communities int
	Summaries   int
	Noise       int
}

// Build runs one rollup pass for a kind: load leaves, cluster, then
// summarize bottom-up writing one summary document per community and
// wiring parent_id. Idempotency across re-runs is the Writer's concern
// (e.g. delete prior summaries for the kind before inserting).
func Build(
	ctx context.Context,
	kind string,
	src LeafSource,
	cl Clusterer,
	sum Summarizer,
	w Writer,
) (Stats, error) {
	leaves, err := src.Load(ctx, kind)
	if err != nil {
		return Stats{}, fmt.Errorf("load leaves: %w", err)
	}
	if len(leaves) == 0 {
		return Stats{}, nil
	}

	tree, err := cl.Cluster(ctx, leaves)
	if err != nil {
		return Stats{}, fmt.Errorf("cluster: %w", err)
	}

	leafContent := make(map[uuid.UUID]string, len(leaves))
	for _, l := range leaves {
		leafContent[l.ID] = l.Content
	}

	// Children index: temp_id -> direct child communities.
	children := map[int][]Community{}
	maxLevel := 0
	for _, c := range tree.Communities {
		if c.ParentTempID != nil {
			children[*c.ParentTempID] = append(children[*c.ParentTempID], c)
		}
		if c.Level > maxLevel {
			maxLevel = c.Level
		}
	}

	// Process bottom-up: leaf-most (highest sidecar level) first so a
	// parent's children already have summaries + doc ids when we reach it.
	ordered := append([]Community(nil), tree.Communities...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Level > ordered[j].Level
	})

	summaryText := map[int]string{}     // temp_id -> generated summary
	summaryDocID := map[int]uuid.UUID{} // temp_id -> documents.id
	stats := Stats{Leaves: len(leaves), Communities: len(tree.Communities), Noise: len(tree.NoiseIDs)}

	for _, c := range ordered {
		kids := children[c.TempID]

		// Gather the texts this community abstracts.
		var inputs []string
		if len(kids) == 0 {
			for _, id := range c.MemberIDs {
				if txt, ok := leafContent[id]; ok && txt != "" {
					inputs = append(inputs, txt)
				}
			}
		} else {
			for _, k := range kids {
				if txt, ok := summaryText[k.TempID]; ok && txt != "" {
					inputs = append(inputs, txt)
				}
			}
		}
		if len(inputs) == 0 {
			continue
		}

		// documents.level: leaf-most community -> 1, increasing toward root.
		docLevel := maxLevel - c.Level + 1

		summary, err := sum.Summarize(ctx, kind, docLevel, inputs)
		if err != nil {
			return stats, fmt.Errorf("summarize community %d: %w", c.TempID, err)
		}
		docID, err := w.InsertSummary(ctx, kind, docLevel, summary, c.MemberIDs)
		if err != nil {
			return stats, fmt.Errorf("insert summary %d: %w", c.TempID, err)
		}
		summaryText[c.TempID] = summary
		summaryDocID[c.TempID] = docID
		stats.Summaries++

		// Wire parent_id: leaf-most communities parent their member leaf
		// documents; higher communities parent their child summaries.
		if len(kids) == 0 {
			if err := w.SetParent(ctx, c.MemberIDs, docID); err != nil {
				return stats, fmt.Errorf("set parent (leaves) for %d: %w", c.TempID, err)
			}
		} else {
			childDocs := make([]uuid.UUID, 0, len(kids))
			for _, k := range kids {
				if id, ok := summaryDocID[k.TempID]; ok {
					childDocs = append(childDocs, id)
				}
			}
			if err := w.SetParent(ctx, childDocs, docID); err != nil {
				return stats, fmt.Errorf("set parent (summaries) for %d: %w", c.TempID, err)
			}
		}
	}

	return stats, nil
}

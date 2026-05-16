package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/store"
)

// GraphDeps owns the postgres pool for graph writes. Kept separate
// from EmbedDeps so the worker can wire them with separate retry
// policies if needed.
type GraphDeps struct {
	Pool *pgxpool.Pool
}

// UpsertGraphInput carries everything needed to atomically refresh a
// skill's subunit graph: the subunits themselves (already extracted
// and embedded) and the description embedding for the skill node.
type UpsertGraphInput struct {
	SkillID                uuid.UUID     `json:"skill_id"`
	Subunits               []SubunitWire `json:"subunits"`
	SubunitVectors         [][]float32   `json:"subunit_vectors"`
	DescriptionVector      []float32     `json:"description_vector"`
}

// UpsertGraphResult is the count summary. Callers feed it into the
// workflow result for telemetry.
type UpsertGraphResult struct {
	SubunitsTotal int            `json:"subunits_total"`
	SubunitsByKind map[string]int `json:"subunits_by_kind"`
}

// UpsertGraph wraps four DB calls in their natural order:
//
//  1. UpsertSubunits — content_hash dedup means the row may already
//     exist; we still get back its UUID so edges can target it.
//  2. DeleteEdgesForSkill — drop stale edges first so a re-index
//     after the skill body changes doesn't leave orphan rows.
//  3. UpsertEdges — fresh (skill_id, subunit_id, line_no) tuples.
//  4. StampDescriptionEmbedding — per-skill l1 input.
//
// All four run in sequence rather than one big tx because UpsertSubunits
// already takes a tx internally and we want it to commit before
// UpsertEdges so the FK is satisfied.
func (d *GraphDeps) UpsertGraph(ctx context.Context, in UpsertGraphInput) (*UpsertGraphResult, error) {
	if len(in.Subunits) != len(in.SubunitVectors) {
		return nil, fmt.Errorf("subunits (%d) != vectors (%d)", len(in.Subunits), len(in.SubunitVectors))
	}

	rows := make([]store.SubunitRow, len(in.Subunits))
	for i, s := range in.Subunits {
		rows[i] = store.SubunitRow{
			Content: s.Content,
			Hash:    s.Hash,
			Kind:    s.Kind,
			Length:  s.Length,
			Vector:  in.SubunitVectors[i],
		}
	}
	idsByHash, err := store.UpsertSubunits(ctx, d.Pool, rows)
	if err != nil {
		return nil, fmt.Errorf("upsert subunits: %w", err)
	}

	if err := store.DeleteEdgesForSkill(ctx, d.Pool, in.SkillID); err != nil {
		return nil, fmt.Errorf("delete edges: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(in.Subunits))
	lineNos := make([]int, 0, len(in.Subunits))
	byKind := map[string]int{}
	for _, s := range in.Subunits {
		id, ok := idsByHash[s.Hash]
		if !ok {
			continue
		}
		ids = append(ids, id)
		lineNos = append(lineNos, s.LineNo)
		byKind[s.Kind]++
	}
	if err := store.UpsertEdges(ctx, d.Pool, in.SkillID, ids, lineNos); err != nil {
		return nil, fmt.Errorf("upsert edges: %w", err)
	}

	if len(in.DescriptionVector) > 0 {
		if err := store.StampDescriptionEmbedding(ctx, d.Pool, in.SkillID, in.DescriptionVector); err != nil {
			return nil, fmt.Errorf("stamp description embedding: %w", err)
		}
	}

	if err := store.StampRAEIndexed(ctx, d.Pool, in.SkillID); err != nil {
		return nil, fmt.Errorf("stamp rae_indexed_at: %w", err)
	}

	return &UpsertGraphResult{
		SubunitsTotal:  len(in.Subunits),
		SubunitsByKind: byKind,
	}, nil
}

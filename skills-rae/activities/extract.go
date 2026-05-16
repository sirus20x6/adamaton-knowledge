package activities

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/subunits"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/store"
)

// SubunitWire is the per-activity wire format for a subunit. Used so
// EmbedAndPersistSubunits doesn't need to redo the regex extraction
// — the extraction activity does the work and the embed/persist
// activity consumes the result. Temporal histories stay small at
// scale since each skill's subunits compress well.
type SubunitWire struct {
	Content string `json:"content"`
	Hash    string `json:"hash"`
	Kind    string `json:"kind"`
	Length  int    `json:"length"`
	LineNo  int    `json:"line_no"`
}

// ExtractDeps is the activity-suite dependency bundle, injected once
// at worker startup. Holds the postgres pool so activities don't have
// to dial Postgres themselves.
type ExtractDeps struct {
	Pool *pgxpool.Pool
}

// ExtractSubunits loads the skill body from postgres and runs the
// deterministic extractor. Returns the wire-format subunits. Does not
// touch the database beyond reading the source row.
func (d *ExtractDeps) ExtractSubunits(ctx context.Context, in IndexSkillInput) ([]SubunitWire, error) {
	skill, err := store.LoadSkill(ctx, d.Pool, in.SkillID)
	if err != nil {
		return nil, err
	}
	if skill.Body == "" {
		return nil, fmt.Errorf("skill %s has empty body", skill.ID)
	}
	subs := subunits.Extract(skill.Body)
	out := make([]SubunitWire, len(subs))
	for i, s := range subs {
		out[i] = SubunitWire{
			Content: s.Content,
			Hash:    s.Hash,
			Kind:    s.Kind,
			Length:  s.Length,
			LineNo:  s.LineNo,
		}
	}
	return out, nil
}

// LoadSkillForEmbedding returns the description for the per-skill
// description_embedding step. Separate from ExtractSubunits because
// the IndexSkillWorkflow may want to batch the description embed
// with the subunit embeds.
func (d *ExtractDeps) LoadSkillForEmbedding(ctx context.Context, in IndexSkillInput) (SkillDescriptionWire, error) {
	skill, err := store.LoadSkill(ctx, d.Pool, in.SkillID)
	if err != nil {
		return SkillDescriptionWire{}, err
	}
	desc := skill.Description
	if desc == "" {
		desc = skill.Name
	}
	return SkillDescriptionWire{SkillID: skill.ID, Description: desc, Name: skill.Name}, nil
}

// SkillDescriptionWire carries description + name across the activity
// boundary so EmbedAndPersistSubunits can include the description in
// the embedding batch.
type SkillDescriptionWire struct {
	SkillID     uuid.UUID `json:"skill_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

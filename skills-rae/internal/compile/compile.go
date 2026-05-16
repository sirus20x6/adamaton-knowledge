// Package compile implements SkillRAE §3.3.2: the 4-step context
// compilation that converts {K, H, R, O} into a compact, grounded,
// immediately usable context block for the downstream agent.
//
//	Step 1 — filter rescue subunits Z(q) by relevance + non-redundancy
//	          + budget.
//	Step 2 — attach each filtered subunit to a selected skill via
//	          Aff(u,s|q) (eq 6).
//	Step 3 — generate task-specific guidance A_final(q): drop
//	          generic/low-affinity cues so the rendered block is
//	          concrete.
//	Step 4 — organise the markdown block including the task-output
//	          contract O(q).
package compile

import (
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/retrieval"
)

// AffWeights are the deterministic compatibility weights from eq 6.
// The ordering matches the feature vector below; tune on the
// SkillsBench dev split during Phase G.
type AffWeights struct {
	QRel    float64 // token overlap of subunit ∩ query
	FSkill  float64 // max token overlap of subunit ∩ skill highlights
	RSel    float64 // normalized retrieval score of the host skill
	GSame   float64 // 1 if source-skill and host-skill share community
	GActive float64 // 1 if host-skill is in B(q)
}

// DefaultAffWeights are the planning-doc defaults.
func DefaultAffWeights() AffWeights {
	return AffWeights{
		QRel:    0.30,
		FSkill:  0.25,
		RSel:    0.20,
		GSame:   0.10,
		GActive: 0.15,
	}
}

// Request bundles everything compile needs. The retrieval Response is
// the bulk; OutputContract is the optional O(q) — the delegator can
// pass per-task constraints (e.g. "return one diff per file"). Empty
// strings produce a block without an Output contract section.
//
// When ``RetrieveResp.QueryVector`` and ``RetrieveResp.SubunitVectors``
// are populated (the Retriever has ``ReturnVectors=true``) the
// pipeline uses real bge-m3 cosine for the q_rel and f_skill Aff
// features and for the non-redundancy dedup; otherwise it falls back
// to Jaccard token overlap. Cosine is strictly better when the
// vectors are available, but the Jaccard path lets the code be
// exercised in tests + lightweight environments.
type Request struct {
	Query            string
	OutputContract   string
	BudgetTokens     int
	RescueBudgetFrac float64 // default 0.25
	Affinities       AffWeights
	RetrieveResp     *retrieval.Response
}

// Compiled is the final payload — the rendered context block plus
// structured metadata for telemetry + the SkillsBench harness.
type Compiled struct {
	Context        string         `json:"context"`
	Selected       []retrieval.SelectedSkill `json:"selected_skills"`
	RescueAttached []AttachedCue  `json:"rescue_attached"`
	Skipped        []SkippedCue   `json:"skipped_cues"`
}

// AttachedCue is one rescue subunit that survived all four steps and
// landed on a selected skill in the final block.
type AttachedCue struct {
	HostSkillID  uuid.UUID `json:"host_skill_id"`
	SourceSkillID uuid.UUID `json:"source_skill_id"`
	Subunit      string    `json:"subunit"`
	Kind         string    `json:"kind"`
	AffScore     float64   `json:"aff_score"`
}

// SkippedCue is a rescue subunit that DIDN'T make it. Surfaced for
// debugging — agents can ignore it.
type SkippedCue struct {
	SubunitID uuid.UUID `json:"subunit_id"`
	Reason    string    `json:"reason"`
}

// minSubunitChars enforces the "concrete" filter in step 3 — anything
// shorter is either uselessly vague or got mangled in normalization.
const minSubunitChars = 30

// minRelevanceForRescue is the cutoff in step 1: subunit similarity
// to q must clear this bar to even enter Z(q). 0.35 chosen
// empirically — bge-m3 cosine sim on a relevant-but-not-trivial pair
// usually lands 0.4-0.7; below 0.35 we're in noise territory.
const minRelevanceForRescue = 0.35

// maxRedundancyJaccard is the upper bound on subunit-vs-subunit
// token overlap for inclusion in Z(q) in the Jaccard fallback path.
// >= 0.85 means "essentially duplicate"; we want compilation to be
// content-additive.
const maxRedundancyJaccard = 0.85

// maxRedundancyCosine is the equivalent threshold in cosine space.
// bge-m3 produces unit-norm vectors so cosine ∈ [-1, 1]; in practice
// "near duplicate" English content sits >= 0.90. 0.92 is the
// boundary that empirically separates "same procedure restated" from
// "related but distinct".
const maxRedundancyCosine = 0.92

// Compile runs the full 4-step pipeline. Deterministic given (query,
// retrieval response, weights) — no LLM calls.
func Compile(req Request) *Compiled {
	if req.RetrieveResp == nil {
		return &Compiled{}
	}
	budget := req.BudgetTokens
	if budget <= 0 {
		budget = 1500
	}
	rescueFrac := req.RescueBudgetFrac
	if rescueFrac <= 0 {
		rescueFrac = 0.25
	}
	weights := req.Affinities
	if weights == (AffWeights{}) {
		weights = DefaultAffWeights()
	}
	vecs := req.RetrieveResp.SubunitVectors

	// Step 1: filter rescue R(q) -> Z(q).
	z := filterRescue(req.Query, req.RetrieveResp.Rescue, budget, rescueFrac, vecs)

	// Step 2: attach each Z(q) subunit to its best selected skill.
	rsel := normalizedSelectionScores(req.RetrieveResp.Selected)
	communityBySkill := map[uuid.UUID]*uuid.UUID{}
	bActive := map[uuid.UUID]bool{}
	for _, s := range req.RetrieveResp.Selected {
		communityBySkill[s.SkillID] = s.CommunityID
		bActive[s.SkillID] = s.Boosted
	}
	attached := attachRescue(req.Query, z, req.RetrieveResp.Selected,
		req.RetrieveResp.Highlights, rsel, communityBySkill, bActive, weights, vecs)

	// Step 3: drop low-affinity / generic cues. Step 2 already
	// produces affinity scores; we apply a floor + concreteness
	// check here. Anything dropped feeds the Skipped list for
	// debugging.
	final, skipped := finaliseCues(attached)

	// Step 4: render.
	ctx := organise(req, final)

	return &Compiled{
		Context:        ctx,
		Selected:       req.RetrieveResp.Selected,
		RescueAttached: final,
		Skipped:        skipped,
	}
}

// filterRescue implements step 1: relevance gate, non-redundancy
// against already-kept subunits, and a budget cap. Operates in sorted
// (similarity desc) order so the most promising rescue lands first.
//
// When ``vecs`` is non-nil the dedup uses bge-m3 cosine (threshold
// ``maxRedundancyCosine`` 0.92); otherwise it falls back to Jaccard
// token overlap (threshold 0.85). Cosine catches paraphrases that
// share few tokens — "rename a python variable" vs "change a variable
// name in a python file" — that Jaccard misses.
func filterRescue(query string, rescue []retrieval.RescueRecord, budget int, frac float64, vecs map[uuid.UUID][]float32) []retrieval.RescueRecord {
	sorted := append([]retrieval.RescueRecord(nil), rescue...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Similarity > sorted[j].Similarity })
	maxChars := int(float64(budget) * frac * 4) // ~4 chars/token rough
	if maxChars < 100 {
		maxChars = 100
	}
	out := make([]retrieval.RescueRecord, 0, len(sorted))
	consumed := 0
	for _, r := range sorted {
		if r.Similarity < minRelevanceForRescue {
			break // sorted, so the rest are even lower
		}
		if len(r.Content) < minSubunitChars {
			continue
		}
		dup := false
		for _, kept := range out {
			if isRedundant(r, kept, vecs) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		if consumed+len(r.Content) > maxChars {
			break
		}
		consumed += len(r.Content)
		out = append(out, r)
	}
	return out
}

// isRedundant returns true when two rescue subunits are near
// duplicates. Prefers cosine when both vectors are available, else
// falls back to Jaccard token overlap.
func isRedundant(a, b retrieval.RescueRecord, vecs map[uuid.UUID][]float32) bool {
	if vecs != nil {
		va, oka := vecs[a.SubunitID]
		vb, okb := vecs[b.SubunitID]
		if oka && okb {
			return cosine(va, vb) >= maxRedundancyCosine
		}
	}
	return jaccardTokens(a.Content, b.Content) >= maxRedundancyJaccard
}

// attachRescue implements step 2 + computes the Aff(u, s | q) score
// for each (subunit, host-skill) pair via the 5-feature linear
// combination. Picks s*(u) = argmax_s Aff(u, s | q).
func attachRescue(query string, rescue []retrieval.RescueRecord,
	selected []retrieval.SelectedSkill, highlights map[uuid.UUID][]retrieval.Highlight,
	rsel map[uuid.UUID]float64, communityBy map[uuid.UUID]*uuid.UUID,
	bActive map[uuid.UUID]bool, w AffWeights, vecs map[uuid.UUID][]float32,
) []AttachedCue {
	out := make([]AttachedCue, 0, len(rescue))
	for _, r := range rescue {
		var bestSkill *retrieval.SelectedSkill
		bestAff := -1.0
		for i := range selected {
			s := &selected[i]
			feat := affFeatures(query, r, *s, highlights[s.SkillID], rsel[s.SkillID],
				communityBy[s.SkillID], communityBy[r.SourceSkillID], bActive[s.SkillID], vecs)
			score := dotAff(feat, w)
			if score > bestAff {
				bestAff = score
				bestSkill = s
			}
		}
		if bestSkill == nil {
			continue
		}
		out = append(out, AttachedCue{
			HostSkillID:   bestSkill.SkillID,
			SourceSkillID: r.SourceSkillID,
			Subunit:       r.Content,
			Kind:          r.Kind,
			AffScore:      bestAff,
		})
	}
	// Stable order: highest affinity first so the renderer surfaces
	// the strongest cues earlier in each skill's block.
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostSkillID != out[j].HostSkillID {
			return out[i].HostSkillID.String() < out[j].HostSkillID.String()
		}
		return out[i].AffScore > out[j].AffScore
	})
	return out
}

// affFeatures returns the 5-feature vector for one (u, s, q) tuple.
//
// q_rel: reuses retrieval.RescueRecord.Similarity directly -- that's
// already the bge-m3 cosine(u, q) the bottom-up step computed.
//
// f_skill: when vectors are available, max(cosine(u, h)) over h in
// the highlights for s -- "how well does this rescue subunit cohere
// with what we already picked for this skill". Falls back to Jaccard
// when vectors are missing (test fixtures, etc.).
//
// r_sel, g_same, g_active stay as scalar features; they don't
// benefit from vector cosine.
func affFeatures(query string, r retrieval.RescueRecord, s retrieval.SelectedSkill,
	hs []retrieval.Highlight, rsel float64,
	hostComm, sourceComm *uuid.UUID, active bool, vecs map[uuid.UUID][]float32,
) [5]float64 {
	qRel := r.Similarity
	if qRel < 0 {
		qRel = 0
	}

	fSkill := 0.0
	if vecs != nil {
		if vu, ok := vecs[r.SubunitID]; ok {
			for _, h := range hs {
				if vh, ok := vecs[h.SubunitID]; ok {
					if c := cosine(vu, vh); c > fSkill {
						fSkill = c
					}
				}
			}
		}
	}
	if fSkill == 0 {
		// Fallback (or vectors missing for some highlights).
		for _, h := range hs {
			if v := jaccardTokens(r.Content, h.Content); v > fSkill {
				fSkill = v
			}
		}
	}

	gSame := 0.0
	if hostComm != nil && sourceComm != nil && *hostComm == *sourceComm {
		gSame = 1.0
	}
	gActive := 0.0
	if active {
		gActive = 1.0
	}
	return [5]float64{qRel, fSkill, rsel, gSame, gActive}
}

// cosine returns the cosine similarity of two equally-sized vectors.
// bge-m3 outputs L2-normalised vectors so the divide step is mostly
// defensive — keeps us correct if a centroid or interpolated vector
// sneaks in.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 12; i++ {
		z = (z + x/z) / 2
	}
	return z
}

func dotAff(feat [5]float64, w AffWeights) float64 {
	return w.QRel*feat[0] + w.FSkill*feat[1] + w.RSel*feat[2] + w.GSame*feat[3] + w.GActive*feat[4]
}

// normalizedSelectionScores returns r_sel ∈ [0,1] per selected
// skill: max-normalize the .Score column so the highest-scoring
// selected skill gets r_sel = 1.0.
func normalizedSelectionScores(sel []retrieval.SelectedSkill) map[uuid.UUID]float64 {
	out := make(map[uuid.UUID]float64, len(sel))
	if len(sel) == 0 {
		return out
	}
	var hi float64
	for _, s := range sel {
		if s.Score > hi {
			hi = s.Score
		}
	}
	if hi <= 0 {
		for _, s := range sel {
			out[s.SkillID] = 0
		}
		return out
	}
	for _, s := range sel {
		out[s.SkillID] = s.Score / hi
	}
	return out
}

// finaliseCues implements step 3: drop generic / low-affinity cues.
// Threshold of 0.10 on Aff filters out borderline matches where none
// of the features fired strongly; the agent gets less but better.
func finaliseCues(in []AttachedCue) (kept []AttachedCue, skipped []SkippedCue) {
	for _, c := range in {
		if c.AffScore < 0.10 {
			skipped = append(skipped, SkippedCue{Reason: "low affinity"})
			continue
		}
		if len(c.Subunit) < minSubunitChars {
			skipped = append(skipped, SkippedCue{Reason: "too short"})
			continue
		}
		kept = append(kept, c)
	}
	return kept, skipped
}

// organise implements step 4: render the markdown context block.
// Tries to stay under BudgetTokens by skipping per-skill bodies once
// the cumulative byte count approaches budget*4 (~4 chars/token).
func organise(req Request, attached []AttachedCue) string {
	budgetChars := req.BudgetTokens * 4
	if budgetChars <= 0 {
		budgetChars = 6000
	}
	cuesBySkill := map[uuid.UUID][]AttachedCue{}
	for _, c := range attached {
		cuesBySkill[c.HostSkillID] = append(cuesBySkill[c.HostSkillID], c)
	}

	var b strings.Builder
	b.WriteString("# RELEVANT SKILLS\n\n")
	b.WriteString("The following skills look relevant to this task. Use them where they apply.\n")
	for _, s := range req.RetrieveResp.Selected {
		if b.Len() > budgetChars {
			break
		}
		b.WriteString("\n## ")
		b.WriteString(s.Name)
		if s.CommunityID != nil {
			// Render community label by lookup against the
			// diagnostics. For v1 we just emit the UUID prefix so the
			// block has visible community grouping without an extra
			// DB roundtrip.
			b.WriteString(" _[")
			b.WriteString(shortID(*s.CommunityID))
			b.WriteString("]_")
		}
		b.WriteString("\n\n")
		if s.Description != "" {
			b.WriteString(s.Description)
			b.WriteString("\n")
		}
		// Highlights
		if hs, ok := req.RetrieveResp.Highlights[s.SkillID]; ok && len(hs) > 0 {
			b.WriteString("\n**Highlights**\n")
			for _, h := range hs {
				b.WriteString("- ")
				b.WriteString(h.Content)
				b.WriteString("\n")
			}
		}
		// Related cues (rescued)
		if cs, ok := cuesBySkill[s.SkillID]; ok && len(cs) > 0 {
			b.WriteString("\n**Related cues**\n")
			for _, c := range cs {
				b.WriteString("- (from related skill) ")
				b.WriteString(c.Subunit)
				b.WriteString("\n")
			}
		}
	}
	if strings.TrimSpace(req.OutputContract) != "" {
		b.WriteString("\n## Output contract\n\n")
		b.WriteString(req.OutputContract)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n")
	return b.String()
}

func shortID(u uuid.UUID) string {
	s := u.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// jaccardTokens is token-overlap Jaccard reused from retrieval. We
// duplicate it here rather than import retrieval to avoid a cycle —
// the function is trivial.
func jaccardTokens(a, b string) float64 {
	aTok := tokensOf(a)
	bTok := tokensOf(b)
	if len(aTok) == 0 || len(bTok) == 0 {
		return 0
	}
	aSet := map[string]struct{}{}
	for _, t := range aTok {
		aSet[t] = struct{}{}
	}
	inter := 0
	bSet := map[string]struct{}{}
	for _, t := range bTok {
		bSet[t] = struct{}{}
		if _, ok := aSet[t]; ok {
			inter++
		}
	}
	union := len(aSet)
	for k := range bSet {
		if _, ok := aSet[k]; !ok {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokensOf(s string) []string {
	s = strings.ToLower(s)
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	out := parts[:0]
	for _, p := range parts {
		if len(p) >= 3 {
			out = append(out, p)
		}
	}
	return out
}

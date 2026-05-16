// Package retrieval implements SkillRAE §3.3.1: the online retrieval
// pipeline that combines top-down (community) and bottom-up (subunit)
// signals into a final skill ranking + evidence record.
//
// The pipeline runs in five steps:
//
//  1. Embed q via bge-m3.
//  2. Top-down: kNN over skill_communities -> B(q), the
//     community-supported skill set.
//  3. Bottom-up: kNN over skill_subunits, project to skills via the
//     extraction edges with degree normalization -> l0(s, q).
//  4. ℓ₁ via skill_description_embedding -> l1(s, q).
//  5. Combine via eq (3): score(s,q) = (α·l1 + β·l0 + γ·name) *
//     (1 + λ·𝟙[s ∈ B(q)]). TopK by score.
//
// Outputs K (selected skills), H (highlighted subunits per selected
// skill), and R (rescue records: non-selected source skills' subunits
// that matched q). These feed the compilation stage.
package retrieval

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/embeddings"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/reranker"
	"github.com/sirus20x6/adamaton-knowledge/skills-rae/store"
)

// Weights mirrors the paper's eq (3) coefficients. Defaults below
// match the values in the plan document; callers can override per-
// request.
type Weights struct {
	Alpha  float64 `json:"alpha"`  // ℓ₁ weight
	Beta   float64 `json:"beta"`   // ℓ₀ weight
	Gamma  float64 `json:"gamma"`  // name-score weight
	Lambda float64 `json:"lambda"` // community boost
}

// DefaultWeights returns sensible starting weights. Tune on the
// SkillsBench dev split in Phase G.
func DefaultWeights() Weights {
	return Weights{Alpha: 0.5, Beta: 0.3, Gamma: 0.2, Lambda: 0.4}
}

// Request is the public retrieval shape.
type Request struct {
	Query            string  `json:"query"`
	TopK             int     `json:"top_k,omitempty"`
	TopDownN         int     `json:"top_down_n,omitempty"`        // default 2 communities
	BottomUpN        int     `json:"bottom_up_n,omitempty"`       // default 50 subunits
	L1N              int     `json:"l1_n,omitempty"`              // default 50 description-knn
	Weights          Weights `json:"weights,omitempty"`
}

// SelectedSkill is one entry in K(q). Carries the raw signals so
// callers can audit ranking decisions.
type SelectedSkill struct {
	SkillID     uuid.UUID `json:"skill_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Score       float64   `json:"score"`
	L1          float64   `json:"l1"`
	L0          float64   `json:"l0"`
	NameScore   float64   `json:"name_score"`
	Boosted     bool      `json:"boosted"`
	CommunityID *uuid.UUID `json:"community_id,omitempty"`
}

// Highlight is one entry in H(s,q): a subunit that contributed to the
// skill's ranking, kept around for compilation as in-skill cue.
type Highlight struct {
	SubunitID  uuid.UUID `json:"subunit_id"`
	Content    string    `json:"content"`
	Kind       string    `json:"kind"`
	Similarity float64   `json:"similarity"`
}

// RescueRecord is one entry in R(q): a subunit from a NON-selected
// skill that hit q's bottom-up matches. Phase E's compilation will
// attach these to selected skills via Aff(u,s|q).
type RescueRecord struct {
	SubunitID     uuid.UUID `json:"subunit_id"`
	SourceSkillID uuid.UUID `json:"source_skill_id"`
	Content       string    `json:"content"`
	Kind          string    `json:"kind"`
	Similarity    float64   `json:"similarity"`
}

// Diagnostics records per-stage timings + the matched community list
// so callers (the debug view, the SkillsBench harness) can audit.
type Diagnostics struct {
	StageMs        map[string]int64 `json:"stage_ms"`
	TopDownLabels  []string         `json:"top_down_labels"`
	BottomUpHits   int              `json:"bottom_up_hits"`
	L1Hits         int              `json:"l1_hits"`
}

// Response is what the retriever returns to the route handler.
//
// QueryVector and SubunitVectors are populated when the retriever
// is constructed with WithVectorReturn(true) (the default for
// CompileContext callers). They let the compile package compute
// real bge-m3 cosines for Aff/dedup instead of Jaccard token
// overlap. Marked json:"-" so we don't ship 1024 floats per
// subunit over the wire to clients that just want skill IDs.
type Response struct {
	Selected    []SelectedSkill           `json:"selected_skills"`
	Highlights  map[uuid.UUID][]Highlight `json:"highlights"`
	Rescue      []RescueRecord            `json:"rescue_candidates"`
	Diagnostics Diagnostics               `json:"diagnostics"`

	QueryVector    []float32              `json:"-"`
	SubunitVectors map[uuid.UUID][]float32 `json:"-"`
}

// Retriever is the orchestration object. Construct once per process
// — it caches no state itself.
//
// ReturnVectors controls whether Retrieve fills QueryVector +
// SubunitVectors on the response. /v1/rae/compile sets this to true
// so the compile step gets real cosines for Aff; /v1/rae/search
// leaves it false so callers that only need the rankings don't pay
// for the extra round-trip to fetch vectors.
type Retriever struct {
	Pool          *pgxpool.Pool
	Embedder      *embeddings.Client
	Reranker      *reranker.Client // optional; when Enabled() rescores bottom-up hits
	ReturnVectors bool
}

// Retrieve runs the full pipeline and returns selected skills +
// evidence + rescue records. Compilation (Phase E) consumes this.
func (r *Retriever) Retrieve(ctx context.Context, req Request) (*Response, error) {
	if r.Pool == nil {
		return nil, errors.New("retrieval: nil pool")
	}
	if r.Embedder == nil || !r.Embedder.Enabled() {
		return nil, errors.New("retrieval: embedder not configured")
	}
	if strings.TrimSpace(req.Query) == "" {
		return nil, errors.New("retrieval: empty query")
	}
	w := req.Weights
	if (w == Weights{}) {
		w = DefaultWeights()
	}
	topK := req.TopK
	if topK <= 0 {
		topK = 5
	}
	topDownN := req.TopDownN
	if topDownN <= 0 {
		topDownN = 2
	}
	bottomUpN := req.BottomUpN
	if bottomUpN <= 0 {
		bottomUpN = 50
	}
	l1N := req.L1N
	if l1N <= 0 {
		l1N = 50
	}

	stage := map[string]int64{}
	t0 := time.Now()

	// Step 1: embed query.
	vec, err := r.Embedder.EmbedOne(ctx, req.Query)
	if err != nil {
		return nil, err
	}
	stage["embed_q"] = time.Since(t0).Milliseconds()

	// Step 2-4: run the three retrievals in sequence. Could be
	// parallel via goroutines but the SQL hits one Postgres at our
	// QPS — serial keeps the connection-pool footprint flat.
	t := time.Now()
	comms, err := store.TopDownCommunities(ctx, r.Pool, vec, topDownN)
	if err != nil {
		return nil, err
	}
	stage["topdown"] = time.Since(t).Milliseconds()

	t = time.Now()
	subs, err := store.BottomUpSubunits(ctx, r.Pool, vec, bottomUpN)
	if err != nil {
		return nil, err
	}
	stage["bottomup"] = time.Since(t).Milliseconds()

	// Optional stage-2 rerank. Bottom-up returns ~50 (subunit, skill)
	// rows ordered by bi-encoder cosine; the cross-encoder rescores
	// (query, subunit_content) pairs and we replace the Similarity
	// field so projection-to-skills (eq 2) and the downstream Aff
	// q_rel feature both see the calibrated score.
	//
	// Important: the same subunit may appear multiple times in
	// ``subs`` (once per (subunit, skill) edge). We rerank each
	// DISTINCT subunit content once, then fan the score back out to
	// every row that shared that content. Otherwise we'd pay the
	// cross-encoder cost per edge.
	if r.Reranker != nil && r.Reranker.Enabled() && len(subs) > 1 {
		t = time.Now()
		rerankCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		uniqContents := make([]string, 0, len(subs))
		contentIdx := make(map[string]int, len(subs))
		for _, s := range subs {
			if _, ok := contentIdx[s.Content]; ok {
				continue
			}
			contentIdx[s.Content] = len(uniqContents)
			uniqContents = append(uniqContents, s.Content)
		}
		ranked, rerankErr := r.Reranker.Rerank(rerankCtx, req.Query, uniqContents, 0)
		cancel()
		if rerankErr != nil && !errors.Is(rerankErr, reranker.ErrDisabled) {
			// Best-effort: a wedged sidecar shouldn't kill the pipeline.
			// We just keep the bi-encoder ordering.
			stage["rerank_skipped"] = 1
		} else if len(ranked) > 0 {
			contentScore := make(map[string]float64, len(ranked))
			for _, rh := range ranked {
				if rh.Index < 0 || rh.Index >= len(uniqContents) {
					continue
				}
				contentScore[uniqContents[rh.Index]] = rh.Score
			}
			for i := range subs {
				if v, ok := contentScore[subs[i].Content]; ok {
					subs[i].Similarity = v
				}
			}
			// Re-sort by the new score so callers that rely on
			// "first occurrence is the best evidence" still get
			// stable behaviour.
			sort.SliceStable(subs, func(i, j int) bool { return subs[i].Similarity > subs[j].Similarity })
		}
		stage["rerank"] = time.Since(t).Milliseconds()
	}

	t = time.Now()
	l1s, err := store.SkillL1(ctx, r.Pool, vec, l1N)
	if err != nil {
		return nil, err
	}
	stage["l1"] = time.Since(t).Milliseconds()

	// Step 5: combine. Build a skill candidate set from the union of
	// (l1 hits) ∪ (bottom-up source skills) ∪ (B(q) members) and
	// score each.
	t = time.Now()
	bSet := map[uuid.UUID]struct{}{}
	for _, c := range comms {
		for _, sid := range c.Members {
			bSet[sid] = struct{}{}
		}
	}
	l0Raw := projectSubunitsToSkills(subs)
	l0Norm := minMaxNormalize(l0Raw)
	candidates := unionCandidates(l1s, l0Norm, bSet)
	scored := make([]SelectedSkill, 0, len(candidates))
	for _, c := range candidates {
		ns := nameScore(req.Query, c.Name)
		l0 := l0Norm[c.SkillID]
		_, boosted := bSet[c.SkillID]
		raw := w.Alpha*c.L1 + w.Beta*l0 + w.Gamma*ns
		boost := 1.0
		if boosted {
			boost = 1.0 + w.Lambda
		}
		score := raw * boost
		scored = append(scored, SelectedSkill{
			SkillID:     c.SkillID,
			Name:        c.Name,
			Description: c.Description,
			Score:       score,
			L1:          c.L1,
			L0:          l0,
			NameScore:   ns,
			Boosted:     boosted,
			CommunityID: c.CommunityID,
		})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].SkillID.String() < scored[j].SkillID.String()
	})
	if len(scored) > topK {
		scored = scored[:topK]
	}
	stage["score"] = time.Since(t).Milliseconds()

	// Evidence: highlights per selected skill + rescue records.
	t = time.Now()
	selectedSet := map[uuid.UUID]struct{}{}
	for _, s := range scored {
		selectedSet[s.SkillID] = struct{}{}
	}
	highlights := map[uuid.UUID][]Highlight{}
	var rescue []RescueRecord
	for _, sh := range subs {
		if _, ok := selectedSet[sh.SkillID]; ok {
			highlights[sh.SkillID] = append(highlights[sh.SkillID], Highlight{
				SubunitID:  sh.ID,
				Content:    sh.Content,
				Kind:       sh.Kind,
				Similarity: sh.Similarity,
			})
		} else {
			rescue = append(rescue, RescueRecord{
				SubunitID:     sh.ID,
				SourceSkillID: sh.SkillID,
				Content:       sh.Content,
				Kind:          sh.Kind,
				Similarity:    sh.Similarity,
			})
		}
	}
	// Cap highlights per skill to the most informative ones.
	for sid, hs := range highlights {
		if len(hs) > 5 {
			highlights[sid] = hs[:5]
		}
	}
	stage["evidence"] = time.Since(t).Milliseconds()

	labels := make([]string, len(comms))
	for i, c := range comms {
		labels[i] = c.Label
	}

	resp := &Response{
		Selected:   scored,
		Highlights: highlights,
		Rescue:     rescue,
		Diagnostics: Diagnostics{
			StageMs:       stage,
			TopDownLabels: labels,
			BottomUpHits:  len(subs),
			L1Hits:        len(l1s),
		},
	}

	// Optionally hydrate the vectors compile.Compile uses for real
	// cosine in Aff/dedup. One SQL round-trip for ~20-30 subunit
	// rows; negligible vs the embedder call.
	if r.ReturnVectors {
		t := time.Now()
		ids := make([]uuid.UUID, 0, len(highlights)*5+len(rescue))
		seen := map[uuid.UUID]struct{}{}
		add := func(id uuid.UUID) {
			if _, ok := seen[id]; ok {
				return
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		for _, hs := range highlights {
			for _, h := range hs {
				add(h.SubunitID)
			}
		}
		for _, rr := range rescue {
			add(rr.SubunitID)
		}
		if len(ids) > 0 {
			vecs, err := store.LoadSubunitVectors(ctx, r.Pool, ids)
			if err != nil {
				return nil, err
			}
			resp.SubunitVectors = vecs
		} else {
			resp.SubunitVectors = map[uuid.UUID][]float32{}
		}
		resp.QueryVector = vec
		stage["fetch_vectors"] = time.Since(t).Milliseconds()
	}

	return resp, nil
}

// candidateSkill is the per-skill scratch record used while
// merging l1 + bottom-up + B(q).
type candidateSkill struct {
	SkillID     uuid.UUID
	Name        string
	Description string
	L1          float64
	CommunityID *uuid.UUID
}

// unionCandidates merges every skill that appears in any retrieval
// stream. l1 hits carry their own L1 score; bottom-up-only hits get
// L1=0; B(q)-only members hit the bSet check below but only enter
// the candidate set if they were also surfaced by l1 or bottomup --
// otherwise B(q) would inject the whole community into K(q).
//
// (We do NOT pull names for bottom-up-only skills here; the
// retrieval-time hot path can't afford an extra pool round-trip per
// skill. Phase D's smoke harness ensures the corpus is fully indexed
// so every skill has a description_embedding and thus a name in l1.)
func unionCandidates(l1s []store.SkillL1Hit, l0 map[uuid.UUID]float64, bSet map[uuid.UUID]struct{}) []candidateSkill {
	out := make(map[uuid.UUID]*candidateSkill, len(l1s))
	for i := range l1s {
		h := l1s[i]
		out[h.ID] = &candidateSkill{
			SkillID:     h.ID,
			Name:        h.Name,
			Description: h.Description,
			L1:          h.L1,
			CommunityID: h.CommunityID,
		}
	}
	for sid := range l0 {
		if _, ok := out[sid]; ok {
			continue
		}
		// Skill has bottom-up evidence but didn't make the l1 top-N.
		// Skip — we have no name/description to render. The l0
		// signal is still preserved as l0Norm[sid] and the rescue
		// path will catch its subunits in R(q).
		_ = bSet
	}
	res := make([]candidateSkill, 0, len(out))
	for _, c := range out {
		res = append(res, *c)
	}
	return res
}

// projectSubunitsToSkills implements eq (2):
//
//	ℓ₀(s,q) raw = Σ_{u ∈ TopK(q)} σ(q,u) / deg(u)
//
// The deg(u) division is the key bit — a subunit that's shared
// across many skills (high deg) contributes proportionally less to
// each, preventing a single generic subunit from inflating every
// skill it links to.
func projectSubunitsToSkills(hits []store.SubunitHit) map[uuid.UUID]float64 {
	out := map[uuid.UUID]float64{}
	for _, h := range hits {
		deg := h.Degree
		if deg < 1 {
			deg = 1
		}
		out[h.SkillID] += h.Similarity / float64(deg)
	}
	return out
}

// minMaxNormalize rescales values to [0,1]. We use min-max rather
// than z-score because the downstream score combines linearly with
// l1 (already in [0,1]) and gamma·name_score (also bounded). All
// signals on the same scale keep weight tuning interpretable.
func minMaxNormalize(in map[uuid.UUID]float64) map[uuid.UUID]float64 {
	if len(in) == 0 {
		return in
	}
	var lo, hi float64
	first := true
	for _, v := range in {
		if first || v < lo {
			lo = v
		}
		if first || v > hi {
			hi = v
		}
		first = false
	}
	span := hi - lo
	out := make(map[uuid.UUID]float64, len(in))
	if span == 0 {
		for k := range in {
			out[k] = 1.0
		}
		return out
	}
	for k, v := range in {
		out[k] = (v - lo) / span
	}
	return out
}

// nameScore returns a token-overlap Jaccard between the query and
// the skill name. Cheap, no embedder call, and surfaces the obvious
// "rename" -> "rename-variable" matches.
func nameScore(query, name string) float64 {
	q := tokens(query)
	n := tokens(name)
	if len(q) == 0 || len(n) == 0 {
		return 0
	}
	inter := 0
	qSet := map[string]struct{}{}
	for _, t := range q {
		qSet[t] = struct{}{}
	}
	for _, t := range n {
		if _, ok := qSet[t]; ok {
			inter++
		}
	}
	union := len(qSet)
	for _, t := range n {
		if _, ok := qSet[t]; !ok {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func tokens(s string) []string {
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

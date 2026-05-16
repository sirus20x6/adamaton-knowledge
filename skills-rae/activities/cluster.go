package activities

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/clusterclient"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/internal/embeddings"
	"github.com/sirus20x6/adamomaton-knowledge/skills-rae/store"
)

// ClusterDeps wraps the pool, the bge-m3 embedder, and the cluster-
// sidecar client. The cluster step is read-heavy (load every skill +
// their top-IDF subunits), embedding-heavy (one bge-m3 call per
// skill_text + one per community representative_text), and now also
// CPU-heavy on the sidecar side (UMAP + HDBSCAN). All three clients
// are shared across the activity.
type ClusterDeps struct {
	Pool          *pgxpool.Pool
	Embedder      *embeddings.Client
	ClusterClient *clusterclient.Client
}

// RecomputeCommunitiesInput holds tuning knobs. The K-means-specific
// fields (K, MaxIter, Seed) are gone — all clustering params now live
// on the sidecar side as env vars (HDBSCAN_MIN_CLUSTER_SIZE etc.).
type RecomputeCommunitiesInput struct {
	TopSubunitsPerSkill int `json:"top_subunits_per_skill"` // default 5
}

// RecomputeCommunitiesResult records what landed.
type RecomputeCommunitiesResult struct {
	CommunitiesCreated int    `json:"communities_created"`
	SkillsAssigned     int    `json:"skills_assigned"`
	SkillsSkipped      int    `json:"skills_skipped"`
	SkillsNoise        int    `json:"skills_noise"`
	TreeID             string `json:"tree_id"`
}

// HDBSCANCluster runs the offline community rebuild end-to-end:
//
//  1. Recompute corpus-wide IDF on subunits.
//  2. Build a skill_text for each skill: name + description + top-K
//     IDF-ranked subunits. Embed via bge-m3, stamp skill_embedding.
//  3. POST the embeddings to the cluster-sidecar /v1/cluster endpoint
//     (UMAP -> HDBSCAN -> 3-level hierarchy).
//  4. Build per-community representative_text from the indices the
//     sidecar returned, embed each via bge-m3, build CommunityRow.
//  5. Atomically ReplaceCommunities + stamp community_id + community_noise
//     on every skill in one transaction.
//
// Failures are tolerated per-skill: a skill with no extractable subunits
// gets skipped (counted in SkillsSkipped). We only abort if RecomputeIDF
// fails, the sidecar is unreachable, or the final transaction errors.
func (d *ClusterDeps) HDBSCANCluster(ctx context.Context, in RecomputeCommunitiesInput) (*RecomputeCommunitiesResult, error) {
	if d.ClusterClient == nil || !d.ClusterClient.Enabled() {
		return nil, fmt.Errorf("HDBSCANCluster: cluster-sidecar client not configured")
	}
	topK := in.TopSubunitsPerSkill
	if topK <= 0 {
		topK = 5
	}

	if err := store.RecomputeIDF(ctx, d.Pool); err != nil {
		return nil, fmt.Errorf("idf: %w", err)
	}

	skills, err := store.ListSkillsForClustering(ctx, d.Pool)
	if err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	if len(skills) == 0 {
		return &RecomputeCommunitiesResult{}, nil
	}

	// Build skill_text per skill.
	skillTexts := make([]string, 0, len(skills))
	skillIndex := make([]int, 0, len(skills)) // map embedding idx -> skills idx
	for i := range skills {
		subs, err := store.LoadSubunitsForSkill(ctx, d.Pool, skills[i].ID)
		if err != nil {
			continue
		}
		text := buildSkillText(skills[i], subs, topK)
		if text == "" {
			continue
		}
		skills[i].SkillText = text
		skillTexts = append(skillTexts, text)
		skillIndex = append(skillIndex, i)
	}
	if len(skillTexts) == 0 {
		return &RecomputeCommunitiesResult{SkillsSkipped: len(skills)}, nil
	}

	// Embed skill_texts in batches of 64.
	const batch = 64
	vectors := make([][]float32, 0, len(skillTexts))
	for i := 0; i < len(skillTexts); i += batch {
		end := i + batch
		if end > len(skillTexts) {
			end = len(skillTexts)
		}
		chunk, err := d.Embedder.Embed(ctx, skillTexts[i:end])
		if err != nil {
			return nil, fmt.Errorf("embed skill_texts: %w", err)
		}
		vectors = append(vectors, chunk...)
	}

	// Stamp skill_text + skill_embedding for each indexable skill.
	for vi, si := range skillIndex {
		skills[si].Embedding = vectors[vi]
		if err := store.StampSkillEmbedding(ctx, d.Pool, skills[si].ID, skills[si].SkillText, vectors[vi]); err != nil {
			return nil, fmt.Errorf("stamp skill_embedding: %w", err)
		}
	}

	// Build the sidecar request. skill_ids must align with the vectors
	// array so the sidecar's returned member_skill_ids point back at
	// the correct rows.
	clusterSkillIDs := make([]string, len(vectors))
	for vi, si := range skillIndex {
		clusterSkillIDs[vi] = skills[si].ID.String()
	}
	resp, err := d.ClusterClient.Cluster(ctx, clusterclient.ClusterRequest{
		Vectors:  vectors,
		SkillIDs: clusterSkillIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("cluster sidecar: %w", err)
	}
	if len(resp.Communities) == 0 {
		return &RecomputeCommunitiesResult{
			SkillsSkipped: len(skills) - len(skillIndex),
			SkillsNoise:   len(resp.NoiseSkillIDs),
			TreeID:        resp.TreeID,
		}, nil
	}

	treeID, err := uuid.Parse(resp.TreeID)
	if err != nil {
		return nil, fmt.Errorf("parse tree_id %q: %w", resp.TreeID, err)
	}

	// Map sidecar TempID -> server-assigned UUID. The sidecar already
	// guarantees TempIDs are dense (0..N-1) but we don't rely on that.
	tempToUUID := make(map[int]uuid.UUID, len(resp.Communities))
	for _, c := range resp.Communities {
		tempToUUID[c.TempID] = uuid.New()
	}

	// Build representative_text per community from the
	// representative_indices the sidecar returned, then bge-m3-embed
	// them in one call.
	repTexts := make([]string, 0, len(resp.Communities))
	repCommunityIdx := make([]int, 0, len(resp.Communities))
	for ci, c := range resp.Communities {
		parts := make([]string, 0, len(c.RepresentativeIndices))
		for _, idx := range c.RepresentativeIndices {
			if idx < 0 || idx >= len(skillTexts) {
				continue
			}
			parts = append(parts, skillTexts[idx])
		}
		text := strings.Join(parts, "\n\n")
		if text == "" {
			text = "community"
		}
		repTexts = append(repTexts, text)
		repCommunityIdx = append(repCommunityIdx, ci)
	}
	repVecs, err := d.Embedder.Embed(ctx, repTexts)
	if err != nil {
		return nil, fmt.Errorf("embed representative texts: %w", err)
	}

	// Assemble CommunityRow values.
	rows := make([]store.CommunityRow, 0, len(resp.Communities))
	for k, ci := range repCommunityIdx {
		c := resp.Communities[ci]
		var parent *uuid.UUID
		if c.ParentTempID != nil {
			if p, ok := tempToUUID[*c.ParentTempID]; ok {
				parent = &p
			}
		}
		rows = append(rows, store.CommunityRow{
			ID:                 tempToUUID[c.TempID],
			ParentID:           parent,
			Level:              c.Level,
			TreeID:             treeID,
			Label:              deriveLabel(repTexts[k]),
			RepresentativeText: repTexts[k],
			Embedding:          repVecs[k],
			MemberCount:        len(c.MemberSkillIDs),
		})
	}

	// Leaf-level assignments: every skill that lands in a leaf
	// community (highest level == maxLevel) gets community_id set to
	// that leaf's UUID. Noise skills get NULL + community_noise=TRUE.
	maxLevel := 0
	for _, c := range resp.Communities {
		if c.Level > maxLevel {
			maxLevel = c.Level
		}
	}
	assignments := make(map[uuid.UUID]uuid.UUID, len(clusterSkillIDs))
	for _, c := range resp.Communities {
		if c.Level != maxLevel {
			continue
		}
		commUUID := tempToUUID[c.TempID]
		for _, sidStr := range c.MemberSkillIDs {
			sid, err := uuid.Parse(sidStr)
			if err != nil {
				continue
			}
			assignments[sid] = commUUID
		}
	}

	noiseIDs := make([]uuid.UUID, 0, len(resp.NoiseSkillIDs))
	for _, sidStr := range resp.NoiseSkillIDs {
		sid, err := uuid.Parse(sidStr)
		if err != nil {
			continue
		}
		noiseIDs = append(noiseIDs, sid)
	}

	if err := store.ReplaceCommunities(ctx, d.Pool, rows, assignments, noiseIDs); err != nil {
		return nil, fmt.Errorf("replace communities: %w", err)
	}

	return &RecomputeCommunitiesResult{
		CommunitiesCreated: len(rows),
		SkillsAssigned:     len(assignments),
		SkillsSkipped:      len(skills) - len(skillIndex),
		SkillsNoise:        len(noiseIDs),
		TreeID:             resp.TreeID,
	}, nil
}

// buildSkillText concatenates the skill name + description + top-IDF
// subunits into the corpus text used for community clustering.
// Caps total length to keep the embedder happy; the embedder will
// truncate anyway but keeping it under 2k chars avoids waste.
func buildSkillText(skill store.SkillForClustering, subs []store.SubunitWithEdges, topK int) string {
	var b strings.Builder
	b.WriteString(skill.Name)
	b.WriteString(". ")
	b.WriteString(skill.Description)
	for i, s := range subs {
		if i >= topK {
			break
		}
		b.WriteString("\n- ")
		b.WriteString(s.Content)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 2048 {
		out = out[:2048]
	}
	return out
}

// deriveLabel picks a short human-readable handle for the community.
// Naive: first significant word of representative_text. Replaceable
// by a noun-phrase extractor later — community labels don't affect
// retrieval, only the debug UI.
func deriveLabel(repText string) string {
	first := strings.Fields(repText)
	for _, w := range first {
		w = strings.TrimFunc(w, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
		})
		if len(w) >= 4 {
			return w
		}
	}
	if len(first) > 0 {
		return first[0]
	}
	return "community"
}

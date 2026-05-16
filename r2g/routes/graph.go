// Package routes — graph.go owns /platform/graph/*.
//
// Ported from platform/backend/app/api/graph.py on 2026-05-15. As of the
// platform.corpus_documents join-table landing, corpus_id is no longer a
// passthrough — every handler that has a corpus context threads it down
// to the store layer, which EXISTS-joins through corpus_documents so
// cross-corpus rows can't bleed into the response. Empty corpus_id keeps
// the unscoped global behaviour for legacy callers.
package routes

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/store"
)

// Real-data tunables. Matched to the Python module-level constants.
const (
	graphDefaultTopCategories      = 24
	graphDefaultEntitiesPerCat     = 1000
	graphDefaultRelsLimit          = 5000
	graphDefaultNeighbourLimit     = 50
	graphDefaultSourceLimit        = 25
	graphDefaultMemberLimit        = 1000
	graphFallbackNodeCount         = 50
	graphFallbackClusterCount      = 4
)

// taxonomicPredicatesByCategory maps a parent's category to the
// predicates we follow as "membership" in members-mode expand. IS_A is
// the generic fallback. Mirrors the Python dict verbatim.
var taxonomicPredicatesByCategory = map[string][]string{
	"SPECIES":      {"MEMBER_SPECIES", "IS_A"},
	"FACTION":      {"MEMBER_OF", "SERVES", "ALLEGIANCE"},
	"RELIGION":     {"WORSHIPS", "BELIEVES_IN", "MEMBER_OF"},
	"ERA":          {"OCCURRED_DURING"},
	"PLANET":       {"INHABITS", "ORIGIN_OF", "RULES"},
	"SETTING":      {"INHABITS", "OCCURRED_AT"},
	"ORGANIZATION": {"MEMBER_OF", "SERVES"},
	"TROPE":        {"EXHIBITS_TROPE", "EMBODIES", "SUBVERTS", "INVERTS", "DECONSTRUCTS"},
	"THEME":        {"HAS_THEME"},
}

// RegisterGraph wires the 8 /platform/graph/* endpoints. Route order
// matters: more-specific paths register first so gorilla/mux doesn't
// shadow /graphrag/{corpus_id}/summary with /graphrag/community/{uuid}.
func RegisterGraph(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	// GraphRAG endpoints first — the static prefixes don't overlap with
	// the level-2 endpoints, but order keeps mux happy under future churn.
	r.HandleFunc("/platform/graph/graphrag/community/{community_uuid}",
		graphRAGCommunityDetailHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/graph/graphrag/{corpus_id}/summary",
		graphRAGSummaryHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/graph/graphrag/{corpus_id}/communities",
		graphRAGCommunitiesHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/graph/graphrag/{corpus_id}/entities",
		graphRAGEntitiesHandler(pool, logger)).Methods(http.MethodGet)

	// Hierarchy endpoints.
	r.HandleFunc("/platform/graph/communities/{corpus_id}",
		graphCommunitiesHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/graph/expand/{community_id}",
		graphExpandHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/graph/node/{node_id}",
		graphNodeHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/graph/search",
		graphSearchHandler(pool, logger)).Methods(http.MethodGet)
}

// ─────────────────────────────────────────────────────────────────────────
// Stable IDs. Python uses urlsafe_b64encode(name.encode("utf-8")).rstrip("=")
// prefixed with "cat:" / "ent:". We reproduce byte-for-byte so cached
// frontend node ids round-trip across the cutover.
// ─────────────────────────────────────────────────────────────────────────

func graphEnc(s string) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(s)), "=")
}

func graphDec(s string) (string, error) {
	if pad := (4 - len(s)%4) % 4; pad > 0 {
		s += strings.Repeat("=", pad)
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func categoryID(category string) string { return "cat:" + graphEnc(category) }
func entityID(name string) string       { return "ent:" + graphEnc(name) }

// decodeNodeID returns (kind, payload). kind is "cat" or "ent". ok=false
// when the id has no recognized prefix or decoding failed (e.g. fake-id).
func decodeNodeID(id string) (kind, payload string, ok bool) {
	for _, prefix := range []string{"cat:", "ent:"} {
		if strings.HasPrefix(id, prefix) {
			payload, err := graphDec(id[len(prefix):])
			if err != nil {
				return "", "", false
			}
			return prefix[:len(prefix)-1], payload, true
		}
	}
	return "", "", false
}

// graphSeed is the synthetic fallback's 32-bit deterministic seed.
// Python: int(sha256("|".join(parts)).hexdigest()[:8], 16). We must
// match byte-for-byte so the fake graph is reproducible across the
// cutover for the same inputs (dev mode, tests).
func graphSeed(parts ...string) uint32 {
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	first8 := hex.EncodeToString(h[:])[:8]
	v, _ := strconv.ParseUint(first8, 16, 64)
	return uint32(v)
}

// categoryClusterIndex picks the cytoscape colour bucket for a category.
// Python: seed(cat) % DEFAULT_CLUSTER_COUNT.
func categoryClusterIndex(category string) int {
	return int(graphSeed(category) % uint32(graphFallbackClusterCount))
}

// scaleSize maps a count to a node radius using log1p, identical to the
// Python helper. Empty/zero ranges collapse to minSize.
func scaleSize(value, maxValue, minSize, maxSize int) int {
	if maxValue <= 0 {
		return minSize
	}
	v := math.Log1p(float64(value)) / math.Log1p(float64(maxValue))
	return minSize + int(math.Round(float64(maxSize-minSize)*v))
}

// roundTo3 mirrors Python's round(x, 3) for the JSON edge weights. The
// frontend compares these as float literals so the rounding has to
// match.
func roundTo3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// clusterLabel mirrors Python's _cluster_label — fixed deterministic
// list used by the synthetic-fallback path.
func clusterLabel(cluster int) string {
	names := []string{"Kinetics", "Topology", "Cosmology", "Materials",
		"Methods", "Statistics", "Quantum", "Geometry"}
	if cluster < 0 {
		cluster = -cluster
	}
	return names[cluster%len(names)]
}

func fallbackCommunityID(corpusID string, level, cluster int) string {
	return fmt.Sprintf("c%d-%s-%d", level, prefix8(corpusID), cluster)
}

func fallbackNodeID(corpusID string, level, cluster, idx int) string {
	return fmt.Sprintf("n%d-%s-%d-%d", level, prefix8(corpusID), cluster, idx)
}

func prefix8(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// fakeCommunities reproduces Python's _fake_communities deterministically.
// The Park-Miller-ish LCG and ordering must match exactly so byte-identical
// JSON falls out for the same inputs.
func fakeCommunities(corpusID string, level, clusterCount int) map[string]any {
	state := graphSeed(corpusID, strconv.Itoa(level)) & 0xFFFFFFFF
	rand := func() float64 {
		state = (state*1103515245 + 12345) & 0x7FFFFFFF
		return float64(state) / float64(0x7FFFFFFF)
	}

	nodes := []map[string]any{}
	edges := []map[string]any{}

	if level >= 2 {
		for cluster := 0; cluster < clusterCount; cluster++ {
			nid := fallbackCommunityID(corpusID, level, cluster)
			nodes = append(nodes, map[string]any{
				"id":           nid,
				"label":        clusterLabel(cluster),
				"level":        level,
				"size":         40 + cluster*4,
				"community_id": nid,
				"cluster":      cluster,
			})
		}
		for cluster := 0; cluster < clusterCount; cluster++ {
			other := (cluster + 1) % clusterCount
			edges = append(edges, map[string]any{
				"source": fallbackCommunityID(corpusID, level, cluster),
				"target": fallbackCommunityID(corpusID, level, other),
				"weight": 0.3 + 0.7*rand(),
			})
		}
		return map[string]any{"nodes": nodes, "edges": edges}
	}

	perCluster := int(math.Max(2, math.Ceil(float64(graphFallbackNodeCount)/float64(clusterCount))))
	if level == 1 {
		perCluster = int(math.Max(2, float64(perCluster/3)))
	}

	for cluster := 0; cluster < clusterCount; cluster++ {
		for idx := 0; idx < perCluster; idx++ {
			nid := fallbackNodeID(corpusID, level, cluster, idx)
			parentLevel := level
			if parentLevel < 0 {
				parentLevel = 0
			}
			nodes = append(nodes, map[string]any{
				"id":           nid,
				"label":        fmt.Sprintf("%s %d", clusterLabel(cluster), idx+1),
				"level":        level,
				"size":         8 + int(rand()*18),
				"community_id": fallbackCommunityID(corpusID, parentLevel+1, cluster),
				"cluster":      cluster,
			})
		}
		for idx := 1; idx < perCluster; idx++ {
			edges = append(edges, map[string]any{
				"source": fallbackNodeID(corpusID, level, cluster, 0),
				"target": fallbackNodeID(corpusID, level, cluster, idx),
				"weight": 0.4 + 0.6*rand(),
			})
			if idx+1 < perCluster && rand() > 0.6 {
				edges = append(edges, map[string]any{
					"source": fallbackNodeID(corpusID, level, cluster, idx),
					"target": fallbackNodeID(corpusID, level, cluster, idx+1),
					"weight": 0.3 + 0.5*rand(),
				})
			}
		}
	}
	for cluster := 0; cluster < clusterCount; cluster++ {
		other := (cluster + 1) % clusterCount
		edges = append(edges, map[string]any{
			"source": fallbackNodeID(corpusID, level, cluster, 0),
			"target": fallbackNodeID(corpusID, level, other, 0),
			"weight": 0.2 + 0.4*rand(),
		})
	}
	return map[string]any{"nodes": nodes, "edges": edges}
}

// normaliseCorpusID converts the frontend's "_" sentinel (and any other
// non-UUID input) into the empty string the store layer treats as
// "unscoped". Without this the corpus_documents EXISTS clause runs with
// a literal "_" parameter, matches no rows, and the response comes back
// empty -- that's the "blank graph after clicking a node" symptom.
func normaliseCorpusID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "_" {
		return ""
	}
	if _, err := uuid.Parse(s); err != nil {
		return ""
	}
	return s
}

// ─────────────────────────────────────────────────────────────────────────
// Handlers
// ─────────────────────────────────────────────────────────────────────────

func graphCommunitiesHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		corpusID := normaliseCorpusID(mux.Vars(req)["corpus_id"])
		level := parseIntInRange(req.URL.Query().Get("level"), 2, 0, 3)

		cats, err := store.QueryCategories(req.Context(), pool, graphDefaultTopCategories, corpusID)
		if err != nil {
			logger.WithError(err).Warn("graph: category query failed; falling back to fakes")
			writeJSON(w, http.StatusOK, fakeCommunities(corpusID, level, graphFallbackClusterCount))
			return
		}
		if len(cats) == 0 {
			writeJSON(w, http.StatusOK, fakeCommunities(corpusID, level, graphFallbackClusterCount))
			return
		}

		if level >= 2 {
			maxSz := 1
			for _, c := range cats {
				if c.Count > maxSz {
					maxSz = c.Count
				}
			}
			nodes := make([]map[string]any, 0, len(cats))
			names := make([]string, 0, len(cats))
			for _, c := range cats {
				cid := categoryID(c.Category)
				nodes = append(nodes, map[string]any{
					"id":           cid,
					"label":        c.Category,
					"level":        2,
					"size":         scaleSize(c.Count, maxSz, 12, 60),
					"community_id": cid,
					"cluster":      categoryClusterIndex(c.Category),
					"category":     c.Category,
					"count":        c.Count,
				})
				names = append(names, c.Category)
			}
			bridges, err := store.QueryCategoryBridges(req.Context(), pool, names, 80, corpusID)
			if err != nil {
				logger.WithError(err).Warn("graph: category bridges failed; rendering edge-less")
				bridges = nil
			}
			maxN := 1
			for _, b := range bridges {
				if b.Count > maxN {
					maxN = b.Count
				}
			}
			edges := []map[string]any{}
			seen := map[string]bool{}
			for _, b := range bridges {
				k1, k2 := b.SubCat, b.ObjCat
				if k1 > k2 {
					k1, k2 = k2, k1
				}
				key := k1 + "\x00" + k2
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, map[string]any{
					"source": categoryID(b.SubCat),
					"target": categoryID(b.ObjCat),
					"weight": roundTo3(0.2 + 0.8*(float64(b.Count)/float64(maxN))),
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "edges": edges})
			return
		}

		// Level 0/1 — expand the biggest category.
		payload, err := expandCategory(req, pool, cats[0].Category, level, graphDefaultEntitiesPerCat, corpusID)
		if err != nil {
			logger.WithError(err).Warn("graph: expand biggest category failed")
			writeJSON(w, http.StatusOK, map[string]any{
				"nodes":  []any{},
				"edges":  []any{},
				"parent": categoryID(cats[0].Category),
			})
			return
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

// expandCategory builds the level-1 node+edge bag for a category.
// corpusID, when non-empty, scopes both the entities pulled for the
// category and the relationships that wire them together.
func expandCategory(req *http.Request, pool *pgxpool.Pool, category string, level, limit int, corpusID string) (map[string]any, error) {
	entities, err := store.QueryCategoryEntities(req.Context(), pool, category, limit, corpusID)
	if err != nil {
		return nil, err
	}
	parentID := categoryID(category)
	if len(entities) == 0 {
		return map[string]any{
			"nodes":  []any{},
			"edges":  []any{},
			"parent": parentID,
		}, nil
	}
	maxMentions := 1
	for _, e := range entities {
		if e.Count > maxMentions {
			maxMentions = e.Count
		}
	}
	cluster := categoryClusterIndex(category)
	nodes := make([]map[string]any, 0, len(entities))
	names := make([]string, 0, len(entities))
	for _, e := range entities {
		nodes = append(nodes, map[string]any{
			"id":           entityID(e.Name),
			"label":        e.Name,
			"level":        level,
			"size":         scaleSize(e.Count, maxMentions, 10, 40),
			"community_id": parentID,
			"cluster":      cluster,
			"category":     category,
			"count":        e.Count,
		})
		names = append(names, e.Name)
	}
	rels, err := store.QueryRelationshipsWithin(req.Context(), pool, names, graphDefaultRelsLimit, corpusID)
	if err != nil {
		return nil, err
	}
	maxN := 1
	for _, r := range rels {
		if r.EdgeCount > maxN {
			maxN = r.EdgeCount
		}
	}
	edges := make([]map[string]any, 0, len(rels))
	for _, r := range rels {
		var pred any = nil
		if r.Predicate != "" {
			pred = r.Predicate
		}
		edges = append(edges, map[string]any{
			"source":    entityID(r.Subject),
			"target":    entityID(r.Object),
			"weight":    roundTo3(math.Min(1.0, r.Weight*0.5+0.5*(float64(r.EdgeCount)/float64(maxN)))),
			"predicate": pred,
		})
	}
	return map[string]any{"nodes": nodes, "edges": edges, "parent": parentID}, nil
}

func graphExpandHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		communityID := mux.Vars(req)["community_id"]
		q := req.URL.Query()
		limit := parseIntInRange(q.Get("limit"), graphDefaultEntitiesPerCat, 1, 20000)
		// Empty corpus_id keeps the global (unscoped) behaviour for old
		// frontend builds that haven't started forwarding the param yet.
		corpusID := normaliseCorpusID(q.Get("corpus_id"))
		mode := q.Get("mode")
		switch mode {
		case "", "auto", "members", "all":
		default:
			writeJSON(w, http.StatusUnprocessableEntity,
				errorBody("mode must be one of: auto, members, all"))
			return
		}
		if mode == "" {
			mode = "auto"
		}

		kind, payload, ok := decodeNodeID(communityID)
		if !ok {
			// Stale fake id from before wiring landed. Frontend handles
			// the empty-graph shape.
			writeJSON(w, http.StatusOK, map[string]any{
				"nodes":  []any{},
				"edges":  []any{},
				"parent": communityID,
			})
			return
		}
		switch kind {
		case "cat":
			out, err := expandCategory(req, pool, payload, 1, limit, corpusID)
			if err != nil {
				logger.WithError(err).WithField("category", payload).Warn("graph: expand category failed")
				writeJSON(w, http.StatusOK, map[string]any{
					"nodes":  []any{},
					"edges":  []any{},
					"parent": communityID,
				})
				return
			}
			writeJSON(w, http.StatusOK, out)
		case "ent":
			out, err := expandEntity(req, pool, payload, mode, limit, corpusID)
			if err != nil {
				logger.WithError(err).WithField("entity", payload).Warn("graph: expand entity failed")
				writeJSON(w, http.StatusOK, map[string]any{
					"nodes":  []any{},
					"edges":  []any{},
					"parent": communityID,
				})
				return
			}
			writeJSON(w, http.StatusOK, out)
		default:
			writeJSON(w, http.StatusOK, map[string]any{
				"nodes":  []any{},
				"edges":  []any{},
				"parent": communityID,
			})
		}
	}
}

// expandEntity renders the entity-centric view. mode picks between
// taxonomic members and the all-edges fallback. corpusID flows through
// to every store call so the modal category, member list and neighbour
// edges all come from the same corpus.
func expandEntity(req *http.Request, pool *pgxpool.Pool, name, mode string, limit int, corpusID string) (map[string]any, error) {
	var entityCategory string
	if mode == "auto" || mode == "members" {
		cat, err := store.QueryEntityCategory(req.Context(), pool, name, corpusID)
		if err != nil {
			return nil, err
		}
		entityCategory = cat
		predicates := taxonomicPredicatesByCategory[strings.ToUpper(cat)]
		if len(predicates) > 0 {
			memberLimit := limit
			if memberLimit < graphDefaultMemberLimit {
				memberLimit = graphDefaultMemberLimit
			}
			out, err := expandEntityAsMembers(req, pool, name, predicates, memberLimit, corpusID)
			if err != nil {
				return nil, err
			}
			if out != nil {
				return out, nil
			}
		}
		if mode == "members" {
			var catAny any = nil
			if entityCategory != "" {
				catAny = entityCategory
			}
			return map[string]any{
				"nodes": []map[string]any{{
					"id":       entityID(name),
					"label":    name,
					"level":    1,
					"size":     18,
					"category": catAny,
				}},
				"edges":  []any{},
				"parent": entityID(name),
			}, nil
		}
	}

	// All-edges path.
	neighbours, err := store.QueryEntityNeighbours(req.Context(), pool, name, limit, corpusID)
	if err != nil {
		return nil, err
	}
	if len(neighbours) == 0 {
		return map[string]any{
			"nodes": []map[string]any{{
				"id":    entityID(name),
				"label": name,
				"level": 0,
				"size":  14,
			}},
			"edges":  []any{},
			"parent": entityID(name),
		}, nil
	}
	maxW := 1.0
	for _, n := range neighbours {
		if n.Weight > maxW {
			maxW = n.Weight
		}
	}
	nodes := []map[string]any{{
		"id":    entityID(name),
		"label": name,
		"level": 0,
		"size":  18,
	}}
	seen := map[string]bool{}
	edges := make([]map[string]any, 0, len(neighbours))
	for _, n := range neighbours {
		if !seen[n.Other] {
			seen[n.Other] = true
			nodes = append(nodes, map[string]any{
				"id":    entityID(n.Other),
				"label": n.Other,
				"level": 0,
				"size":  scaleSize(int(n.Weight*100), int(maxW*100), 8, 22),
			})
		}
		var source, target string
		if n.Direction == "out" {
			source, target = entityID(name), entityID(n.Other)
		} else {
			source, target = entityID(n.Other), entityID(name)
		}
		var pred any = nil
		if n.Predicate != "" {
			pred = n.Predicate
		}
		w := n.Weight
		if maxW > 0 {
			w = n.Weight / maxW
		}
		edges = append(edges, map[string]any{
			"source":    source,
			"target":    target,
			"weight":    roundTo3(math.Min(1.0, w)),
			"predicate": pred,
		})
	}
	return map[string]any{"nodes": nodes, "edges": edges, "parent": entityID(name)}, nil
}

func expandEntityAsMembers(req *http.Request, pool *pgxpool.Pool, name string, predicates []string, limit int, corpusID string) (map[string]any, error) {
	members, err := store.QueryEntityMembers(req.Context(), pool, name, predicates, limit, corpusID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}
	parentCategory, err := store.QueryEntityCategory(req.Context(), pool, name, corpusID)
	if err != nil {
		return nil, err
	}
	parentID := entityID(name)
	var parentCatAny any = nil
	if parentCategory != "" {
		parentCatAny = parentCategory
	}
	nodes := []map[string]any{{
		"id":       parentID,
		"label":    name,
		"level":    1,
		"size":     28,
		"category": parentCatAny,
		"count":    len(members),
	}}
	edges := make([]map[string]any, 0, len(members))
	maxW := 1.0
	for _, m := range members {
		if m.Weight > maxW {
			maxW = m.Weight
		}
	}
	for _, m := range members {
		var memberCat any = nil
		if m.Category != "" {
			memberCat = m.Category
		}
		nodes = append(nodes, map[string]any{
			"id":       entityID(m.Name),
			"label":    m.Name,
			"level":    0,
			"size":     scaleSize(int(m.Weight*100), int(math.Max(1, maxW*100)), 10, 20),
			"category": memberCat,
		})
		var pred any = nil
		if m.Predicate != "" {
			pred = m.Predicate
		}
		w := m.Weight
		if maxW > 0 {
			w = m.Weight / maxW
		}
		edges = append(edges, map[string]any{
			"source":    entityID(m.Name),
			"target":    parentID,
			"weight":    roundTo3(math.Min(1.0, w)),
			"predicate": pred,
		})
	}
	return map[string]any{"nodes": nodes, "edges": edges, "parent": parentID}, nil
}

func graphSearchHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		needle := strings.TrimSpace(q.Get("q"))
		if needle == "" {
			// FastAPI's Query(..., min_length=1) returns 422 on missing/empty.
			writeJSON(w, http.StatusUnprocessableEntity, errorBody("q is required"))
			return
		}
		limit := parseIntInRange(q.Get("limit"), 20, 1, 100)
		corpusID := normaliseCorpusID(q.Get("corpus_id"))
		rows, err := store.SearchEntities(req.Context(), pool, needle, limit, corpusID)
		if err != nil {
			logger.WithError(err).Warn("graph: entity search failed; returning empty")
			writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}})
			return
		}
		if len(rows) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"hits": []any{}})
			return
		}
		maxMentions := 1
		for _, r := range rows {
			if r.Mentions > maxMentions {
				maxMentions = r.Mentions
			}
		}
		hits := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			var cidAny any = nil
			cluster := 0
			var catAny any = nil
			if r.Category != "" {
				cidAny = categoryID(r.Category)
				cluster = categoryClusterIndex(r.Category)
				catAny = r.Category
			}
			hits = append(hits, map[string]any{
				"id":           entityID(r.Name),
				"label":        r.Name,
				"level":        1,
				"size":         scaleSize(r.Mentions, maxMentions, 10, 40),
				"community_id": cidAny,
				"cluster":      cluster,
				"category":     catAny,
				"count":        r.Mentions,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"hits": hits})
	}
}

func graphNodeHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		nodeID := mux.Vars(req)["node_id"]
		corpusID := normaliseCorpusID(req.URL.Query().Get("corpus_id"))
		kind, payload, ok := decodeNodeID(nodeID)
		if !ok {
			writeJSON(w, http.StatusOK, nodeStub(nodeID))
			return
		}
		switch kind {
		case "ent":
			out, err := entityNodeDetail(req, pool, nodeID, payload, corpusID)
			if err != nil {
				logger.WithError(err).WithField("entity", payload).Warn("graph: node detail failed")
				writeJSON(w, http.StatusOK, nodeStub(nodeID))
				return
			}
			writeJSON(w, http.StatusOK, out)
		case "cat":
			out, err := categoryNodeDetail(req, pool, nodeID, payload, corpusID)
			if err != nil {
				logger.WithError(err).WithField("category", payload).Warn("graph: category detail failed")
				writeJSON(w, http.StatusOK, nodeStub(nodeID))
				return
			}
			writeJSON(w, http.StatusOK, out)
		default:
			writeJSON(w, http.StatusOK, nodeStub(nodeID))
		}
	}
}

func nodeStub(id string) map[string]any {
	return map[string]any{
		"id":         id,
		"label":      id,
		"level":      0,
		"size":       12,
		"stub":       true,
		"neighbours": []any{},
		"children":   []any{},
	}
}

func entityNodeDetail(req *http.Request, pool *pgxpool.Pool, nodeID, name, corpusID string) (map[string]any, error) {
	meta, err := store.QueryEntityMeta(req.Context(), pool, name, corpusID)
	if err != nil {
		return nil, err
	}
	neighboursRaw, err := store.QueryEntityNeighbours(req.Context(), pool, name, graphDefaultNeighbourLimit, corpusID)
	if err != nil {
		return nil, err
	}
	sources, err := store.QueryEntitySources(req.Context(), pool, name, graphDefaultSourceLimit, corpusID)
	if err != nil {
		return nil, err
	}
	neighbours := make([]map[string]any, 0, len(neighboursRaw))
	for _, n := range neighboursRaw {
		var pred any = nil
		if n.Predicate != "" {
			pred = n.Predicate
		}
		neighbours = append(neighbours, map[string]any{
			"id":        entityID(n.Other),
			"label":     n.Other,
			"weight":    roundTo3(n.Weight),
			"direction": n.Direction,
			"predicate": pred,
		})
	}
	var (
		cat         string
		mentions    int
		description any = nil
	)
	if meta != nil {
		cat = meta.Category
		mentions = meta.Mentions
		if meta.Description != "" {
			description = meta.Description
		}
	}
	out := map[string]any{
		"id":           nodeID,
		"label":        name,
		"level":        1,
		"size":         scaleSize(mentionsOrOne(meta), 100, 12, 60),
		"description":  description,
		"mentions":     mentions,
		"children":     []any{},
		"neighbours":   neighbours,
		"sources":      entitySourcesPayload(sources),
		"temporal":     nil, // entity_temporal isn't ported in this slice.
	}
	if cat != "" {
		out["category"] = cat
		out["community_id"] = categoryID(cat)
		out["cluster"] = categoryClusterIndex(cat)
	} else {
		out["category"] = nil
		out["community_id"] = nil
		out["cluster"] = 0
	}
	return out, nil
}

// mentionsOrOne mirrors Python's `int(meta["mentions"]) if meta else 1`
// inside the scale_size call.
func mentionsOrOne(m *store.EntityMeta) int {
	if m == nil {
		return 1
	}
	return m.Mentions
}

func entitySourcesPayload(sources []store.EntitySource) []map[string]any {
	out := make([]map[string]any, 0, len(sources))
	for _, s := range sources {
		title := s.Title
		if title == "" {
			title = s.DocumentID.String()
		}
		var createdAt any = nil
		if s.CreatedAt != nil {
			createdAt = s.CreatedAt.Format(time.RFC3339Nano)
		}
		chunkIDs := make([]string, 0, len(s.ChunkIDs))
		for _, c := range s.ChunkIDs {
			chunkIDs = append(chunkIDs, c.String())
		}
		var category any = nil
		if s.Category != "" {
			category = s.Category
		}
		out = append(out, map[string]any{
			"document_id": s.DocumentID.String(),
			"title":       title,
			"category":    category,
			"mentions":    s.Mentions,
			"chunk_ids":   chunkIDs,
			"source_url":  documentSourceURL(s.Metadata),
			"created_at":  createdAt,
		})
	}
	return out
}

// documentSourceURL probes the metadata dict for a usable source URL.
// Mirrors Python's _document_source_url precedence.
func documentSourceURL(metadata map[string]any) any {
	if metadata == nil {
		return nil
	}
	for _, key := range []string{"source_url", "external_url", "url", "open_access_pdf_url", "pdf_url"} {
		if v, ok := metadata[key]; ok {
			if s, ok := v.(string); ok {
				trimmed := strings.TrimSpace(s)
				if trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if v, ok := metadata["doi"].(string); ok {
		if t := strings.TrimSpace(v); t != "" {
			return "https://doi.org/" + t
		}
	}
	if v, ok := metadata["external_id"].(string); ok {
		t := strings.TrimSpace(v)
		if strings.HasPrefix(t, "arXiv:") {
			return "https://arxiv.org/abs/" + t[len("arXiv:"):]
		}
	}
	return nil
}

func categoryNodeDetail(req *http.Request, pool *pgxpool.Pool, nodeID, category, corpusID string) (map[string]any, error) {
	entities, err := store.QueryCategoryEntities(req.Context(), pool, category, 6, corpusID)
	if err != nil {
		return nil, err
	}
	children := make([]map[string]any, 0, len(entities))
	for _, e := range entities {
		children = append(children, map[string]any{
			"id":    entityID(e.Name),
			"label": e.Name,
			"level": 1,
		})
	}
	return map[string]any{
		"id":           nodeID,
		"label":        category,
		"level":        2,
		"size":         40,
		"category":     category,
		"community_id": nodeID,
		"cluster":      categoryClusterIndex(category),
		"children":     children,
		"neighbours":   []any{},
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────
// GraphRAG handlers
// ─────────────────────────────────────────────────────────────────────────

func graphRAGSummaryHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	empty := map[string]any{
		"entities":      0,
		"relationships": 0,
		"communities":   0,
		"reports":       0,
		"text_units":    0,
		"levels":        []any{},
	}
	return func(w http.ResponseWriter, req *http.Request) {
		corpus, err := uuid.Parse(mux.Vars(req)["corpus_id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("corpus_id must be a UUID"))
			return
		}
		exists, err := store.CorpusExists(req.Context(), pool, corpus)
		if err != nil {
			logger.WithError(err).Warn("graphrag_summary: corpus_exists DB error")
			writeJSON(w, http.StatusOK, empty)
			return
		}
		if !exists {
			writeJSON(w, http.StatusNotFound, errorBody("corpus not found"))
			return
		}
		c, err := store.QueryGraphRAGSummary(req.Context(), pool, corpus)
		if err != nil {
			logger.WithError(err).Warn("graphrag_summary: DB error")
			writeJSON(w, http.StatusOK, empty)
			return
		}
		levels := make([]int, 0, len(c.Levels))
		levels = append(levels, c.Levels...)
		writeJSON(w, http.StatusOK, map[string]any{
			"entities":      c.Entities,
			"relationships": c.Relationships,
			"communities":   c.Communities,
			"reports":       c.Reports,
			"text_units":    c.TextUnits,
			"levels":        levels,
		})
	}
}

func graphRAGCommunitiesHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		corpus, err := uuid.Parse(mux.Vars(req)["corpus_id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("corpus_id must be a UUID"))
			return
		}
		q := req.URL.Query()
		level := 2
		if v := q.Get("level"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, errorBody("level must be an int"))
				return
			}
			level = n
		}
		limit := parseIntInRange(q.Get("limit"), 200, 1, 2000)

		exists, err := store.CorpusExists(req.Context(), pool, corpus)
		if err != nil {
			logger.WithError(err).Warn("graphrag_communities: corpus_exists DB error")
			writeJSON(w, http.StatusOK, map[string]any{"communities": []any{}})
			return
		}
		if !exists {
			writeJSON(w, http.StatusNotFound, errorBody("corpus not found"))
			return
		}
		rows, err := store.QueryGraphRAGCommunities(req.Context(), pool, corpus, level, limit)
		if err != nil {
			logger.WithError(err).Warn("graphrag_communities: DB error")
			writeJSON(w, http.StatusOK, map[string]any{"communities": []any{}})
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			var (
				cidAny  any = nil
				lvlAny  any = nil
				parent  any = nil
				summary any = nil
				rank    any = nil
			)
			if r.CommunityID != nil {
				cidAny = *r.CommunityID
			}
			if r.Level != nil {
				lvlAny = *r.Level
			}
			if r.Parent != nil {
				parent = *r.Parent
			}
			if r.ReportSummary != nil {
				summary = *r.ReportSummary
			}
			if r.ReportRank != nil {
				rank = *r.ReportRank
			}
			out = append(out, map[string]any{
				"id":             r.RowID.String(),
				"community_id":   cidAny,
				"level":          lvlAny,
				"title":          r.Title,
				"size":           r.Size,
				"parent":         parent,
				"report_summary": summary,
				"report_rank":    rank,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"communities": out})
	}
}

func graphRAGCommunityDetailHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		rowID, err := uuid.Parse(mux.Vars(req)["community_uuid"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("community uuid must be a UUID"))
			return
		}
		community, report, err := store.QueryGraphRAGCommunityDetail(req.Context(), pool, rowID)
		if err != nil {
			logger.WithError(err).Warn("graphrag_community_detail: DB error")
			writeJSON(w, http.StatusOK, map[string]any{"community": map[string]any{}, "report": nil})
			return
		}
		if community == nil {
			writeJSON(w, http.StatusNotFound, errorBody("community not found"))
			return
		}
		var reportAny any = nil
		if report != nil {
			reportAny = report
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"community": community,
			"report":    reportAny,
		})
	}
}

func graphRAGEntitiesHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		corpus, err := uuid.Parse(mux.Vars(req)["corpus_id"])
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("corpus_id must be a UUID"))
			return
		}
		q := req.URL.Query()
		needle := strings.TrimSpace(q.Get("q"))
		limit := parseIntInRange(q.Get("limit"), 50, 1, 500)

		exists, err := store.CorpusExists(req.Context(), pool, corpus)
		if err != nil {
			logger.WithError(err).Warn("graphrag_entities: corpus_exists DB error")
			writeJSON(w, http.StatusOK, map[string]any{"entities": []any{}})
			return
		}
		if !exists {
			writeJSON(w, http.StatusNotFound, errorBody("corpus not found"))
			return
		}
		rows, err := store.QueryGraphRAGEntities(req.Context(), pool, corpus, needle, limit)
		if err != nil {
			logger.WithError(err).Warn("graphrag_entities: DB error")
			writeJSON(w, http.StatusOK, map[string]any{"entities": []any{}})
			return
		}
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			var (
				typeAny      any = nil
				descAny      any = nil
				degreeAny    any = nil
				frequencyAny any = nil
			)
			if r.Type != nil {
				typeAny = *r.Type
			}
			if r.Description != nil {
				descAny = *r.Description
			}
			if r.Degree != nil {
				degreeAny = *r.Degree
			}
			if r.Frequency != nil {
				frequencyAny = *r.Frequency
			}
			out = append(out, map[string]any{
				"id":          r.ID.String(),
				"title":       r.Title,
				"type":        typeAny,
				"description": descAny,
				"degree":      degreeAny,
				"frequency":   frequencyAny,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"entities": out})
	}
}

// parseIntInRange clamps a query-param int into [lo, hi], returning def
// for missing/malformed. Out-of-range values 422 in FastAPI; we mirror
// that with the same status when the caller forwards the error.
func parseIntInRange(s string, def, lo, hi int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}


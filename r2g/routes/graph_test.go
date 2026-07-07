package routes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/sirus20x6/adamaton-knowledge/r2g/store"
)

// TestGraphEnc_RoundTrip confirms the URL-safe base64 with stripped
// padding matches Python's urlsafe_b64encode + .rstrip("="). The
// frontend caches node ids; mismatched encoding would invalidate cached
// state on the cutover.
func TestGraphEnc_RoundTrip(t *testing.T) {
	cases := []string{
		"Person",
		"Albert Einstein",
		"Café",      // utf-8 mb
		"O'Hare",    // apostrophe
		"a/b c+d=e", // chars that base64 cares about
		"💀 ghoul",   // multi-byte emoji
		"longer name that fills four bytes",
	}
	for _, c := range cases {
		enc := graphEnc(c)
		dec, err := graphDec(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", c, err)
		}
		if dec != c {
			t.Fatalf("round-trip failed: got %q want %q", dec, c)
		}
		// padding must be stripped
		for _, r := range enc {
			if r == '=' {
				t.Fatalf("encoded %q kept padding: %q", c, enc)
			}
		}
	}
}

// TestEntityID_KnownValues pins the Python reference outputs so a future
// refactor can't silently change the encoding.
func TestEntityID_KnownValues(t *testing.T) {
	// Reference values produced by:
	//   python -c "import base64;print('ent:'+base64.urlsafe_b64encode('Person'.encode()).decode().rstrip('='))"
	pairs := []struct {
		name, want string
	}{
		{"Person", "ent:UGVyc29u"},
		{"Albert Einstein", "ent:QWxiZXJ0IEVpbnN0ZWlu"},
	}
	for _, p := range pairs {
		got := entityID(p.name)
		if got != p.want {
			t.Errorf("entityID(%q)=%q want %q", p.name, got, p.want)
		}
	}
}

// TestDecodeNodeID_RejectsUnknown confirms unknown prefixes return ok=false
// so the handler falls back to the stub-node payload.
func TestDecodeNodeID_RejectsUnknown(t *testing.T) {
	_, _, ok := decodeNodeID("garbage")
	if ok {
		t.Error("expected ok=false on unknown prefix")
	}
	_, _, ok = decodeNodeID("ent:not_base64!@#$")
	if ok {
		t.Error("expected ok=false on malformed base64")
	}
	kind, payload, ok := decodeNodeID(entityID("Albert"))
	if !ok || kind != "ent" || payload != "Albert" {
		t.Errorf("entity round-trip: ok=%v kind=%q payload=%q", ok, kind, payload)
	}
	kind, payload, ok = decodeNodeID(categoryID("Person"))
	if !ok || kind != "cat" || payload != "Person" {
		t.Errorf("category round-trip: ok=%v kind=%q payload=%q", ok, kind, payload)
	}
}

// TestGraphSeed_Deterministic confirms that the fallback seed is
// reproducible — same inputs always give the same uint32.
func TestGraphSeed_Deterministic(t *testing.T) {
	a := graphSeed("abc", "1")
	b := graphSeed("abc", "1")
	if a != b {
		t.Errorf("seed not deterministic: %d vs %d", a, b)
	}
	// Differs from a different input.
	if graphSeed("abc", "2") == a {
		t.Error("seed collides across distinct inputs (suspicious)")
	}
}

// TestFakeCommunities_Deterministic builds the synthetic graph twice
// with the same inputs and compares the JSON byte-for-byte. Mirrors the
// Python module's claim that the empty-DB fallback is reproducible.
func TestFakeCommunities_Deterministic(t *testing.T) {
	a := fakeCommunities("00000000-0000-0000-0000-000000000001", 2, graphFallbackClusterCount)
	b := fakeCommunities("00000000-0000-0000-0000-000000000001", 2, graphFallbackClusterCount)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		t.Errorf("synthetic graph not deterministic:\n A=%s\n B=%s", aj, bj)
	}
}

// TestFakeCommunities_Level2 checks the level-2 fallback structure: one
// node per cluster + one edge per cluster (ring).
func TestFakeCommunities_Level2(t *testing.T) {
	g := fakeCommunities("00000000-0000-0000-0000-000000000001", 2, graphFallbackClusterCount)
	nodes := g["nodes"].([]map[string]any)
	edges := g["edges"].([]map[string]any)
	if len(nodes) != graphFallbackClusterCount {
		t.Errorf("level-2 nodes = %d, want %d", len(nodes), graphFallbackClusterCount)
	}
	if len(edges) != graphFallbackClusterCount {
		t.Errorf("level-2 edges = %d, want %d", len(edges), graphFallbackClusterCount)
	}
	// node[0] should have community_id == its own id.
	if nodes[0]["id"] != nodes[0]["community_id"] {
		t.Error("level-2 node should self-reference community_id")
	}
}

// TestFakeCommunities_DifferentSeedsDiffer guards against accidentally
// dropping the seed input (regression bait — a fix that makes the LCG
// always start at 0 would still pass the determinism test above).
func TestFakeCommunities_DifferentSeedsDiffer(t *testing.T) {
	a := fakeCommunities("00000000-0000-0000-0000-000000000001", 2, graphFallbackClusterCount)
	b := fakeCommunities("00000000-0000-0000-0000-000000000002", 2, graphFallbackClusterCount)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) == string(bj) {
		t.Error("expected different fake graphs for different corpus ids")
	}
}

// TestScaleSize covers the log1p mapping for the edge cases the handler
// hits in practice (zero max, value == max, value much smaller).
func TestScaleSize(t *testing.T) {
	if got := scaleSize(0, 0, 10, 40); got != 10 {
		t.Errorf("zero max: got %d want 10", got)
	}
	if got := scaleSize(0, 100, 10, 40); got != 10 {
		t.Errorf("value=0: got %d want 10", got)
	}
	if got := scaleSize(100, 100, 10, 40); got != 40 {
		t.Errorf("value=max: got %d want 40", got)
	}
}

// TestRoundTo3 verifies the weight rounding matches Python's round(x, 3).
func TestRoundTo3(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0.123456, 0.123},
		{0.123999, 0.124},
		{1.0, 1.0},
		{0.0005, 0.001},
	}
	for _, c := range cases {
		if got := roundTo3(c.in); got != c.want {
			t.Errorf("roundTo3(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

// TestDocumentSourceURL exercises the precedence ladder used when
// composing /node/{id} payloads.
func TestDocumentSourceURL(t *testing.T) {
	cases := []struct {
		md   map[string]any
		want any
	}{
		{nil, nil},
		{map[string]any{}, nil},
		{map[string]any{"source_url": "https://x.example/a"}, "https://x.example/a"},
		{map[string]any{"url": " https://x.example/b "}, "https://x.example/b"},
		// source_url wins over doi
		{map[string]any{"source_url": "https://x.example/c", "doi": "10.1/abc"},
			"https://x.example/c"},
		// doi alone falls through to the doi.org URL
		{map[string]any{"doi": "10.1/abc"}, "https://doi.org/10.1/abc"},
		// external_id arXiv handling
		{map[string]any{"external_id": "arXiv:2401.00001"}, "https://arxiv.org/abs/2401.00001"},
		// non-arxiv external_id is ignored
		{map[string]any{"external_id": "biorxiv:123"}, nil},
		// empty strings don't count
		{map[string]any{"source_url": "   "}, nil},
	}
	for i, c := range cases {
		got := documentSourceURL(c.md)
		if got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

// TestCategoryClusterIndex_Bounded confirms the cluster bucket stays in
// [0, DEFAULT_CLUSTER_COUNT). The frontend colour ramp has that many
// slots; an out-of-range value would render as an uncoloured node.
func TestCategoryClusterIndex_Bounded(t *testing.T) {
	for _, c := range []string{"Person", "Concept", "Technology", "Event", "Faction",
		"", "X", "  trailing  ", "Café"} {
		idx := categoryClusterIndex(c)
		if idx < 0 || idx >= graphFallbackClusterCount {
			t.Errorf("categoryClusterIndex(%q)=%d out of [0,%d)",
				c, idx, graphFallbackClusterCount)
		}
	}
}

// TestParseIntInRange covers the four edge cases the handlers care about:
// missing param uses the default, out-of-range clamps, invalid uses
// default, in-range passes through.
func TestParseIntInRange(t *testing.T) {
	if parseIntInRange("", 10, 1, 100) != 10 {
		t.Error("empty should return default")
	}
	if parseIntInRange("0", 10, 1, 100) != 1 {
		t.Error("below lo should clamp to lo")
	}
	if parseIntInRange("9999", 10, 1, 100) != 100 {
		t.Error("above hi should clamp to hi")
	}
	if parseIntInRange("not-a-num", 10, 1, 100) != 10 {
		t.Error("invalid should return default")
	}
	if parseIntInRange("42", 10, 1, 100) != 42 {
		t.Error("in-range should pass through")
	}
}

// TestGraphV3Relationships_BadCorpus pins the input validation on the
// native /v3/graphs/{corpus}/relationships: a non-UUID corpus is a 400
// before any DB access (pool is nil here and must not be touched).
func TestGraphV3Relationships_BadCorpus(t *testing.T) {
	r := mux.NewRouter()
	RegisterGraph(r, nil, quietLogger())
	req := httptest.NewRequest(http.MethodGet, "/v3/graphs/not-a-uuid/relationships", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

// TestGraphRelationshipPayload_Shape pins the R2R wire shape the
// dashboard's fetchR2RRelationships parser reads: subject_id/object_id
// as UUID strings when linked, null otherwise; chunk_ids as a string
// array; metadata always an object.
func TestGraphRelationshipPayload_Shape(t *testing.T) {
	sid := uuid.New()
	oid := uuid.New()
	pid := uuid.New()
	chunk := uuid.New()
	full := store.GraphRelationship{
		ID: uuid.New(), ParentID: &pid,
		Subject: "A", Predicate: "REL", Object: "B",
		Description: "d", SubjectID: &sid, ObjectID: &oid,
		Weight: 0.5, ChunkIDs: []uuid.UUID{chunk},
		Metadata: map[string]any{"k": "v"},
	}
	p := graphRelationshipPayload(full)
	if p["subject_id"] != sid.String() || p["object_id"] != oid.String() {
		t.Errorf("subject_id/object_id = %v/%v", p["subject_id"], p["object_id"])
	}
	if p["parent_id"] != pid.String() {
		t.Errorf("parent_id = %v", p["parent_id"])
	}
	if got := p["chunk_ids"].([]string); len(got) != 1 || got[0] != chunk.String() {
		t.Errorf("chunk_ids = %v", p["chunk_ids"])
	}

	// Sparse row: nils serialize to null, metadata coerces to {}.
	sparse := store.GraphRelationship{ID: uuid.New(), Subject: "A", Object: "B"}
	p2 := graphRelationshipPayload(sparse)
	for _, k := range []string{"subject_id", "object_id", "parent_id", "description"} {
		if p2[k] != nil {
			t.Errorf("%s = %v, want nil", k, p2[k])
		}
	}
	if p2["metadata"] == nil {
		t.Error("metadata should coerce to an empty object, not null")
	}
	b, err := json.Marshal(p2)
	if err != nil {
		t.Fatalf("payload must be JSON-serializable: %v", err)
	}
	if !bytes.Contains(b, []byte(`"metadata":{}`)) {
		t.Errorf("serialized payload = %s", b)
	}
}

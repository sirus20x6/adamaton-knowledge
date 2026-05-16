package bench

import (
	"math"
	"testing"

	"github.com/google/uuid"
)

func mkIDs(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.New()
	}
	return out
}

func TestHitAndRecallAtK(t *testing.T) {
	ids := mkIDs(5)
	gold := []uuid.UUID{ids[2]}
	returned := []uuid.UUID{ids[0], ids[1], ids[2], ids[3], ids[4]}
	if got := HitAtK(gold, returned, 1); got != 0 {
		t.Fatalf("hit@1 want 0 got %d", got)
	}
	if got := HitAtK(gold, returned, 3); got != 1 {
		t.Fatalf("hit@3 want 1 got %d", got)
	}
	if got := RecallAtK(gold, returned, 3); got != 1.0 {
		t.Fatalf("recall@3 want 1.0 got %v", got)
	}
	if got := RecallAtK(gold, returned, 1); got != 0 {
		t.Fatalf("recall@1 want 0 got %v", got)
	}

	// Multi-gold case: 2 of 3 returned in top-3.
	gold2 := []uuid.UUID{ids[0], ids[2], uuid.New()}
	if got := RecallAtK(gold2, returned, 3); math.Abs(got-2.0/3.0) > 1e-9 {
		t.Fatalf("recall@3 (multi-gold) want 0.667 got %v", got)
	}
}

func TestMRRAndAP(t *testing.T) {
	ids := mkIDs(5)
	returned := []uuid.UUID{ids[0], ids[1], ids[2], ids[3], ids[4]}

	// First gold at rank 3 → MRR = 1/3.
	gold := []uuid.UUID{ids[2]}
	if got := MRR(gold, returned); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Fatalf("mrr want 0.333 got %v", got)
	}

	// AP with golds at ranks 1 and 3: precisions at hits are 1/1 + 2/3, divide by 2.
	gold = []uuid.UUID{ids[0], ids[2]}
	want := (1.0 + 2.0/3.0) / 2.0
	if got := AveragePrecision(gold, returned); math.Abs(got-want) > 1e-9 {
		t.Fatalf("AP want %v got %v", want, got)
	}

	// No-hit case → MRR=AP=0.
	missing := mkIDs(2)
	if got := MRR(missing, returned); got != 0 {
		t.Fatalf("mrr no-hit want 0 got %v", got)
	}
	if got := AveragePrecision(missing, returned); got != 0 {
		t.Fatalf("AP no-hit want 0 got %v", got)
	}
}

func TestNDCG(t *testing.T) {
	ids := mkIDs(4)
	returned := []uuid.UUID{ids[0], ids[1], ids[2], ids[3]}

	// Single gold at top → nDCG@4 == 1.
	gold := []uuid.UUID{ids[0]}
	if got := NDCGAtK(gold, returned, 4); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("ndcg perfect want 1 got %v", got)
	}

	// Single gold at rank 4 → nDCG = (1/log2(5)) / (1/log2(2)) = log2(2)/log2(5).
	gold = []uuid.UUID{ids[3]}
	want := math.Log2(2) / math.Log2(5)
	if got := NDCGAtK(gold, returned, 4); math.Abs(got-want) > 1e-9 {
		t.Fatalf("ndcg rank-4 want %v got %v", want, got)
	}
}

func TestPercentile(t *testing.T) {
	vs := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if got := Percentile(vs, 50); got != 55 {
		t.Fatalf("p50 want 55 got %d", got)
	}
	if got := Percentile(vs, 0); got != 10 {
		t.Fatalf("p0 want 10 got %d", got)
	}
	if got := Percentile(vs, 100); got != 100 {
		t.Fatalf("p100 want 100 got %d", got)
	}
	if got := Percentile([]int64{}, 50); got != 0 {
		t.Fatalf("p50 empty want 0 got %d", got)
	}
}

func TestEdgeCases(t *testing.T) {
	if got := HitAtK(nil, mkIDs(3), 5); got != 0 {
		t.Fatalf("hit empty gold want 0 got %d", got)
	}
	if got := RecallAtK(nil, mkIDs(3), 5); got != 0 {
		t.Fatalf("recall empty gold want 0 got %v", got)
	}
	if got := MRR(mkIDs(3), nil); got != 0 {
		t.Fatalf("mrr empty returned want 0 got %v", got)
	}
}

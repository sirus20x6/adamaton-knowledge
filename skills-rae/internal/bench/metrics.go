package bench

import (
	"math"
	"sort"

	"github.com/google/uuid"
)

// HitAtK returns 1 iff any gold id appears in returned[:k]. k<=0
// means "look at all returned".
func HitAtK(gold, returned []uuid.UUID, k int) int {
	if len(gold) == 0 || len(returned) == 0 {
		return 0
	}
	g := idSet(gold)
	n := len(returned)
	if k > 0 && k < n {
		n = k
	}
	for i := 0; i < n; i++ {
		if _, ok := g[returned[i]]; ok {
			return 1
		}
	}
	return 0
}

// RecallAtK = |gold ∩ returned[:k]| / |gold|. k<=0 means "all".
func RecallAtK(gold, returned []uuid.UUID, k int) float64 {
	if len(gold) == 0 {
		return 0
	}
	g := idSet(gold)
	n := len(returned)
	if k > 0 && k < n {
		n = k
	}
	hits := 0
	for i := 0; i < n; i++ {
		if _, ok := g[returned[i]]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(g))
}

// MRR = 1/rank of the first gold hit; 0 if no gold appears. Considers
// the full returned list (a rank is undefined past it).
func MRR(gold, returned []uuid.UUID) float64 {
	if len(gold) == 0 || len(returned) == 0 {
		return 0
	}
	g := idSet(gold)
	for i, id := range returned {
		if _, ok := g[id]; ok {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// AveragePrecision is the standard binary-relevance AP, computed over
// the full returned list. The denominator is |gold| so an exhaustive
// retrieval that returns every relevant item gets AP = 1.
func AveragePrecision(gold, returned []uuid.UUID) float64 {
	if len(gold) == 0 || len(returned) == 0 {
		return 0
	}
	g := idSet(gold)
	hits := 0
	sum := 0.0
	for i, id := range returned {
		if _, ok := g[id]; !ok {
			continue
		}
		hits++
		sum += float64(hits) / float64(i+1)
	}
	return sum / float64(len(g))
}

// NDCGAtK uses binary relevance — gold ids get gain 1, others 0. The
// ideal DCG is min(|gold|, k) ones at the top, which equals
// sum_{i=1..min(|gold|,k)} 1/log2(i+1).
func NDCGAtK(gold, returned []uuid.UUID, k int) float64 {
	if len(gold) == 0 || len(returned) == 0 {
		return 0
	}
	g := idSet(gold)
	n := len(returned)
	if k > 0 && k < n {
		n = k
	}
	dcg := 0.0
	for i := 0; i < n; i++ {
		if _, ok := g[returned[i]]; ok {
			dcg += 1.0 / math.Log2(float64(i+2)) // i+2 because i is 0-based; log2(rank+1)
		}
	}
	idealN := len(g)
	if k > 0 && k < idealN {
		idealN = k
	}
	idcg := 0.0
	for i := 0; i < idealN; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// Percentile returns the value at percentile p (0..100) of vs. Uses
// linear interpolation between the two nearest indices — the same
// rule as numpy's default and Postgres's percentile_cont.
func Percentile(vs []int64, p float64) int64 {
	if len(vs) == 0 {
		return 0
	}
	cp := make([]int64, len(vs))
	copy(cp, vs)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	if p <= 0 {
		return cp[0]
	}
	if p >= 100 {
		return cp[len(cp)-1]
	}
	rank := (p / 100) * float64(len(cp)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return cp[lo]
	}
	frac := rank - float64(lo)
	return cp[lo] + int64(frac*float64(cp[hi]-cp[lo]))
}

// MeanInt64 — convenience for latency aggregates. Returns 0 on empty
// input (callers treat that as "no data").
func MeanInt64(vs []int64) int64 {
	if len(vs) == 0 {
		return 0
	}
	var sum int64
	for _, v := range vs {
		sum += v
	}
	return sum / int64(len(vs))
}

func idSet(ids []uuid.UUID) map[uuid.UUID]struct{} {
	out := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

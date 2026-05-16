package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteMarkdown renders a human-readable report. Stable order so the
// output diffs cleanly in git.
func WriteMarkdown(w io.Writer, s RunSummary, results []QueryResult) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	dur := s.EndedAt.Sub(s.StartedAt)
	fmt.Fprintf(bw, "# SkillsBench report — %s\n\n", s.Dataset)
	fmt.Fprintf(bw, "**Started**: %s\n", s.StartedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(bw, "**Duration**: %s\n", dur.Round(1e6))
	fmt.Fprintf(bw, "**Endpoint**: %s\n", endpointForReport(s.Config))
	fmt.Fprintf(bw, "**Base URL**: %s\n", s.Config.BaseURL)
	fmt.Fprintf(bw, "**TopK**: %d   **Concurrency**: %d\n\n", s.Config.TopK, s.Config.Concurrency)

	fmt.Fprintf(bw, "## Weights\n\n")
	fmt.Fprintf(bw, "- retrieval: α=%.3f β=%.3f γ=%.3f λ=%.3f\n",
		s.Config.Weights.Alpha, s.Config.Weights.Beta, s.Config.Weights.Gamma, s.Config.Weights.Lambda)
	fmt.Fprintf(bw, "- compile (Aff): q_rel=%.3f f_skill=%.3f r_sel=%.3f g_same=%.3f g_active=%.3f\n\n",
		s.Config.AffWeights.QRel, s.Config.AffWeights.FSkill, s.Config.AffWeights.RSel,
		s.Config.AffWeights.GSame, s.Config.AffWeights.GActive)

	fmt.Fprintf(bw, "## Aggregate\n\n")
	fmt.Fprintf(bw, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(bw, "| N | %d |\n", s.N)
	fmt.Fprintf(bw, "| errors | %d |\n", s.NErrors)
	fmt.Fprintf(bw, "| Hit@1 | %.4f |\n", s.HitRateAt1)
	fmt.Fprintf(bw, "| Hit@K | %.4f |\n", s.HitRateAtK)
	fmt.Fprintf(bw, "| Recall@K (mean) | %.4f |\n", s.MeanRecallAtK)
	fmt.Fprintf(bw, "| MRR (mean) | %.4f |\n", s.MeanMRR)
	fmt.Fprintf(bw, "| nDCG@K (mean) | %.4f |\n", s.MeanNDCG)
	fmt.Fprintf(bw, "| MAP | %.4f |\n", s.MeanAP)
	fmt.Fprintf(bw, "| latency mean (ms) | %d |\n", s.MeanLatencyMs)
	fmt.Fprintf(bw, "| latency p50 (ms) | %d |\n", s.P50LatencyMs)
	fmt.Fprintf(bw, "| latency p95 (ms) | %d |\n", s.P95LatencyMs)
	fmt.Fprintf(bw, "| latency p99 (ms) | %d |\n", s.P99LatencyMs)
	fmt.Fprintln(bw)

	// Per-item table — sort by id for stable diffs.
	sorted := SortResultsByID(results)
	fmt.Fprintf(bw, "## Per-item\n\n")
	fmt.Fprintf(bw, "| id | hit@1 | hit@K | recall@K | mrr | ndcg | ms | query |\n")
	fmt.Fprintf(bw, "|---|---|---|---|---|---|---|---|\n")
	for _, r := range sorted {
		q := r.Item.Query
		if len(q) > 70 {
			q = q[:67] + "..."
		}
		q = strings.ReplaceAll(q, "|", "\\|")
		if r.Error != "" {
			fmt.Fprintf(bw, "| %s | — | — | — | — | — | %d | ERR: %s |\n",
				r.Item.ID, r.ElapsedMs, summariseErr(r.Error))
			continue
		}
		fmt.Fprintf(bw, "| %s | %d | %d | %.3f | %.3f | %.3f | %d | %s |\n",
			r.Item.ID, r.HitAt1, r.HitAtK, r.RecallAtK, r.MRR, r.NDCG, r.ElapsedMs, q)
	}
	fmt.Fprintln(bw)

	if s.NErrors > 0 {
		fmt.Fprintf(bw, "## Errors\n\n")
		for _, r := range sorted {
			if r.Error == "" {
				continue
			}
			fmt.Fprintf(bw, "- **%s**: %s\n", r.Item.ID, r.Error)
		}
		fmt.Fprintln(bw)
	}
	return nil
}

// WriteSweepMarkdown renders the ranked grid result table. Top-of-list
// is the best cell by the chosen SortKey.
func WriteSweepMarkdown(w io.Writer, ds string, sortBy SortKey, results []SweepResult) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	fmt.Fprintf(bw, "# SkillsBench sweep — %s\n\n", ds)
	fmt.Fprintf(bw, "Sorted by **%s**, descending. %d cells.\n\n", sortBy, len(results))

	fmt.Fprintf(bw, "| rank | α | β | γ | λ | q_rel | f_skill | r_sel | g_same | g_active | MRR | Recall@K | nDCG | Hit@1 | p50 ms |\n")
	fmt.Fprintf(bw, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for i, r := range results {
		s := r.Summary
		fmt.Fprintf(bw, "| %d | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %.2f | %.4f | %.4f | %.4f | %.4f | %d |\n",
			i+1,
			r.Weights.Alpha, r.Weights.Beta, r.Weights.Gamma, r.Weights.Lambda,
			r.AffWeights.QRel, r.AffWeights.FSkill, r.AffWeights.RSel, r.AffWeights.GSame, r.AffWeights.GActive,
			s.MeanMRR, s.MeanRecallAtK, s.MeanNDCG, s.HitRateAt1, s.P50LatencyMs)
	}
	return nil
}

// WriteResultsJSONL writes one JSON object per line — stable order
// for diffs.
func WriteResultsJSONL(w io.Writer, results []QueryResult) error {
	sorted := SortResultsByID(results)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	for _, r := range sorted {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := bw.Write(b); err != nil {
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

// WriteSummaryJSON writes the RunSummary alone (pretty-printed).
func WriteSummaryJSON(w io.Writer, s RunSummary) error {
	return json.NewEncoder(w).Encode(s)
}

// CompareRuns writes a side-by-side delta table between two summaries.
// Useful for A/B testing a config change against a baseline.
func CompareRuns(w io.Writer, label string, base, candidate RunSummary) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	fmt.Fprintf(bw, "## A/B — %s\n\n", label)
	fmt.Fprintf(bw, "| metric | baseline | candidate | Δ |\n|---|---|---|---|\n")
	rows := []struct {
		name string
		b, c float64
	}{
		{"Hit@1", base.HitRateAt1, candidate.HitRateAt1},
		{"Hit@K", base.HitRateAtK, candidate.HitRateAtK},
		{"Recall@K", base.MeanRecallAtK, candidate.MeanRecallAtK},
		{"MRR", base.MeanMRR, candidate.MeanMRR},
		{"nDCG", base.MeanNDCG, candidate.MeanNDCG},
		{"MAP", base.MeanAP, candidate.MeanAP},
	}
	for _, r := range rows {
		delta := r.c - r.b
		sign := ""
		if delta > 0 {
			sign = "+"
		}
		fmt.Fprintf(bw, "| %s | %.4f | %.4f | %s%.4f |\n", r.name, r.b, r.c, sign, delta)
	}
	// Latency too — separate because integer.
	lrows := []struct {
		name string
		b, c int64
	}{
		{"mean ms", base.MeanLatencyMs, candidate.MeanLatencyMs},
		{"p50 ms", base.P50LatencyMs, candidate.P50LatencyMs},
		{"p95 ms", base.P95LatencyMs, candidate.P95LatencyMs},
	}
	for _, r := range lrows {
		delta := r.c - r.b
		sign := ""
		if delta > 0 {
			sign = "+"
		}
		fmt.Fprintf(bw, "| %s | %d | %d | %s%d |\n", r.name, r.b, r.c, sign, delta)
	}
	return nil
}

func endpointForReport(cfg Config) string {
	if cfg.Endpoint == "" {
		return "compile"
	}
	return cfg.Endpoint
}

func summariseErr(e string) string {
	if len(e) > 80 {
		return e[:77] + "..."
	}
	return e
}

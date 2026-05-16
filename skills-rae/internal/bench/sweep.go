package bench

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Grid expresses a Cartesian product to sweep across. Each axis is a
// finite list of values; if an axis is empty the corresponding
// Weights/AffWeights field is left at its default.
type Grid struct {
	Alpha   []float64 `json:"alpha,omitempty"`
	Beta    []float64 `json:"beta,omitempty"`
	Gamma   []float64 `json:"gamma,omitempty"`
	Lambda  []float64 `json:"lambda,omitempty"`
	QRel    []float64 `json:"q_rel,omitempty"`
	FSkill  []float64 `json:"f_skill,omitempty"`
	RSel    []float64 `json:"r_sel,omitempty"`
	GSame   []float64 `json:"g_same,omitempty"`
	GActive []float64 `json:"g_active,omitempty"`
}

// SortKey selects which summary metric Sweep ranks by.
type SortKey string

const (
	SortByMRR    SortKey = "mrr"
	SortByRecall SortKey = "recall"
	SortByNDCG   SortKey = "ndcg"
	SortByMAP    SortKey = "map"
	SortByHit1   SortKey = "hit_at_1"
)

// Sweep runs Cartesian product of Grid against the dataset and
// returns SweepResults sorted descending by sortBy. The caller is
// responsible for budgeting: a 4×4×4 grid is 64 evals.
func Sweep(ctx context.Context, ds *Dataset, base Config, grid Grid, client *Client, sortBy SortKey) ([]SweepResult, error) {
	if ds == nil || len(ds.Items) == 0 {
		return nil, errors.New("sweep: empty dataset")
	}
	if client == nil {
		return nil, errors.New("sweep: nil client")
	}
	combos := expand(grid, base)
	if len(combos) == 0 {
		return nil, errors.New("sweep: grid produced no combinations")
	}
	out := make([]SweepResult, 0, len(combos))
	for i, c := range combos {
		cfg := base
		cfg.Weights = c.W
		cfg.AffWeights = c.A
		summary, _, err := Run(ctx, ds, cfg, client)
		if err != nil {
			return nil, fmt.Errorf("sweep cell %d: %w", i, err)
		}
		out = append(out, SweepResult{Weights: c.W, AffWeights: c.A, Summary: summary})
	}
	sort.SliceStable(out, func(i, j int) bool { return metricFor(out[i].Summary, sortBy) > metricFor(out[j].Summary, sortBy) })
	return out, nil
}

type combo struct {
	W Weights
	A AffWeights
}

func expand(g Grid, base Config) []combo {
	alpha := nonEmpty(g.Alpha, base.Weights.Alpha)
	beta := nonEmpty(g.Beta, base.Weights.Beta)
	gamma := nonEmpty(g.Gamma, base.Weights.Gamma)
	lambda := nonEmpty(g.Lambda, base.Weights.Lambda)
	qrel := nonEmpty(g.QRel, base.AffWeights.QRel)
	fskill := nonEmpty(g.FSkill, base.AffWeights.FSkill)
	rsel := nonEmpty(g.RSel, base.AffWeights.RSel)
	gsame := nonEmpty(g.GSame, base.AffWeights.GSame)
	gactive := nonEmpty(g.GActive, base.AffWeights.GActive)

	var out []combo
	for _, a := range alpha {
		for _, b := range beta {
			for _, c := range gamma {
				for _, l := range lambda {
					for _, qr := range qrel {
						for _, fs := range fskill {
							for _, rs := range rsel {
								for _, gs := range gsame {
									for _, ga := range gactive {
										out = append(out, combo{
											W: Weights{Alpha: a, Beta: b, Gamma: c, Lambda: l},
											A: AffWeights{QRel: qr, FSkill: fs, RSel: rs, GSame: gs, GActive: ga},
										})
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return out
}

func nonEmpty(vs []float64, fallback float64) []float64 {
	if len(vs) == 0 {
		return []float64{fallback}
	}
	return vs
}

func metricFor(s RunSummary, key SortKey) float64 {
	switch key {
	case SortByRecall:
		return s.MeanRecallAtK
	case SortByNDCG:
		return s.MeanNDCG
	case SortByMAP:
		return s.MeanAP
	case SortByHit1:
		return s.HitRateAt1
	case SortByMRR, "":
		return s.MeanMRR
	}
	return s.MeanMRR
}

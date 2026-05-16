package bench

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Run executes one Config against one Dataset and returns the
// aggregate summary plus per-item results. Concurrency defaults to 4;
// pass cfg.Concurrency to override.
//
// Errors at the item level are recorded on the QueryResult but never
// abort the run — a wedged item just contributes Error+0 metrics. The
// caller can filter on r.Error to surface failures in the report.
func Run(ctx context.Context, ds *Dataset, cfg Config, client *Client) (RunSummary, []QueryResult, error) {
	if ds == nil || len(ds.Items) == 0 {
		return RunSummary{}, nil, errors.New("run: empty dataset")
	}
	if client == nil {
		return RunSummary{}, nil, errors.New("run: nil client")
	}
	if cfg.TopK <= 0 {
		cfg.TopK = 5
	}
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 4
	}
	if conc > len(ds.Items) {
		conc = len(ds.Items)
	}

	summary := RunSummary{
		Dataset:   ds.Name,
		Config:    cfg,
		N:         len(ds.Items),
		StartedAt: time.Now(),
	}
	results := make([]QueryResult, len(ds.Items))

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range ds.Items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = runOne(ctx, ds.Items[i], cfg, client)
		}(i)
	}
	wg.Wait()
	summary.EndedAt = time.Now()

	aggregate(&summary, results)
	return summary, results, nil
}

// runOne does one item end-to-end: HTTP call + score it.
func runOne(ctx context.Context, item Item, cfg Config, client *Client) QueryResult {
	res := client.call(ctx, cfg, item)
	qr := QueryResult{
		Item:             item,
		ReturnedSkillIDs: res.SelectedIDs,
		ElapsedMs:        res.ElapsedMs,
		HTTPStatus:       res.HTTPStatus,
		BottomUpHits:     res.BottomUpHits,
		L1Hits:           res.L1Hits,
		CommLabels:       res.TopDownLabels,
	}
	if res.Err != nil {
		qr.Error = res.Err.Error()
		return qr
	}
	k := cfg.TopK
	if k <= 0 {
		k = 5
	}
	qr.HitAt1 = HitAtK(item.GoldSkillIDs, res.SelectedIDs, 1)
	qr.HitAtK = HitAtK(item.GoldSkillIDs, res.SelectedIDs, k)
	qr.RecallAtK = RecallAtK(item.GoldSkillIDs, res.SelectedIDs, k)
	qr.MRR = MRR(item.GoldSkillIDs, res.SelectedIDs)
	qr.NDCG = NDCGAtK(item.GoldSkillIDs, res.SelectedIDs, k)
	qr.AP = AveragePrecision(item.GoldSkillIDs, res.SelectedIDs)
	return qr
}

// aggregate fills the mean / percentile fields on summary. Skips
// rows with Error set so a single broken item doesn't poison the
// scoreboard.
func aggregate(summary *RunSummary, results []QueryResult) {
	var (
		latencies         []int64
		sumRecall, sumMRR float64
		sumNDCG, sumAP    float64
		hits1, hitsK      int
		ok                int
	)
	for _, r := range results {
		if r.Error != "" {
			summary.NErrors++
			continue
		}
		ok++
		latencies = append(latencies, r.ElapsedMs)
		sumRecall += r.RecallAtK
		sumMRR += r.MRR
		sumNDCG += r.NDCG
		sumAP += r.AP
		hits1 += r.HitAt1
		hitsK += r.HitAtK
	}
	if ok == 0 {
		return
	}
	d := float64(ok)
	summary.MeanRecallAtK = sumRecall / d
	summary.MeanMRR = sumMRR / d
	summary.MeanNDCG = sumNDCG / d
	summary.MeanAP = sumAP / d
	summary.HitRateAt1 = float64(hits1) / d
	summary.HitRateAtK = float64(hitsK) / d
	summary.MeanLatencyMs = MeanInt64(latencies)
	summary.P50LatencyMs = Percentile(latencies, 50)
	summary.P95LatencyMs = Percentile(latencies, 95)
	summary.P99LatencyMs = Percentile(latencies, 99)
}

// SortResultsByID returns results sorted by item id. Convenient for
// stable diff-friendly JSONL output.
func SortResultsByID(results []QueryResult) []QueryResult {
	cp := make([]QueryResult, len(results))
	copy(cp, results)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].Item.ID < cp[j].Item.ID })
	return cp
}

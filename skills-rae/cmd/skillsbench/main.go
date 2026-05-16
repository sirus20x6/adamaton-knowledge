// Command skillsbench is the SkillRAE Phase G eval harness CLI.
//
// Three subcommands:
//
//	skillsbench run        — score one dataset against /v1/rae/compile.
//	skillsbench self-gold  — generate a synthetic dataset from evo.skills.
//	skillsbench sweep      — Cartesian-product weight grid; rank cells.
//
// All three speak pure HTTP to a running skills-rae instance — no
// shared in-process state, so the binary works against the Pi
// production service or a local instance with equal facility.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirus20x6/adamaton-knowledge/skills-rae/internal/bench"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "skillsbench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(rest)
	case "self-gold":
		return cmdSelfGold(rest)
	case "sweep":
		return cmdSweep(rest)
	case "compare":
		return cmdCompare(rest)
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown subcommand %q (try: run | self-gold | sweep | compare)", cmd)
	}
}

func usage() error {
	fmt.Fprintln(os.Stderr, `skillsbench — SkillRAE eval harness

usage:
  skillsbench run        --dataset PATH --base-url URL [flags]
  skillsbench self-gold  --dsn DSN --out PATH [--n N] [--mode name|description|both] [--seed S]
  skillsbench sweep      --dataset PATH --base-url URL --grid GRID.json [flags]
  skillsbench compare    --baseline summary.json --candidate summary.json --label TEXT

common flags for run/sweep:
  --top-k INT             retrieval K (default 5)
  --concurrency INT       parallel requests (default 4)
  --timeout DURATION      per-request HTTP timeout (default 30s)
  --endpoint compile|search   which RAE endpoint to hit (default compile)
  --report PATH           markdown report destination (default stdout)
  --results-jsonl PATH    per-item JSONL output (optional)
  --summary-json PATH     summary JSON output (optional)
  --alpha/--beta/--gamma/--lambda FLOAT   retrieval weights
  --q-rel/--f-skill/--r-sel/--g-same/--g-active FLOAT   compile (Aff) weights
  --budget-tokens INT     compile token budget (passed through)
  --output-contract TEXT  compile output contract`)
	return nil
}

// ----- run -----

type commonFlags struct {
	dataset        string
	baseURL        string
	topK           int
	concurrency    int
	timeout        time.Duration
	endpoint       string
	reportPath     string
	resultsPath    string
	summaryPath    string
	weights        bench.Weights
	aff            bench.AffWeights
	budgetTokens   int
	outputContract string
}

func bindCommonFlags(fs *flag.FlagSet, c *commonFlags) {
	fs.StringVar(&c.dataset, "dataset", "", "path to JSONL dataset (required)")
	fs.StringVar(&c.baseURL, "base-url", "http://localhost:7376", "skills-rae base URL")
	fs.IntVar(&c.topK, "top-k", 5, "retrieval K")
	fs.IntVar(&c.concurrency, "concurrency", 4, "parallel requests")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "per-request HTTP timeout")
	fs.StringVar(&c.endpoint, "endpoint", "compile", "compile|search")
	fs.StringVar(&c.reportPath, "report", "", "markdown report destination (default stdout)")
	fs.StringVar(&c.resultsPath, "results-jsonl", "", "per-item JSONL output (optional)")
	fs.StringVar(&c.summaryPath, "summary-json", "", "summary JSON output (optional)")
	fs.Float64Var(&c.weights.Alpha, "alpha", 0, "retrieval α (ℓ₁ weight); 0 = service default")
	fs.Float64Var(&c.weights.Beta, "beta", 0, "retrieval β (ℓ₀ weight)")
	fs.Float64Var(&c.weights.Gamma, "gamma", 0, "retrieval γ (name-score weight)")
	fs.Float64Var(&c.weights.Lambda, "lambda", 0, "retrieval λ (community boost)")
	fs.Float64Var(&c.aff.QRel, "q-rel", 0, "compile Aff q_rel weight")
	fs.Float64Var(&c.aff.FSkill, "f-skill", 0, "compile Aff f_skill weight")
	fs.Float64Var(&c.aff.RSel, "r-sel", 0, "compile Aff r_sel weight")
	fs.Float64Var(&c.aff.GSame, "g-same", 0, "compile Aff g_same weight")
	fs.Float64Var(&c.aff.GActive, "g-active", 0, "compile Aff g_active weight")
	fs.IntVar(&c.budgetTokens, "budget-tokens", 0, "compile token budget (0 = service default)")
	fs.StringVar(&c.outputContract, "output-contract", "", "compile output contract")
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var c commonFlags
	bindCommonFlags(fs, &c)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.dataset == "" {
		return errors.New("--dataset is required")
	}
	ds, err := bench.LoadJSONL(c.dataset)
	if err != nil {
		return err
	}
	cfg := bench.Config{
		BaseURL:        c.baseURL,
		TopK:           c.topK,
		Concurrency:    c.concurrency,
		HTTPTimeout:    c.timeout,
		Endpoint:       c.endpoint,
		Weights:        c.weights,
		AffWeights:     c.aff,
		BudgetTokens:   c.budgetTokens,
		OutputContract: c.outputContract,
	}
	client := bench.NewClient(c.baseURL, c.timeout)
	client.Endpoint = c.endpoint

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	summary, results, err := bench.Run(ctx, ds, cfg, client)
	if err != nil {
		return err
	}
	if err := emitOutputs(summary, results, c); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "OK — %d items, hit@1=%.3f recall@%d=%.3f mrr=%.3f p50=%dms\n",
		summary.N, summary.HitRateAt1, c.topK, summary.MeanRecallAtK, summary.MeanMRR, summary.P50LatencyMs)
	return nil
}

func emitOutputs(summary bench.RunSummary, results []bench.QueryResult, c commonFlags) error {
	out := os.Stdout
	if c.reportPath != "" {
		f, err := os.Create(c.reportPath)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	if err := bench.WriteMarkdown(out, summary, results); err != nil {
		return err
	}
	if c.resultsPath != "" {
		f, err := os.Create(c.resultsPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := bench.WriteResultsJSONL(f, results); err != nil {
			return err
		}
	}
	if c.summaryPath != "" {
		f, err := os.Create(c.summaryPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := bench.WriteSummaryJSON(f, summary); err != nil {
			return err
		}
	}
	return nil
}

// ----- self-gold -----

func cmdSelfGold(args []string) error {
	fs := flag.NewFlagSet("self-gold", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "Postgres DSN (or set SKILLS_RAE_DSN)")
	outPath := fs.String("out", "", "destination JSONL path (required)")
	n := fs.Int("n", 50, "sample size")
	mode := fs.String("mode", "name", "name|description|both")
	seed := fs.Int64("seed", 1, "sampling seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outPath == "" {
		return errors.New("--out is required")
	}
	if *dsn == "" {
		*dsn = os.Getenv("SKILLS_RAE_DSN")
	}
	if *dsn == "" {
		return errors.New("--dsn or SKILLS_RAE_DSN required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("postgres connect: %w", err)
	}
	defer pool.Close()

	ds, err := bench.SelfGoldFromDB(ctx, pool, bench.SelfGoldOpts{N: *n, Mode: *mode, Seed: *seed})
	if err != nil {
		return err
	}
	f, err := os.Create(*outPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := bench.WriteJSONL(f, ds.Items); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d items to %s (mode=%s seed=%d)\n", len(ds.Items), *outPath, *mode, *seed)
	return nil
}

// ----- sweep -----

func cmdSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	var c commonFlags
	bindCommonFlags(fs, &c)
	gridPath := fs.String("grid", "", "JSON grid file (required)")
	sortBy := fs.String("sort-by", "mrr", "mrr|recall|ndcg|map|hit_at_1")
	topN := fs.Int("top-n", 0, "if >0, only emit the top-N cells in the report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if c.dataset == "" || *gridPath == "" {
		return errors.New("--dataset and --grid are required")
	}
	ds, err := bench.LoadJSONL(c.dataset)
	if err != nil {
		return err
	}
	gridBytes, err := os.ReadFile(*gridPath)
	if err != nil {
		return fmt.Errorf("read grid: %w", err)
	}
	var grid bench.Grid
	if err := json.Unmarshal(gridBytes, &grid); err != nil {
		return fmt.Errorf("parse grid: %w", err)
	}

	base := bench.Config{
		BaseURL:        c.baseURL,
		TopK:           c.topK,
		Concurrency:    c.concurrency,
		HTTPTimeout:    c.timeout,
		Endpoint:       c.endpoint,
		Weights:        c.weights,
		AffWeights:     c.aff,
		BudgetTokens:   c.budgetTokens,
		OutputContract: c.outputContract,
	}
	client := bench.NewClient(c.baseURL, c.timeout)
	client.Endpoint = c.endpoint

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sweep, err := bench.Sweep(ctx, ds, base, grid, client, bench.SortKey(strings.ToLower(*sortBy)))
	if err != nil {
		return err
	}
	if *topN > 0 && *topN < len(sweep) {
		sweep = sweep[:*topN]
	}
	out := os.Stdout
	if c.reportPath != "" {
		f, err := os.Create(c.reportPath)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	if err := bench.WriteSweepMarkdown(out, ds.Name, bench.SortKey(strings.ToLower(*sortBy)), sweep); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "OK — %d cells; best %s=%.4f (α=%.2f β=%.2f γ=%.2f λ=%.2f)\n",
		len(sweep), *sortBy,
		topMetric(sweep, bench.SortKey(strings.ToLower(*sortBy))),
		sweep[0].Weights.Alpha, sweep[0].Weights.Beta, sweep[0].Weights.Gamma, sweep[0].Weights.Lambda)
	return nil
}

func topMetric(rs []bench.SweepResult, key bench.SortKey) float64 {
	if len(rs) == 0 {
		return 0
	}
	switch key {
	case bench.SortByRecall:
		return rs[0].Summary.MeanRecallAtK
	case bench.SortByNDCG:
		return rs[0].Summary.MeanNDCG
	case bench.SortByMAP:
		return rs[0].Summary.MeanAP
	case bench.SortByHit1:
		return rs[0].Summary.HitRateAt1
	}
	return rs[0].Summary.MeanMRR
}

// ----- compare -----

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	baseline := fs.String("baseline", "", "summary.json from the baseline run (required)")
	candidate := fs.String("candidate", "", "summary.json from the candidate run (required)")
	label := fs.String("label", "candidate vs baseline", "headline for the report section")
	report := fs.String("report", "", "markdown report destination (default stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *baseline == "" || *candidate == "" {
		return errors.New("--baseline and --candidate are required")
	}
	b, err := loadSummary(*baseline)
	if err != nil {
		return err
	}
	cnd, err := loadSummary(*candidate)
	if err != nil {
		return err
	}
	out := os.Stdout
	if *report != "" {
		f, err := os.Create(*report)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	return bench.CompareRuns(out, *label, b, cnd)
}

func loadSummary(path string) (bench.RunSummary, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return bench.RunSummary{}, fmt.Errorf("read %s: %w", path, err)
	}
	var s bench.RunSummary
	if err := json.Unmarshal(b, &s); err != nil {
		return bench.RunSummary{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

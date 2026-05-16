package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadJSONL reads a JSONL file where each line is a bench.Item. Blank
// lines and lines starting with '#' are ignored so authors can leave
// comments in seed files.
func LoadJSONL(path string) (*Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	ds := &Dataset{Name: name}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var it Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			return nil, fmt.Errorf("%s:%d invalid json: %w", path, lineno, err)
		}
		if it.ID == "" {
			it.ID = fmt.Sprintf("%s#%d", name, lineno)
		}
		if strings.TrimSpace(it.Query) == "" {
			return nil, fmt.Errorf("%s:%d item has empty query", path, lineno)
		}
		ds.Items = append(ds.Items, it)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	if len(ds.Items) == 0 {
		return nil, fmt.Errorf("%s: no items loaded", path)
	}
	return ds, nil
}

// WriteJSONL writes one Item per line. Used by the self-gold generator
// and by anyone exporting per-item results.
func WriteJSONL(w io.Writer, items []Item) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	for _, it := range items {
		b, err := json.Marshal(it)
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

// SelfGoldOpts controls SelfGoldFromDB.
type SelfGoldOpts struct {
	// N is the number of items to generate. Capped at the corpus size.
	N int
	// Mode is one of "name", "description", "both". "name" uses the
	// skill name as the query (easy baseline). "description" uses the
	// first sentence of the description (slightly harder). "both"
	// emits one item per skill per mode.
	Mode string
	// Seed makes sampling deterministic.
	Seed int64
}

// SelfGoldFromDB samples skills from the live evo.skills table and
// emits a synthetic dataset where each query is the skill's name or
// the first sentence of its description, and the gold set is just
// that skill's UUID. The point of self-gold isn't to measure absolute
// quality — it's a regression smoke: if Recall@5 on self-gold drops
// from ~0.95 to ~0.6 after a code change, retrieval broke.
func SelfGoldFromDB(ctx context.Context, pool *pgxpool.Pool, opts SelfGoldOpts) (*Dataset, error) {
	if pool == nil {
		return nil, errors.New("self-gold: nil pool")
	}
	n := opts.N
	if n <= 0 {
		n = 50
	}
	mode := opts.Mode
	if mode == "" {
		mode = "name"
	}
	switch mode {
	case "name", "description", "both":
	default:
		return nil, fmt.Errorf("self-gold: unknown mode %q (want name|description|both)", mode)
	}
	// Deterministic sample via setseed/random — keeps multi-pass runs
	// reproducible and matches the way r2g's reindexer seeds work.
	if _, err := pool.Exec(ctx, "SELECT setseed($1)", float64(opts.Seed%1000)/1000.0); err != nil {
		return nil, fmt.Errorf("self-gold setseed: %w", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT id, name, COALESCE(description, '')
		  FROM evo.skills
		 WHERE rae_indexed_at IS NOT NULL
		 ORDER BY random()
		 LIMIT $1`, n)
	if err != nil {
		return nil, fmt.Errorf("self-gold query: %w", err)
	}
	defer rows.Close()
	var skills []struct {
		ID          uuid.UUID
		Name        string
		Description string
	}
	for rows.Next() {
		var s struct {
			ID          uuid.UUID
			Name        string
			Description string
		}
		if err := rows.Scan(&s.ID, &s.Name, &s.Description); err != nil {
			return nil, err
		}
		skills = append(skills, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(skills) == 0 {
		return nil, errors.New("self-gold: no indexed skills (evo.skills with rae_indexed_at IS NOT NULL)")
	}

	ds := &Dataset{Name: fmt.Sprintf("self-gold-%s-%d", mode, opts.Seed)}
	for _, s := range skills {
		if mode == "name" || mode == "both" {
			ds.Items = append(ds.Items, Item{
				ID:           fmt.Sprintf("name:%s", s.ID),
				Query:        s.Name,
				GoldSkillIDs: []uuid.UUID{s.ID},
				Notes:        "self-gold: query = skill name",
			})
		}
		if mode == "description" || mode == "both" {
			q := firstSentence(s.Description)
			if q == "" {
				q = s.Name
			}
			ds.Items = append(ds.Items, Item{
				ID:           fmt.Sprintf("desc:%s", s.ID),
				Query:        q,
				GoldSkillIDs: []uuid.UUID{s.ID},
				Notes:        "self-gold: query = first sentence of description",
			})
		}
	}
	return ds, nil
}

// firstSentence returns text up to (and not including) the first
// sentence terminator, or the first 160 characters if no terminator
// is found. Avoids feeding the retriever a 4-paragraph description
// when we want a search-query-sized prompt.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for i, r := range s {
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			out := strings.TrimSpace(s[:i])
			if len(out) >= 8 {
				return out
			}
		}
	}
	if len(s) > 160 {
		return strings.TrimSpace(s[:160])
	}
	return s
}

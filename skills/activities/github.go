// Package activities holds Temporal activities for the skills subsystem.
// GithubActivities ports the synchronous GitHub importer from
// dashboard/apiserver/skills_importers.go into the workflow world:
// each step is its own activity so the workflow can checkpoint between
// them, rate limits raise retryable errors with a precise NextRetryDelay,
// and one bad file no longer kills the whole import.
package activities

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"gopkg.in/yaml.v3"

	skillsworkflows "github.com/sirus20x6/adamomaton-knowledge/skills/workflows"
)

// GithubActivities groups the GitHub importer activities. Constructed
// once at worker startup and registered on the "skills" task queue.
type GithubActivities struct {
	HTTPClient *http.Client
	Pool       *pgxpool.Pool
	Logger     *logrus.Logger
}

// ---------------- WalkGithubRepo ----------------

// githubContent mirrors the relevant slice of GitHub's contents API.
type githubContent struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"` // "file" | "dir"
	DownloadURL string `json:"download_url"`
}

// WalkGithubRepo walks a github.com/<owner>/<repo>/<subpath> subtree
// and returns the list of raw download URLs for every .md / .markdown
// file found. Caps depth + file count exactly like the synchronous
// importer (see dashboard/apiserver/skills_importers.go:importGithubRepo).
//
// Heartbeats every 10 files so a worker restart mid-walk doesn't look
// like a stuck activity.
func (a *GithubActivities) WalkGithubRepo(ctx context.Context, in skillsworkflows.WalkInput) ([]string, error) {
	owner, repo, err := parseGithubRepoURL(in.URL)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(
			err.Error(), "BadRepoURL", err,
		)
	}
	subpath := strings.Trim(in.Subpath, "/")
	if subpath == "" {
		subpath = "skills"
	}
	ref := "HEAD"

	maxFiles := in.MaxFiles
	if maxFiles <= 0 || maxFiles > 2500 {
		maxFiles = 2500
	}
	const maxDepth = 4

	results := make([]string, 0, 16)
	var walk func(path string, depth int) error
	walk = func(path string, depth int) error {
		if depth > maxDepth {
			return nil
		}
		if len(results) >= maxFiles {
			return nil
		}
		entries, err := a.listContents(ctx, owner, repo, path, ref)
		if err != nil {
			return err
		}
		mdFiles := make([]githubContent, 0)
		subdirs := make([]githubContent, 0)
		for _, e := range entries {
			switch e.Type {
			case "file":
				ext := strings.ToLower(filepath.Ext(e.Name))
				if (ext == ".md" || ext == ".markdown") && e.DownloadURL != "" {
					mdFiles = append(mdFiles, e)
				}
			case "dir":
				subdirs = append(subdirs, e)
			}
		}
		for _, e := range mdFiles {
			if len(results) >= maxFiles {
				return nil
			}
			results = append(results, e.DownloadURL)
			if len(results)%10 == 0 {
				activity.RecordHeartbeat(ctx, len(results))
			}
		}
		// "Skill bundle" heuristic: if this is a non-root directory and
		// it contained .md files, treat sibling subdirs as auxiliary
		// (scripts/templates) rather than nested skills.
		if depth == 0 || len(mdFiles) == 0 {
			for _, e := range subdirs {
				if len(results) >= maxFiles {
					return nil
				}
				if err := walk(e.Path, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(subpath, 0); err != nil {
		return results, err
	}
	activity.RecordHeartbeat(ctx, len(results))
	return results, nil
}

// listContents calls the GitHub contents API for one directory or file
// and unmarshals the response. Handles both the directory shape (array)
// and the single-file shape (object).
func (a *GithubActivities) listContents(ctx context.Context, owner, repo, path, ref string) ([]githubContent, error) {
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s",
		url.PathEscape(owner), url.PathEscape(repo), path)
	if ref != "" && ref != "HEAD" {
		api += "?ref=" + url.QueryEscape(ref)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return nil, err
	}
	addGithubAuth(req)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", api, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if rlErr := buildRateLimitError(resp); rlErr != nil {
			return nil, rlErr
		}
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode != http.StatusOK {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		// 404 = bad subpath; permanent.
		if resp.StatusCode == http.StatusNotFound {
			return nil, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("HTTP 404 from %s: %s", api, preview),
				"NotFound", nil,
			)
		}
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, api, preview)
	}

	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []githubContent
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("parse json: %w", err)
		}
		return arr, nil
	}
	var single githubContent
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	return []githubContent{single}, nil
}

// ---------------- FetchGithubFile ----------------

// FetchGithubFile fetches a single markdown URL, parses its frontmatter,
// and returns a workflows.Skill ready for upsert. The ID field is left
// empty — UpsertSkill assigns it based on dedup state.
//
// Accepts both raw.githubusercontent.com URLs and the regular
// github.com/<owner>/<repo>/blob/<ref>/<path> browser form (normalised
// to raw before fetching). Rate limits raise a retryable error with the
// reset delta encoded as NextRetryDelay.
func (a *GithubActivities) FetchGithubFile(ctx context.Context, rawURL string) (*skillsworkflows.Skill, error) {
	resolved, err := normaliseGithubRawURL(rawURL)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(
			err.Error(), "BadURL", err,
		)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", resolved, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	addGithubAuth(req)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", resolved, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if rlErr := buildRateLimitError(resp); rlErr != nil {
			return nil, rlErr
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("HTTP 404 from %s: %s", resolved, string(body)),
			"NotFound", nil,
		)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, resolved, string(body))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	parsed, err := parseSkillMarkdown(raw, pathStem(resolved), resolved)
	if err != nil {
		return nil, temporal.NewNonRetryableApplicationError(
			err.Error(), "ParseError", err,
		)
	}
	return parsed, nil
}

// ---------------- UpsertSkill ----------------

// UpsertSkill applies a parsed Skill to evo.skills, mirroring the dedup
// logic in dashboard/apiserver/skills_import_handlers.go:applyImportedSkill.
// Returns one of "imported" | "updated" | "unchanged". Name collisions
// against a different body raise a NonRetryable error so the workflow
// counts it as a bad file and moves on.
//
// The lookup-then-update / lookup-then-insert sequence runs inside a
// single ReadCommitted transaction so a concurrent run of the same
// activity (Temporal can re-enqueue an activity that timed out while
// the DB write was still in flight) doesn't observe a half-applied
// state. ReadCommitted is enough — we only care about row-level
// atomicity, not snapshot isolation across the lookup ladder.
func (a *GithubActivities) UpsertSkill(ctx context.Context, in skillsworkflows.UpsertInput) (string, error) {
	if a.Pool == nil {
		return "", temporal.NewNonRetryableApplicationError(
			"postgres pool not configured", "ConfigError", nil,
		)
	}
	sk := in.Skill

	// Overlay caller-provided defaults.
	if (sk.Community == nil || *sk.Community == "") && in.Community != "" {
		c := in.Community
		sk.Community = &c
	}
	sk.Tags = mergeTags(sk.Tags, in.Tags)
	if sk.Tags == nil {
		sk.Tags = []string{}
	}
	if sk.Origin == "" {
		if in.Origin != "" {
			sk.Origin = in.Origin
		} else {
			sk.Origin = "github"
		}
	}
	dependsOn := []string{}

	tx, err := a.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	// Safe to call after Commit — pgx documents Rollback as a no-op
	// once the tx is committed/rolled-back.
	defer func() { _ = tx.Rollback(ctx) }()

	// 1) Lookup by source_url.
	var (
		existingID  string
		existingSHA *string
		haveRow     bool
	)
	if sk.SourceURL != nil && *sk.SourceURL != "" {
		err := tx.QueryRow(ctx, `
			SELECT id, source_sha
			FROM evo.skills
			WHERE source_url = $1
			ORDER BY updated_at DESC
			LIMIT 1
		`, *sk.SourceURL).Scan(&existingID, &existingSHA)
		switch {
		case err == nil:
			haveRow = true
		case errors.Is(err, pgx.ErrNoRows):
			// not found, fall through
		default:
			return "", fmt.Errorf("lookup by url: %w", err)
		}
	}

	nowChecked := time.Now()

	if haveRow {
		// 2) Found by URL. SHA match → unchanged; else UPDATE.
		if existingSHA != nil && sk.SourceSHA != nil && *existingSHA == *sk.SourceSHA {
			if err := tx.Commit(ctx); err != nil {
				return "", fmt.Errorf("commit unchanged: %w", err)
			}
			return "unchanged", nil
		}
		_, err := tx.Exec(ctx, `
			UPDATE evo.skills SET
			  description       = $1,
			  body              = $2,
			  when_to_use       = $3,
			  example           = $4,
			  community         = COALESCE(NULLIF($5, ''), community),
			  tags              = $6,
			  source_sha        = $7,
			  source_checked_at = $8,
			  updated_at        = NOW()
			WHERE id = $9
		`,
			sk.Description,
			sk.Body,
			nullableP(sk.WhenToUse),
			nullableP(sk.Example),
			derefOrEmpty(sk.Community),
			sk.Tags,
			derefOrEmpty(sk.SourceSHA),
			nowChecked,
			existingID,
		)
		if err != nil {
			return "", fmt.Errorf("update: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("commit update: %w", err)
		}
		return "updated", nil
	}

	// 3) Fresh INSERT path.
	_, err = tx.Exec(ctx, `
		INSERT INTO evo.skills
		  (name, description, body, when_to_use, example, community,
		   tags, depends_on, origin, source_url, source_sha, source_checked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`,
		sk.Name,
		sk.Description,
		sk.Body,
		nullableP(sk.WhenToUse),
		nullableP(sk.Example),
		nullableP(sk.Community),
		sk.Tags,
		dependsOn,
		sk.Origin,
		nullableP(sk.SourceURL),
		derefOrEmpty(sk.SourceSHA),
		nowChecked,
	)
	if err == nil {
		if cerr := tx.Commit(ctx); cerr != nil {
			return "", fmt.Errorf("commit insert: %w", cerr)
		}
		return "imported", nil
	}
	if !isUniqueViolation(err) {
		return "", fmt.Errorf("insert: %w", err)
	}

	// 4) Disambiguate the UNIQUE collision. Postgres aborts the
	// transaction on a constraint violation, so the disambiguation
	// query needs its own connection — roll back the failed tx first.
	_ = tx.Rollback(ctx)

	var (
		conflictID   string
		conflictName string
		conflictSHA  *string
	)
	qerr := a.Pool.QueryRow(ctx, `
		SELECT id, name, source_sha
		FROM evo.skills
		WHERE source_sha = $1 OR name = $2
		ORDER BY (source_sha = $1) DESC, updated_at DESC
		LIMIT 1
	`, derefOrEmpty(sk.SourceSHA), sk.Name).Scan(&conflictID, &conflictName, &conflictSHA)
	if qerr != nil {
		return "", fmt.Errorf("insert collision but could not identify cause: %w (original %v)", qerr, err)
	}
	if conflictSHA != nil && sk.SourceSHA != nil && *conflictSHA == *sk.SourceSHA {
		return "unchanged", nil
	}
	return "", temporal.NewNonRetryableApplicationError(
		fmt.Sprintf("a different skill already exists with name %q (id %s)", conflictName, conflictID),
		"NameCollision", nil,
	)
}

// ---------------- helpers ----------------

func (a *GithubActivities) client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// buildRateLimitError reads X-RateLimit-Remaining + X-RateLimit-Reset
// and, when the response really is a rate limit (remaining == 0 OR the
// 403 body mentions "rate limit"), returns a retryable application
// error with the reset delta encoded as NextRetryDelay so Temporal
// sleeps until the bucket refills.
func buildRateLimitError(resp *http.Response) error {
	remaining := resp.Header.Get("X-RateLimit-Remaining")
	resetHeader := resp.Header.Get("X-RateLimit-Reset")
	// Best-effort body sniff to distinguish quota-403 from auth-403.
	bodyPeek, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	isRateLimit := strings.EqualFold(remaining, "0") ||
		bytes.Contains(bytes.ToLower(bodyPeek), []byte("rate limit")) ||
		bytes.Contains(bytes.ToLower(bodyPeek), []byte("api rate limit exceeded")) ||
		resp.StatusCode == http.StatusTooManyRequests
	if !isRateLimit {
		// Some other 403 (auth, abuse detection without rate-limit
		// header). Make the caller surface it as a regular error.
		// Reset the body so the caller's later io.ReadAll still works.
		resp.Body = io.NopCloser(bytes.NewReader(bodyPeek))
		return nil
	}
	var delay time.Duration
	if reset, err := strconv.ParseInt(resetHeader, 10, 64); err == nil && reset > 0 {
		resetAt := time.Unix(reset, 0)
		delay = time.Until(resetAt)
		if delay < 0 {
			delay = 0
		}
	}
	if delay <= 0 {
		// Default to 60s if the header was missing/stale.
		delay = 60 * time.Second
	}
	// Cap at 1 hour — anything beyond that and the user should re-trigger.
	if delay > time.Hour {
		delay = time.Hour
	}
	return temporal.NewApplicationErrorWithOptions(
		fmt.Sprintf("github rate limit: retry after %s", delay),
		"RateLimited",
		temporal.ApplicationErrorOptions{NextRetryDelay: delay},
	)
}

// parseSkillMarkdown is the workflow-side port of the dashboard's
// parser. Produces a workflows.Skill (ID left empty) with frontmatter
// applied and source_sha set to sha256(raw).
func parseSkillMarkdown(raw []byte, defaultName, sourceURL string) (*skillsworkflows.Skill, error) {
	sha := sha256Hex(raw)
	out := &skillsworkflows.Skill{
		SourceURL: &sourceURL,
		SourceSHA: &sha,
		Tags:      []string{},
		DependsOn: []string{},
	}

	trimmed := bytes.TrimLeft(raw, "\xef\xbb\xbf \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		out.Body = string(raw)
		out.Name = strings.TrimSpace(defaultName)
		out.Description = firstNonBlankLine(out.Body)
		return finalize(out)
	}

	rest := trimmed[3:]
	rest = bytes.TrimLeft(rest, "\r\n")
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return out, errors.New("frontmatter open fence but no closing ---")
	}
	yamlBlock := rest[:end]
	body := rest[end+len("\n---"):]
	body = bytes.TrimLeft(body, "\r\n")

	type frontmatter struct {
		Name        string    `yaml:"name"`
		Description string    `yaml:"description"`
		WhenToUse   string    `yaml:"when_to_use"`
		Example     string    `yaml:"example"`
		Community   string    `yaml:"community"`
		Tags        yaml.Node `yaml:"tags"`
	}
	var fm frontmatter
	if err := yaml.Unmarshal(yamlBlock, &fm); err != nil {
		return out, fmt.Errorf("frontmatter yaml: %w", err)
	}

	out.Name = strings.TrimSpace(fm.Name)
	if out.Name == "" {
		out.Name = strings.TrimSpace(defaultName)
	}
	out.Description = strings.TrimSpace(fm.Description)
	if out.Description == "" {
		out.Description = firstNonBlankLine(string(body))
	}
	if wt := strings.TrimSpace(fm.WhenToUse); wt != "" {
		out.WhenToUse = &wt
	}
	if ex := strings.TrimSpace(fm.Example); ex != "" {
		out.Example = &ex
	}
	if cm := strings.TrimSpace(fm.Community); cm != "" {
		out.Community = &cm
	}
	out.Tags = tagsFromNode(fm.Tags)
	out.Body = string(body)
	return finalize(out)
}

func finalize(p *skillsworkflows.Skill) (*skillsworkflows.Skill, error) {
	if p.Name == "" {
		return p, errors.New("skill name is required (set frontmatter `name:` or rename the file)")
	}
	if p.Description == "" {
		return p, errors.New("skill description is required (set frontmatter `description:` or add a first paragraph)")
	}
	if strings.TrimSpace(p.Body) == "" {
		return p, errors.New("skill body is empty")
	}
	return p, nil
}

func tagsFromNode(n yaml.Node) []string {
	switch n.Kind {
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				t := strings.TrimSpace(c.Value)
				if t != "" {
					out = append(out, t)
				}
			}
		}
		return out
	case yaml.ScalarNode:
		out := make([]string, 0)
		for _, t := range strings.Split(n.Value, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return []string{}
}

func firstNonBlankLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		ln = strings.TrimPrefix(ln, "> ")
		return ln
	}
	return ""
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func normaliseGithubRawURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	switch u.Host {
	case "raw.githubusercontent.com":
		return raw, nil
	case "github.com":
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 5)
		if len(parts) < 5 || parts[2] != "blob" {
			return "", fmt.Errorf("github URL must be github.com/<owner>/<repo>/blob/<ref>/<path>, got %s", u.Path)
		}
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
			parts[0], parts[1], parts[3], parts[4]), nil
	default:
		if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
			return raw, nil
		}
		return "", fmt.Errorf("unsupported URL %s", raw)
	}
}

func parseGithubRepoURL(raw string) (owner, repo string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", fmt.Errorf("parse url: %w", perr)
	}
	if u.Host != "github.com" {
		return "", "", fmt.Errorf("only github.com repos are supported (got %s)", u.Host)
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("URL must be github.com/<owner>/<repo>, got %s", u.Path)
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func pathStem(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	base := filepath.Base(s)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// addGithubAuth reads GITHUB_TOKEN at call time — the worker may have
// been started before the operator set it.
//
// Token attachment is gated on a hostname allowlist so a normaliseGithubRawURL
// that accidentally lets a third-party URL through (e.g. a future "raw" fork)
// cannot leak the bearer to that host. Accept header is always set —
// harmless on non-GitHub endpoints, helpful for the actual contents API.
func addGithubAuth(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json,application/vnd.github.raw")
	if req.URL == nil {
		return
	}
	host := strings.ToLower(req.URL.Hostname())
	switch host {
	case "github.com", "api.github.com",
		"raw.githubusercontent.com", "objects.githubusercontent.com":
		// allowed
	default:
		return
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}

func mergeTags(existing, extra []string) []string {
	if len(extra) == 0 {
		if existing == nil {
			return []string{}
		}
		return existing
	}
	seen := make(map[string]bool, len(existing)+len(extra))
	out := make([]string, 0, len(existing)+len(extra))
	for _, t := range existing {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range extra {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func nullableP(s *string) *string {
	if s == nil {
		return nil
	}
	if strings.TrimSpace(*s) == "" {
		return nil
	}
	return s
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

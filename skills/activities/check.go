package activities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// CheckActivities groups the activity methods that drive the daily
// check-source drift detector. Ported from
// dashboard/apiserver/skills_import_handlers.go:324-384 — the activities
// strip the HTTP-handler scaffolding and let the workflow orchestrate
// retry/timeout durability.
//
// Dependencies are injected once at worker startup so each invocation
// reuses the same Postgres pool and HTTP client rather than rebuilding
// them per call.
type CheckActivities struct {
	Pool       *pgxpool.Pool
	HTTPClient *http.Client
	Logger     *logrus.Logger
}

// SourceMeta is the slim subset of evo.skills the check workflow needs
// before deciding whether to re-fetch. Returned by LoadSkillSourceMeta.
type SourceMeta struct {
	SourceURL string `json:"source_url"`
	SourceSHA string `json:"source_sha"`
}

// RefetchResult is the activity result from refetching a skill's
// upstream source. Body is the raw bytes (decoded to string); NewSHA is
// sha256(body) hex-encoded — matching the bodySHA helper at
// dashboard/apiserver/skills_endpoints.go:439-442 so wire SHAs are
// comparable across the dashboard's synchronous import path and this
// async drift detector.
type RefetchResult struct {
	Body   string `json:"body"`
	NewSHA string `json:"new_sha"`
}

// LoadSkillSourceMeta reads source_url and source_sha for one skill.
// NULL columns come back as empty strings — the workflow treats an
// empty source_url as "skip, nothing to check".
func (a *CheckActivities) LoadSkillSourceMeta(ctx context.Context, skillID string) (*SourceMeta, error) {
	var url, sha *string
	row := a.Pool.QueryRow(ctx,
		`SELECT source_url, source_sha FROM evo.skills WHERE id = $1`, skillID)
	if err := row.Scan(&url, &sha); err != nil {
		return nil, fmt.Errorf("load skill %s: %w", skillID, err)
	}
	out := &SourceMeta{}
	if url != nil {
		out.SourceURL = *url
	}
	if sha != nil {
		out.SourceSHA = *sha
	}
	return out, nil
}

// RefetchSource downloads the bytes at source_url and returns the body
// along with its sha256. Supports three schemes:
//
//   - file:// — local filesystem read (used during testing and for
//     skills imported from local Markdown directories).
//   - https:// / http:// — HTTP GET via the injected HTTPClient. When
//     the host is raw.githubusercontent.com or api.github.com and the
//     environment has GITHUB_TOKEN set, a Bearer auth header is added
//     to dodge the 60-req/hr unauthenticated rate limit.
//   - github:// — same as https but with the host normalised to raw.
//
// Body size is capped at 1 MiB (matches importGithubRawURL).
func (a *CheckActivities) RefetchSource(ctx context.Context, sourceURL string) (*RefetchResult, error) {
	source := strings.TrimSpace(sourceURL)
	if source == "" {
		return nil, fmt.Errorf("empty source_url")
	}

	switch {
	case strings.HasPrefix(source, "file://"):
		path := strings.TrimPrefix(source, "file://")
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		return resultFromBytes(raw), nil

	case strings.HasPrefix(source, "github://"):
		// github://<owner>/<repo>/<ref>/<path> — normalise to raw.
		rest := strings.TrimPrefix(source, "github://")
		return a.fetchHTTP(ctx, "https://raw.githubusercontent.com/"+rest)

	case strings.HasPrefix(source, "https://"), strings.HasPrefix(source, "http://"):
		return a.fetchHTTP(ctx, source)

	default:
		return nil, fmt.Errorf("unsupported source_url scheme: %s", source)
	}
}

// fetchHTTP performs the HTTP GET shared between the https and github
// branches above. Separated so the auth-header logic stays in one
// place.
func (a *CheckActivities) fetchHTTP(ctx context.Context, rawURL string) (*RefetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// GITHUB_TOKEN only attached when the host is a GitHub endpoint —
	// avoids leaking the bearer to a third-party forge.
	if u, perr := url.Parse(rawURL); perr == nil {
		host := strings.ToLower(u.Host)
		if host == "raw.githubusercontent.com" || host == "api.github.com" {
			if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
				req.Header.Set("Authorization", "Bearer "+t)
				if host == "api.github.com" {
					req.Header.Set("Accept", "application/vnd.github.v3.raw")
				}
			}
		}
	}

	client := a.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, rawURL, strings.TrimSpace(string(preview)))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return resultFromBytes(raw), nil
}

// MarkSourceChecked stamps source_checked_at on the skill row. Called
// unconditionally after every refetch — the column proves we looked
// even when the SHA was identical.
func (a *CheckActivities) MarkSourceChecked(ctx context.Context, skillID string) error {
	if _, err := a.Pool.Exec(ctx,
		`UPDATE evo.skills SET source_checked_at = NOW() WHERE id = $1`, skillID); err != nil {
		return fmt.Errorf("stamp source_checked_at on %s: %w", skillID, err)
	}
	return nil
}

// FlagDrift sets drift_detected = TRUE on the skill row. The
// 0003_skills_drift migration ensures the column exists; the partial
// index on (drift_detected = TRUE) means the UI's drift-list query is
// O(drifted), not O(all skills).
func (a *CheckActivities) FlagDrift(ctx context.Context, skillID string) error {
	if _, err := a.Pool.Exec(ctx,
		`UPDATE evo.skills SET drift_detected = TRUE WHERE id = $1`, skillID); err != nil {
		return fmt.Errorf("flag drift on %s: %w", skillID, err)
	}
	return nil
}

// QuerySkillsWithSourceURLs returns every skill ID whose source_url is
// non-null and non-empty, ordered deterministically. The batch
// workflow consumes the list to fan out child workflows.
func (a *CheckActivities) QuerySkillsWithSourceURLs(ctx context.Context) ([]string, error) {
	rows, err := a.Pool.Query(ctx,
		`SELECT id FROM evo.skills
		 WHERE source_url IS NOT NULL AND source_url <> ''
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query skills with source_urls: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate skills: %w", err)
	}
	return out, nil
}

// resultFromBytes wraps raw bytes into the RefetchResult shape with
// sha256 computed. Matches dashboard/apiserver/skills_endpoints.go:439-442.
func resultFromBytes(raw []byte) *RefetchResult {
	sum := sha256.Sum256(raw)
	return &RefetchResult{
		Body:   string(raw),
		NewSHA: hex.EncodeToString(sum[:]),
	}
}

// Package arxiv is the Go port of platform/backend/app/search/arxiv_fetcher.py.
// It implements the LaTeX-preferred fetch cascade used by /platform/sources/ingest.
//
// Cascade (in `prefer` order, default ["latex","ar5iv","vanity","pdf"]):
//
//	latex   ->  https://arxiv.org/e-print/{id}        (gzipped tar of TeX)
//	ar5iv   ->  https://ar5iv.labs.arxiv.org/html/{id}  (KaTeX HTML)
//	vanity  ->  https://www.arxiv-vanity.com/papers/{id}/  (older HTML fallback)
//	pdf     ->  https://arxiv.org/pdf/{id}.pdf
//
// One difference from the Python: the latex tier in Go does NOT run any
// local LaTeX-to-Markdown conversion. The cascade returns the raw
// tarball bytes; the downstream ingestpipeline runs r2g's latex2text
// subprocess against them. Cuts ~250 LOC of pandoc / latex2text-driver
// glue out of this package and drops the pandoc binary from r2g's
// deployment surface.
package arxiv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamomaton-knowledge/r2g/internal/tarwalk"
)

const userAgent = "deepresearch-platform/0.1.0 (+r2g)"

// MaxEprintBytes is a hard ceiling on tarball size. Anything bigger is
// almost certainly a misuse (a video paper with embedded clips, or a
// thesis dump). Matches the Python MAX_EPRINT_BYTES.
const MaxEprintBytes = 50 << 20 // 50 MiB

// DefaultPrefer is the cascade order when callers don't override. LaTeX
// first because the e-print archive preserves figure + section structure
// that r2g's latex2text turns into figure rows and chunk_kind-tagged
// chunks; the HTML tiers are flat fallbacks.
var DefaultPrefer = []string{"latex", "ar5iv", "vanity", "pdf"}

// FetchedDoc is the cascade's output. Exactly one tier wins per call.
// The platform's response includes a redacted version of this; see
// sourcesIngestHandler for the wire shape.
type FetchedDoc struct {
	Adapter     string            // always "arxiv"
	ExternalID  string            // arxiv id as passed in
	URL         string            // upstream URL we hit
	Title       string            // pulled from upstream, may be the id
	ContentType string            // "text/markdown" | "application/gzip" | "application/pdf"
	Body        []byte            // payload — for latex this is the tarball
	SourceTier  string            // "latex" | "ar5iv" | "vanity" | "pdf"
	Metadata    map[string]any    // tier-specific extras: main_tex, tex_files, length
}

// ErrAllTiersExhausted means every tier the caller asked for missed.
// The ingest handler maps this to 502 (upstream unreachable / no data).
var ErrAllTiersExhausted = errors.New("arxiv: all fetch tiers exhausted")

// ar5ivErrorSentinels mirrors _AR5IV_ERROR_SENTINELS in the Python.
var ar5ivErrorSentinels = []string{
	"could not be processed",
	"ar5iv-error",
	"Could not process",
}

// arxivIDRe is exported here too (regex shared with the route layer).
var arxivIDRe = regexp.MustCompile(`^[A-Za-z\-\.]+/\d{7}$|^\d{4}\.\d{4,5}(v\d+)?$`)

// Client wraps an http.Client with the user-agent and default timeout
// we want for all arxiv-side HTTP. Re-using one Client across requests
// is critical -- TLS handshakes to arxiv.org are slow.
type Client struct {
	http   *http.Client
	logger *logrus.Logger
}

// NewClient builds a Client with sensible defaults. A nil logger
// silently drops cascade events; callers should pass a real logger in
// production.
func NewClient(logger *logrus.Logger) *Client {
	if logger == nil {
		logger = logrus.New()
		logger.SetOutput(io.Discard)
	}
	return &Client{
		http: &http.Client{
			// Generous overall ceiling — arxiv occasionally takes 20s+
			// to fulfil e-print downloads under load. The cascade
			// gives up by moving to the next tier rather than retrying
			// at the HTTP layer.
			Timeout: 60 * time.Second,
		},
		logger: logger,
	}
}

// Fetch runs the cascade for arxivID. Tries each tier in prefer order
// (DefaultPrefer if empty), short-circuits on the first hit. Returns
// ErrAllTiersExhausted when every tier the caller asked for missed.
func (c *Client) Fetch(ctx context.Context, arxivID string, prefer []string) (*FetchedDoc, error) {
	if !arxivIDRe.MatchString(arxivID) {
		return nil, fmt.Errorf("arxiv: invalid id: %q", arxivID)
	}
	if len(prefer) == 0 {
		prefer = DefaultPrefer
	}
	for _, tier := range prefer {
		var (
			doc *FetchedDoc
			err error
		)
		switch tier {
		case "latex":
			doc, err = c.fetchLatex(ctx, arxivID)
		case "ar5iv":
			doc, err = c.fetchAr5iv(ctx, arxivID)
		case "vanity":
			doc, err = c.fetchVanity(ctx, arxivID)
		case "pdf":
			doc, err = c.fetchPDF(ctx, arxivID)
		default:
			c.logger.WithField("tier", tier).Warn("arxiv: unknown tier, skipping")
			continue
		}
		if err != nil {
			c.logger.WithError(err).WithFields(logrus.Fields{
				"tier":     tier,
				"arxiv_id": arxivID,
			}).Info("arxiv: tier failed, trying next")
			continue
		}
		if doc != nil {
			return doc, nil
		}
	}
	return nil, ErrAllTiersExhausted
}

// fetchLatex downloads the e-print tarball and returns it wholesale.
// Doesn't run any conversion locally — that work happens in
// ingestpipeline.Run via parsing.ParseLatexArchive.
func (c *Client) fetchLatex(ctx context.Context, arxivID string) (*FetchedDoc, error) {
	url := fmt.Sprintf("https://arxiv.org/e-print/%s", arxivID)
	c.logger.WithField("arxiv_id", arxivID).Info("arxiv: trying e-print/LaTeX")

	payload, code, err := c.get(ctx, url, MaxEprintBytes+1)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound || code == http.StatusGone {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	if int64(len(payload)) > MaxEprintBytes {
		c.logger.WithFields(logrus.Fields{
			"arxiv_id": arxivID,
			"bytes":    len(payload),
		}).Warn("arxiv: e-print exceeds size limit")
		return nil, nil
	}

	// Peek inside the tarball: we need at least one .tex member to call
	// this a usable e-print. Some arxiv ids return tarballs that are
	// just supplementary materials (zip of zip of zip). Bail and let the
	// PDF tier handle them.
	entries, err := tarwalk.WalkNames(payload)
	if err != nil {
		// If the bytes don't even parse as tar+gzip, this isn't an
		// e-print archive. Cascade falls through.
		return nil, fmt.Errorf("walk e-print: %w", err)
	}
	var texFiles []string
	for _, e := range entries {
		if strings.HasSuffix(strings.ToLower(e.Name), ".tex") {
			texFiles = append(texFiles, e.Name)
		}
	}
	if len(texFiles) == 0 {
		c.logger.WithField("arxiv_id", arxivID).Info("arxiv: e-print has no .tex members, falling through")
		return nil, nil
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		sizes[e.Name] = e.Size
	}
	mainTex := pickMainTex(texFiles, sizes)

	return &FetchedDoc{
		Adapter:     "arxiv",
		ExternalID:  arxivID,
		URL:         url,
		Title:       strings.TrimSuffix(filepath.Base(mainTex), filepath.Ext(mainTex)),
		ContentType: "application/gzip",
		Body:        payload,
		SourceTier:  "latex",
		Metadata: map[string]any{
			"main_tex":         mainTex,
			"tex_files":        texFiles,
			"raw_archive_size": len(payload),
			"length":           len(payload),
		},
	}, nil
}

// fetchAr5iv fetches ar5iv's HTML and converts to Markdown.
func (c *Client) fetchAr5iv(ctx context.Context, arxivID string) (*FetchedDoc, error) {
	url := fmt.Sprintf("https://ar5iv.labs.arxiv.org/html/%s", arxivID)
	c.logger.WithField("arxiv_id", arxivID).Info("arxiv: trying ar5iv")

	payload, code, err := c.get(ctx, url, 10<<20)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound || code == http.StatusGone {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	htmlStr := string(payload)
	for _, sentinel := range ar5ivErrorSentinels {
		if strings.Contains(htmlStr, sentinel) {
			c.logger.WithField("arxiv_id", arxivID).Info("arxiv: ar5iv reported processing error")
			return nil, nil
		}
	}
	md, title, ok, err := convertAr5ivHTML(htmlStr, arxivID)
	if err != nil {
		return nil, fmt.Errorf("convert ar5iv html: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return &FetchedDoc{
		Adapter:     "arxiv",
		ExternalID:  arxivID,
		URL:         url,
		Title:       title,
		ContentType: "text/markdown",
		Body:        []byte(md),
		SourceTier:  "ar5iv",
		Metadata: map[string]any{
			"length": len(md),
		},
	}, nil
}

// fetchVanity fetches arxiv-vanity HTML and converts to Markdown.
func (c *Client) fetchVanity(ctx context.Context, arxivID string) (*FetchedDoc, error) {
	url := fmt.Sprintf("https://www.arxiv-vanity.com/papers/%s/", arxivID)
	c.logger.WithField("arxiv_id", arxivID).Info("arxiv: trying arxiv-vanity")

	payload, code, err := c.get(ctx, url, 10<<20)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound || code == http.StatusGone || code == http.StatusServiceUnavailable {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	md, title, err := convertVanityHTML(string(payload), arxivID)
	if err != nil {
		return nil, fmt.Errorf("convert vanity html: %w", err)
	}
	if md == "" {
		return nil, nil
	}
	return &FetchedDoc{
		Adapter:     "arxiv",
		ExternalID:  arxivID,
		URL:         url,
		Title:       title,
		ContentType: "text/markdown",
		Body:        []byte(md),
		SourceTier:  "vanity",
		Metadata: map[string]any{
			"length": len(md),
		},
	}, nil
}

// fetchPDF downloads the PDF as the last-resort tier. No conversion --
// ingestpipeline.Parse routes application/pdf through pdftotext.
func (c *Client) fetchPDF(ctx context.Context, arxivID string) (*FetchedDoc, error) {
	url := fmt.Sprintf("https://arxiv.org/pdf/%s.pdf", arxivID)
	c.logger.WithField("arxiv_id", arxivID).Info("arxiv: trying PDF")

	payload, code, err := c.get(ctx, url, 50<<20)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound || code == http.StatusGone {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", code)
	}
	if !bytes.HasPrefix(payload, []byte("%PDF-")) {
		// arxiv occasionally returns a HTML "loading..." page when the
		// PDF isn't ready yet. Treat as a miss.
		return nil, nil
	}
	return &FetchedDoc{
		Adapter:     "arxiv",
		ExternalID:  arxivID,
		URL:         url,
		Title:       arxivID, // PDF metadata isn't worth a second round-trip
		ContentType: "application/pdf",
		Body:        payload,
		SourceTier:  "pdf",
		Metadata: map[string]any{
			"length": len(payload),
		},
	}, nil
}

// get does the HTTP fetch with a single retry on transient errors.
// Returns (body, statusCode, error). A non-error 404 is the cascade's
// signal to skip the tier; the caller decides what to do.
func (c *Client) get(ctx context.Context, url string, maxBytes int64) ([]byte, int, error) {
	var lastErr error
	delays := []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond}
	for _, d := range delays {
		if d > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(d):
			}
		}
		body, code, err := c.doOnce(ctx, url, maxBytes)
		if err == nil {
			return body, code, nil
		}
		// Don't retry 4xx — those are deterministic and the tier
		// should give up. Retries are for network blips / 5xx only.
		if code >= 400 && code < 500 {
			return body, code, err
		}
		lastErr = err
	}
	return nil, 0, lastErr
}

func (c *Client) doOnce(ctx context.Context, url string, maxBytes int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// pickMainTex mirrors Python _pick_main_tex. Preference order:
//
//  1. main.tex
//  2. paper.tex
//  3. largest .tex file
//
// Step 3 of the Python helper (\documentclass scan) is dropped --
// requires reading file bodies, and r2g's latex2text picks the right
// main file inside the Zig binary anyway. This output is only used for
// the metadata blob on the persisted document.
func pickMainTex(names []string, sizes map[string]int64) string {
	if len(names) == 0 {
		return ""
	}
	for _, preferred := range []string{"main.tex", "paper.tex"} {
		for _, n := range names {
			if strings.EqualFold(path.Base(n), preferred) {
				return n
			}
		}
	}
	largest := names[0]
	for _, n := range names {
		if sizes[n] > sizes[largest] {
			largest = n
		}
	}
	return largest
}

// Package webfetch implements the doi: and url: ingest fetchers for
// POST /platform/sources/ingest. It is the general-document counterpart
// of internal/arxiv: given a DOI or a bare URL it resolves/downloads the
// content and hands back bytes + a content type the ingest pipeline's
// parser already understands (application/pdf or text/markdown).
//
// DOI path: the DOI is resolved through doi.org (content-negotiated to
// prefer PDF, falling back to the publisher's HTML landing page). When
// the landing page is HTML we first look for the widely-used
// citation_pdf_url <meta> tag (Google Scholar convention, emitted by
// most publishers) and fetch that PDF; otherwise we fall back to
// readability-style main-content extraction and convert to markdown.
//
// URL path: plain fetch. PDF bytes pass through; HTML goes through the
// same readability extraction; plain text / markdown pass through.
package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/PuerkitoBio/goquery"
	"github.com/sirupsen/logrus"
)

const userAgent = "deepresearch-platform/0.1.0 (+r2g webfetch)"

// MaxFetchBytes caps any single download. Mirrors arxiv.MaxEprintBytes:
// anything bigger is almost certainly a misuse of the ingest endpoint.
const MaxFetchBytes = 50 << 20 // 50 MiB

// Sentinel errors. The route layer maps ErrInvalidDOI / ErrInvalidURL /
// ErrUnsupportedContent to 400 and everything else to 502.
var (
	ErrInvalidDOI         = errors.New("webfetch: invalid DOI")
	ErrInvalidURL         = errors.New("webfetch: invalid URL (must be absolute http/https)")
	ErrUnsupportedContent = errors.New("webfetch: unsupported content type")
	ErrTooLarge           = errors.New("webfetch: response exceeds size limit")
	ErrEmptyBody          = errors.New("webfetch: empty response body")
)

// doiRE validates the modern Crossref DOI shape (10.NNNN/suffix). The
// suffix is intentionally loose — publishers put nearly anything there.
var doiRE = regexp.MustCompile(`^10\.\d{4,9}/\S+$`)

// multiBlankLine collapses 3+ consecutive newlines down to 2 in the
// converted markdown (same post-processing as internal/arxiv).
var multiBlankLine = regexp.MustCompile(`\n{3,}`)

// Doc is the fetch result. ContentType is always one the ingest
// pipeline's parser dispatches on (application/pdf, text/markdown or
// text/plain). Metadata carries provenance for the document row.
type Doc struct {
	URL         string // final URL after redirects
	Title       string // best-effort; falls back to the DOI / URL
	ContentType string
	Body        []byte
	SourceKind  string // "pdf" | "html" | "text"
	Metadata    map[string]any
}

// Client wraps an http.Client with the UA + timeout we want for all
// outbound document fetches. DOIBase is overridable for tests (defaults
// to https://doi.org).
type Client struct {
	http    *http.Client
	logger  *logrus.Logger
	DOIBase string
	// MaxBytes caps each download; zero means MaxFetchBytes.
	MaxBytes int64
}

// NewClient builds a Client with production defaults. A nil logger
// silently drops fetch events.
func NewClient(logger *logrus.Logger) *Client {
	if logger == nil {
		logger = logrus.New()
		logger.SetOutput(io.Discard)
	}
	return &Client{
		http: &http.Client{
			Timeout: 60 * time.Second,
		},
		logger:  logger,
		DOIBase: "https://doi.org",
	}
}

// NormalizeDOI strips the common wrapper shapes callers paste in:
// "https://doi.org/10.x/y", "doi.org/10.x/y", "doi:10.x/y". Returns the
// bare "10.x/y" or an error when the residue doesn't look like a DOI.
func NormalizeDOI(raw string) (string, error) {
	d := strings.TrimSpace(raw)
	lower := strings.ToLower(d)
	for _, prefix := range []string{
		"https://doi.org/", "http://doi.org/",
		"https://dx.doi.org/", "http://dx.doi.org/",
		"doi.org/", "dx.doi.org/", "doi:",
	} {
		if strings.HasPrefix(lower, prefix) {
			d = d[len(prefix):]
			break
		}
	}
	d = strings.TrimSpace(d)
	if !doiRE.MatchString(d) {
		return "", fmt.Errorf("%w: %q", ErrInvalidDOI, raw)
	}
	return d, nil
}

// FetchDOI resolves a DOI to document bytes. Cascade:
//
//  1. GET {DOIBase}/{doi} with Accept preferring PDF over HTML. The
//     resolver 302s to the publisher; the http.Client follows.
//  2. Response is PDF -> done.
//  3. Response is HTML -> if the page carries a citation_pdf_url meta
//     tag, fetch that PDF; on any failure fall back to readability
//     extraction of the landing page itself.
func (c *Client) FetchDOI(ctx context.Context, rawDOI string) (*Doc, error) {
	doi, err := NormalizeDOI(rawDOI)
	if err != nil {
		return nil, err
	}
	base := c.DOIBase
	if base == "" {
		base = "https://doi.org"
	}
	resolveURL := strings.TrimRight(base, "/") + "/" + doi

	body, finalURL, contentType, err := c.get(ctx, resolveURL,
		"application/pdf, text/html;q=0.9, */*;q=0.8")
	if err != nil {
		return nil, fmt.Errorf("resolve doi %s: %w", doi, err)
	}

	meta := map[string]any{
		"source":       "doi",
		"doi":          doi,
		"source_url":   "https://doi.org/" + doi,
		"resolved_url": finalURL,
	}

	switch {
	case isPDF(contentType, body):
		return &Doc{
			URL: finalURL, Title: doi, ContentType: "application/pdf",
			Body: body, SourceKind: "pdf", Metadata: meta,
		}, nil
	case isHTML(contentType, body):
		doc, parseErr := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
		if parseErr == nil {
			if pdfURL := citationPDFURL(doc, finalURL); pdfURL != "" {
				pdfBody, pdfFinal, pdfCT, pdfErr := c.get(ctx, pdfURL, "application/pdf, */*;q=0.8")
				if pdfErr == nil && isPDF(pdfCT, pdfBody) {
					meta["resolved_url"] = pdfFinal
					meta["pdf_url"] = pdfURL
					return &Doc{
						URL: pdfFinal, Title: doi, ContentType: "application/pdf",
						Body: pdfBody, SourceKind: "pdf", Metadata: meta,
					}, nil
				}
				c.logger.WithField("pdf_url", pdfURL).WithError(pdfErr).
					Debug("webfetch: citation_pdf_url fetch missed; falling back to landing page")
			}
		}
		md, title, exErr := ExtractReadable(string(body), doi)
		if exErr != nil {
			return nil, fmt.Errorf("extract doi landing page: %w", exErr)
		}
		return &Doc{
			URL: finalURL, Title: title, ContentType: "text/markdown",
			Body: []byte(md), SourceKind: "html", Metadata: meta,
		}, nil
	default:
		return nil, fmt.Errorf("%w: doi %s resolved to %s", ErrUnsupportedContent, doi, contentType)
	}
}

// FetchURL downloads an arbitrary http(s) URL and normalizes it into
// pipeline-parseable bytes. HTML goes through readability extraction;
// PDF and text pass through.
func (c *Client) FetchURL(ctx context.Context, rawURL string) (*Doc, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidURL, rawURL)
	}

	body, finalURL, contentType, err := c.get(ctx, u.String(),
		"application/pdf, text/html;q=0.9, text/plain;q=0.8, */*;q=0.5")
	if err != nil {
		return nil, fmt.Errorf("fetch url: %w", err)
	}

	meta := map[string]any{
		"source":       "url",
		"source_url":   u.String(),
		"resolved_url": finalURL,
	}

	switch {
	case isPDF(contentType, body):
		return &Doc{
			URL: finalURL, Title: titleFromURL(u), ContentType: "application/pdf",
			Body: body, SourceKind: "pdf", Metadata: meta,
		}, nil
	case isHTML(contentType, body):
		md, title, exErr := ExtractReadable(string(body), titleFromURL(u))
		if exErr != nil {
			return nil, fmt.Errorf("extract url content: %w", exErr)
		}
		return &Doc{
			URL: finalURL, Title: title, ContentType: "text/markdown",
			Body: []byte(md), SourceKind: "html", Metadata: meta,
		}, nil
	case strings.HasPrefix(baseCT(contentType), "text/markdown"),
		strings.HasPrefix(baseCT(contentType), "text/x-markdown"):
		return &Doc{
			URL: finalURL, Title: titleFromURL(u), ContentType: "text/markdown",
			Body: body, SourceKind: "text", Metadata: meta,
		}, nil
	case strings.HasPrefix(baseCT(contentType), "text/plain"):
		return &Doc{
			URL: finalURL, Title: titleFromURL(u), ContentType: "text/plain",
			Body: body, SourceKind: "text", Metadata: meta,
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s served %s", ErrUnsupportedContent, u.String(), contentType)
	}
}

// get performs one GET (following redirects) and returns (body,
// finalURL, contentType). Non-2xx statuses are errors. The body is
// capped at MaxBytes.
func (c *Client) get(ctx context.Context, rawURL, accept string) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", accept)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, finalURL, "", fmt.Errorf("upstream HTTP %d for %s", resp.StatusCode, finalURL)
	}

	maxBytes := c.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxFetchBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, finalURL, "", fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, finalURL, "", fmt.Errorf("%w (> %d bytes)", ErrTooLarge, maxBytes)
	}
	if len(body) == 0 {
		return nil, finalURL, "", ErrEmptyBody
	}
	return body, finalURL, resp.Header.Get("Content-Type"), nil
}

// ─────────────────────────────────────────────────────────────────────────
// HTML readability extraction
// ─────────────────────────────────────────────────────────────────────────

// readabilityNoise are selectors stripped from the chosen content root
// before markdown conversion. This is a pragmatic reduction of the
// readability algorithm: drop chrome + boilerplate, keep the article.
var readabilityNoise = []string{
	"script", "style", "noscript", "template", "iframe", "svg", "canvas",
	"nav", "header", "footer", "aside", "form", "button", "input", "select",
	"[role=navigation]", "[role=banner]", "[role=contentinfo]", "[role=search]",
	"[aria-hidden=true]",
	".nav", ".navbar", ".menu", ".sidebar", ".footer", ".header",
	".advertisement", ".ads", ".ad", ".cookie-banner", ".newsletter",
	".share", ".social", ".comments", ".related", ".breadcrumb",
}

// readabilityRoots are tried in order; the first non-empty match wins.
// Falls back to <body> when no candidate matches.
var readabilityRoots = []string{
	"article", "main", "[role=main]",
	"#content", "#main-content", ".post-content", ".article-content",
	".entry-content", ".content",
}

// ExtractReadable pulls the main content out of an HTML page and
// converts it to markdown. Returns (markdown, title, err). The title is
// og:title > <title> > citation_title > fallbackTitle.
func ExtractReadable(htmlStr, fallbackTitle string) (string, string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlStr))
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}

	title := fallbackTitle
	if t, ok := doc.Find(`meta[property="og:title"]`).Attr("content"); ok && strings.TrimSpace(t) != "" {
		title = strings.TrimSpace(t)
	} else if t, ok := doc.Find(`meta[name="citation_title"]`).Attr("content"); ok && strings.TrimSpace(t) != "" {
		title = strings.TrimSpace(t)
	} else if t := strings.TrimSpace(doc.Find("title").First().Text()); t != "" {
		title = t
	}

	var root *goquery.Selection
	for _, sel := range readabilityRoots {
		if s := doc.Find(sel).First(); s.Length() > 0 && len(strings.TrimSpace(s.Text())) > 0 {
			root = s
			break
		}
	}
	if root == nil {
		root = doc.Find("body").First()
	}
	if root.Length() == 0 {
		root = doc.Selection
	}
	for _, sel := range readabilityNoise {
		root.Find(sel).Remove()
	}

	htmlOut, err := goquery.OuterHtml(root)
	if err != nil {
		return "", "", fmt.Errorf("serialise content root: %w", err)
	}
	md, err := htmltomarkdown.ConvertString(htmlOut)
	if err != nil {
		return "", "", fmt.Errorf("html-to-markdown: %w", err)
	}
	md = multiBlankLine.ReplaceAllString(md, "\n\n")
	md = strings.TrimSpace(md)
	if md == "" {
		return "", "", errors.New("webfetch: page produced no extractable text")
	}
	return md + "\n", title, nil
}

// citationPDFURL returns the citation_pdf_url meta value resolved
// against the page URL, or "" when absent/unresolvable.
func citationPDFURL(doc *goquery.Document, pageURL string) string {
	v, ok := doc.Find(`meta[name="citation_pdf_url"]`).Attr("content")
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return v
	}
	ref, err := url.Parse(v)
	if err != nil {
		return ""
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return ""
	}
	return abs.String()
}

// baseCT strips parameters and lowercases a Content-Type header value.
func baseCT(ct string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
}

func isPDF(contentType string, body []byte) bool {
	if baseCT(contentType) == "application/pdf" {
		return true
	}
	return len(body) >= 5 && strings.HasPrefix(string(body[:5]), "%PDF-")
}

func isHTML(contentType string, body []byte) bool {
	ct := baseCT(contentType)
	if ct == "text/html" || ct == "application/xhtml+xml" {
		return true
	}
	if ct != "" && ct != "application/octet-stream" {
		return false
	}
	head := strings.ToLower(string(body[:min(len(body), 512)]))
	return strings.Contains(head, "<!doctype html") || strings.Contains(head, "<html")
}

// titleFromURL derives a placeholder title from the URL path (last
// non-empty segment) falling back to the host.
func titleFromURL(u *url.URL) string {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(segs[i]); s != "" {
			return s
		}
	}
	return u.Host
}

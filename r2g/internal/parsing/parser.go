// Package parsing turns raw upload bytes into plain text ready for
// chunking. The dispatcher in Parse() inspects the request's
// content-type plus a small magic-byte sniff and routes to the
// right backend:
//
//   text/markdown, text/plain      -> pass-through
//   application/pdf                -> pdftotext subprocess (stdin/stdout)
//   application/x-tar, gzip, etc.  -> latex2text Zig binary (file -> JSON)
//
// Both subprocess paths are bounded by context: caller's request
// deadline cancels the child via ctx, so a wedged pdftotext or
// latex2text doesn't hold a chunk slot. Failure falls through to an
// error -- the ingest handler maps that to 415 / 500 as appropriate.
package parsing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrUnsupportedType is returned when the content type doesn't map to
// any known parser. Handler code surfaces this as HTTP 415.
var ErrUnsupportedType = errors.New("unsupported content type")

// FigureInfo is one figure's metadata surfaced from the parsing layer
// up to the ingest route, which then resolves the src paths against the
// tarball (see r2g/internal/figures). Empty for non-eprint inputs.
type FigureInfo struct {
	Label    string
	Caption  string
	SrcPaths []string
}

// Result is what Parse returns. Text and DocType are always populated on
// success; Figures is non-nil only for the eprint path (latex2text
// emits the structured figure list there).
type Result struct {
	Text    string
	DocType string
	Figures []FigureInfo
}

// Parse routes content to the right parser by mime type. body is the
// raw upload bytes; filename is the optional original name (used as a
// fallback type hint when contentType is generic like
// application/octet-stream). Returns the extracted plain text, the
// canonical document_type ("md", "pdf", "eprint", ...), and figure
// metadata (eprint only).
func Parse(ctx context.Context, contentType, filename string, body []byte) (*Result, error) {
	ct := normalizeCT(contentType)
	if ct == "application/octet-stream" {
		ct = sniff(body, filename)
	}
	switch ct {
	case "text/markdown", "text/x-markdown", "text/md":
		return &Result{Text: string(body), DocType: "md"}, nil
	case "text/plain":
		return &Result{Text: string(body), DocType: "txt"}, nil
	case "application/pdf":
		text, err := ParsePDF(ctx, body)
		return &Result{Text: text, DocType: "pdf"}, err
	case "application/x-tar", "application/gzip", "application/x-gzip", "application/x-bzip2":
		text, figs, err := ParseLatexArchive(ctx, body)
		return &Result{Text: text, DocType: "eprint", Figures: figs}, err
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedType, ct)
}

// normalizeCT strips parameters (charset, boundary) and lowercases.
func normalizeCT(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	return strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
}

// sniff guesses content type from magic bytes when the caller's header
// was octet-stream. Covers the three cases the reindex pipeline
// produces: PDF (%PDF), gzipped tarball (1f 8b), or markdown (assume).
func sniff(body []byte, filename string) string {
	if len(body) >= 5 && bytes.HasPrefix(body, []byte("%PDF-")) {
		return "application/pdf"
	}
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		return "application/gzip"
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".md":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".pdf":
		return "application/pdf"
	case ".tar", ".gz", ".tgz", ".bz2":
		return "application/gzip"
	}
	// Defensive default: treat as markdown. Caller already gated on
	// content-types it expected.
	return "text/markdown"
}

// ParsePDF shells out to `pdftotext -enc UTF-8 -nopgbrk - -` reading
// the PDF from stdin and writing plain text to stdout. pdftotext is
// from poppler-utils which is already installed in the deepresearch
// backend container (Dockerfile:22).
//
// Defaults chosen to match what R2R extracts:
//   - UTF-8 output (otherwise pdftotext emits Latin-1 by default)
//   - -nopgbrk: no form-feed page break characters (otherwise our
//     chunker sees them as opaque tokens)
//   - default layout (not -layout / -raw) -- approximates reading
//     order for one- and two-column papers
func ParsePDF(ctx context.Context, body []byte) (string, error) {
	if len(body) == 0 {
		return "", errors.New("pdf: empty body")
	}
	cmd := exec.CommandContext(ctx, "pdftotext", "-enc", "UTF-8", "-nopgbrk", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w (stderr: %s)", err, tail(stderr.String(), 200))
	}
	return out.String(), nil
}

// ParseLatexArchive pipes the tarball bytes into the Zig latex2text
// binary on stdin and reads structured JSON from stdout. The binary
// emits title/abstract/authors/sections/figures/math; we synthesize a
// markdown blob from those fields for chunking.
//
// Binary path is `LATEX2TEXT_BIN` env (default /app/bin/latex2text in
// the deepresearch container) -- the same env var the Python wrapper
// uses, so deploying r2g alongside R2R adds no new ops surface.
func ParseLatexArchive(ctx context.Context, body []byte) (string, []FigureInfo, error) {
	binPath := os.Getenv("LATEX2TEXT_BIN")
	if binPath == "" {
		binPath = "/app/bin/latex2text"
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", nil, fmt.Errorf("latex2text binary not available at %s: %w", binPath, err)
	}

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("latex2text: %w (stderr: %s)", err, tail(stderr.String(), 200))
	}

	var doc latex2textDoc
	if err := json.NewDecoder(io.LimitReader(&stdout, 64<<20)).Decode(&doc); err != nil {
		return "", nil, fmt.Errorf("parse latex2text json: %w", err)
	}
	if len(doc.ParserErrors) > 0 || len(doc.Sections) == 0 {
		// "Unparseable content" is NOT "unsupported type" -- the
		// content-type was right (application/gzip), the bytes just
		// didn't yield usable sections. Return a distinct error so
		// the route maps to 422, not 415. The platform wrapper sees
		// a non-2xx and falls back to ar5iv/vanity, which is what
		// we want.
		return "", nil, fmt.Errorf("latex2text parser reported issues or zero sections (errors=%d sections=%d)",
			len(doc.ParserErrors), len(doc.Sections))
	}
	figs := make([]FigureInfo, 0, len(doc.Figures))
	for _, f := range doc.Figures {
		figs = append(figs, FigureInfo{
			Label:    f.Label,
			Caption:  f.Caption,
			SrcPaths: f.SrcPaths,
		})
	}
	return synthesizeMarkdown(doc), figs, nil
}

// latex2textDoc mirrors the JSON shape produced by the Zig binary.
// See tools/latex2text/README.md for the full schema; we read only
// the fields ingest needs.
type latex2textDoc struct {
	Title    string `json:"title"`
	Abstract string `json:"abstract"`
	Authors  []string `json:"authors"`
	Sections []struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Level int    `json:"level"`
	} `json:"sections"`
	Figures []struct {
		Caption  string   `json:"caption"`
		Label    string   `json:"label"`
		SrcPaths []string `json:"src_paths"`
	} `json:"figures"`
	ParserErrors []string `json:"parser_errors"`
}

// synthesizeMarkdown turns the latex2text JSON into a single markdown
// blob ready for chunking. Header levels mirror section levels;
// figures are inlined as italic captions so they show up in
// embeddings without needing a separate retrieval path.
func synthesizeMarkdown(d latex2textDoc) string {
	var b strings.Builder
	if d.Title != "" {
		b.WriteString("# ")
		b.WriteString(d.Title)
		b.WriteString("\n\n")
	}
	if len(d.Authors) > 0 {
		b.WriteString("*Authors:* ")
		b.WriteString(strings.Join(d.Authors, ", "))
		b.WriteString("\n\n")
	}
	if d.Abstract != "" {
		b.WriteString("## Abstract\n\n")
		b.WriteString(d.Abstract)
		b.WriteString("\n\n")
	}
	for _, s := range d.Sections {
		level := s.Level
		if level <= 0 {
			level = 2
		}
		if level > 6 {
			level = 6
		}
		b.WriteString(strings.Repeat("#", level))
		b.WriteString(" ")
		if s.Title != "" {
			b.WriteString(s.Title)
		}
		b.WriteString("\n\n")
		b.WriteString(s.Body)
		b.WriteString("\n\n")
	}
	for _, f := range d.Figures {
		if f.Caption == "" && f.Label == "" {
			continue
		}
		b.WriteString("*Figure")
		if f.Label != "" {
			b.WriteString(" ")
			b.WriteString(f.Label)
		}
		if f.Caption != "" {
			b.WriteString(": ")
			b.WriteString(f.Caption)
		}
		b.WriteString("*\n\n")
	}
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

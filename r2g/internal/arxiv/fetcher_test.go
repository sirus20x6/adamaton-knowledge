package arxiv

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// makeTarGz builds a small gzipped tarball from a name->contents map. Used
// to build fixtures for the latex-tier tests.
func makeTarGz(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, contents := range members {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(contents)),
			Typeflag: tar.TypeReg,
			ModTime:  time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(contents)); err != nil {
			t.Fatalf("tar write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return buf.Bytes()
}

// fixtureClient swaps the cascade's http.Client for one whose Transport
// rewrites every request URL to the test server's address. We never want
// to hit the real arxiv during unit tests.
type fixtureTransport struct {
	server *httptest.Server
}

func (t *fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Send everything to the local test server; keep the path so the
	// server's handler can branch on it.
	newReq := req.Clone(req.Context())
	newReq.URL.Scheme = "http"
	newReq.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(newReq)
}

// withFixture spins up an httptest server and returns a Client wired to it
// plus a cleanup func.
func withFixture(t *testing.T, h http.Handler) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := NewClient(nil)
	c.http = &http.Client{Transport: &fixtureTransport{server: srv}}
	return c, srv.Close
}

func TestArxivIDValidation(t *testing.T) {
	for _, id := range []string{"1706.03762", "2403.12345v2", "hep-th/0301001"} {
		if !arxivIDRe.MatchString(id) {
			t.Errorf("%q should match arxivIDRe", id)
		}
	}
	for _, id := range []string{"", "notanid", "12345", "foo-bar"} {
		if arxivIDRe.MatchString(id) {
			t.Errorf("%q should NOT match", id)
		}
	}
}

func TestFetchLatexHappyPath(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{
		"main.tex":   `\documentclass{article}\begin{document}Hi\end{document}`,
		"figure.png": "PNGDATA",
	})
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/e-print/") {
			t.Errorf("expected /e-print path, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	defer close()

	doc, err := c.Fetch(context.Background(), "1706.03762", []string{"latex"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.SourceTier != "latex" {
		t.Errorf("source_tier=%q want latex", doc.SourceTier)
	}
	if doc.ContentType != "application/gzip" {
		t.Errorf("content_type=%q want application/gzip", doc.ContentType)
	}
	if !bytes.Equal(doc.Body, tarball) {
		t.Errorf("body bytes differ from input tarball")
	}
	if doc.Metadata["main_tex"] != "main.tex" {
		t.Errorf("main_tex=%v want main.tex", doc.Metadata["main_tex"])
	}
}

func TestFetchLatexNoTexFallsThrough(t *testing.T) {
	tarball := makeTarGz(t, map[string]string{
		"data.csv": "x,y",
	})
	hitPDF := false
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/e-print/"):
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(tarball)
		case strings.Contains(r.URL.Path, "/pdf/"):
			hitPDF = true
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4\nfake pdf payload"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer close()

	doc, err := c.Fetch(context.Background(), "1706.03762", []string{"latex", "pdf"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.SourceTier != "pdf" {
		t.Errorf("expected fallthrough to pdf, got tier=%q", doc.SourceTier)
	}
	if !hitPDF {
		t.Errorf("expected PDF tier to be hit")
	}
}

func TestFetchPDFHappyPath(t *testing.T) {
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4\nfake"))
	}))
	defer close()
	doc, err := c.Fetch(context.Background(), "2403.12345", []string{"pdf"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.SourceTier != "pdf" || doc.ContentType != "application/pdf" {
		t.Errorf("got tier=%q ct=%q", doc.SourceTier, doc.ContentType)
	}
}

func TestFetchPDFRejectsNonPDF(t *testing.T) {
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Some arxiv responses are HTML "loading..." pages even with
		// 200 OK. The cascade must reject them.
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>loading...</html>"))
	}))
	defer close()
	_, err := c.Fetch(context.Background(), "2403.12345", []string{"pdf"})
	if !errors.Is(err, ErrAllTiersExhausted) {
		t.Errorf("expected ErrAllTiersExhausted, got %v", err)
	}
}

func TestFetchAr5ivHappyPath(t *testing.T) {
	html := `<!doctype html><html><body>
<article class="ltx_document">
  <h1 class="ltx_title">Attention Is All You Need</h1>
  <p>The dominant sequence transduction models...</p>
  <math alttext="E = mc^2" display="block"></math>
</article>
</body></html>`
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, html)
	}))
	defer close()

	doc, err := c.Fetch(context.Background(), "1706.03762", []string{"ar5iv"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if doc.SourceTier != "ar5iv" {
		t.Errorf("source_tier=%q want ar5iv", doc.SourceTier)
	}
	body := string(doc.Body)
	if !strings.Contains(body, "Attention Is All You Need") {
		t.Errorf("body should keep title text, got:\n%s", body)
	}
	if !strings.Contains(body, "$$E = mc^2$$") {
		t.Errorf("MathML alttext should become $$E = mc^2$$, got:\n%s", body)
	}
}

func TestFetchAr5ivErrorPageFallsThrough(t *testing.T) {
	hitVanity := false
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Host, "ar5iv") || strings.Contains(r.URL.Path, "/html/"):
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html>this paper could not be processed</html>`)
		case strings.Contains(r.URL.Path, "/papers/"):
			hitVanity = true
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<html><body><article><h1>Vanity Title</h1><p>body</p></article></body></html>`)
		}
	}))
	defer close()

	doc, err := c.Fetch(context.Background(), "1706.03762", []string{"ar5iv", "vanity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !hitVanity {
		t.Errorf("expected vanity tier hit after ar5iv miss")
	}
	if doc.SourceTier != "vanity" {
		t.Errorf("source_tier=%q want vanity", doc.SourceTier)
	}
}

func TestFetchAllTiersMiss(t *testing.T) {
	c, close := withFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer close()
	_, err := c.Fetch(context.Background(), "1234.56789", []string{"latex", "ar5iv", "pdf"})
	if !errors.Is(err, ErrAllTiersExhausted) {
		t.Errorf("want ErrAllTiersExhausted, got %v", err)
	}
}

func TestPickMainTexPreference(t *testing.T) {
	cases := []struct {
		names []string
		sizes map[string]int64
		want  string
	}{
		{[]string{"chapter1.tex", "main.tex", "appendix.tex"}, nil, "main.tex"},
		{[]string{"paper.tex", "notes.tex"}, nil, "paper.tex"},
		{[]string{"a.tex", "b.tex"}, map[string]int64{"a.tex": 100, "b.tex": 9000}, "b.tex"},
		{[]string{"only.tex"}, nil, "only.tex"},
	}
	for _, tc := range cases {
		got := pickMainTex(tc.names, tc.sizes)
		if got != tc.want {
			t.Errorf("pickMainTex(%v, %v)=%q want %q", tc.names, tc.sizes, got, tc.want)
		}
	}
}

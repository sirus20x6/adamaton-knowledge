package webfetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"io"
)

func testClient() *Client {
	lg := logrus.New()
	lg.SetOutput(io.Discard)
	return NewClient(lg)
}

// fakePDF is a minimal body that passes the %PDF- magic sniff.
const fakePDF = "%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n"

const articleHTML = `<!doctype html>
<html>
<head>
  <title>Fallback Title</title>
  <meta property="og:title" content="The Attention Economy">
</head>
<body>
  <nav><a href="/">Home</a><a href="/about">About</a></nav>
  <header>Site banner</header>
  <article>
    <h1>The Attention Economy</h1>
    <p>Attention is the scarce resource of the modern web.</p>
    <p>This second paragraph carries the substantive argument.</p>
    <aside>Subscribe to our newsletter!</aside>
    <script>trackUser();</script>
  </article>
  <footer>Copyright 2026</footer>
</body>
</html>`

// TestNormalizeDOI covers the wrapper shapes callers paste in plus the
// invalid rejections.
func TestNormalizeDOI(t *testing.T) {
	valid := map[string]string{
		"10.1038/nphys1170":                   "10.1038/nphys1170",
		"https://doi.org/10.1038/nphys1170":   "10.1038/nphys1170",
		"http://dx.doi.org/10.1038/nphys1170": "10.1038/nphys1170",
		"doi:10.48550/arXiv.1706.03762":       "10.48550/arXiv.1706.03762",
		"  doi.org/10.1145/3292500.3330701  ": "10.1145/3292500.3330701",
	}
	for in, want := range valid {
		got, err := NormalizeDOI(in)
		if err != nil {
			t.Errorf("NormalizeDOI(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeDOI(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "not-a-doi", "11.1038/x", "10.38/x", "10.1038/", "10.1038"} {
		if _, err := NormalizeDOI(in); !errors.Is(err, ErrInvalidDOI) {
			t.Errorf("NormalizeDOI(%q) = err %v, want ErrInvalidDOI", in, err)
		}
	}
}

// TestFetchURL_PDF pins the passthrough path: a served PDF comes back
// byte-identical with content type application/pdf.
func TestFetchURL_PDF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(fakePDF))
	}))
	defer srv.Close()

	doc, err := testClient().FetchURL(context.Background(), srv.URL+"/paper.pdf")
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if doc.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf", doc.ContentType)
	}
	if string(doc.Body) != fakePDF {
		t.Errorf("body mismatch: got %d bytes", len(doc.Body))
	}
	if doc.SourceKind != "pdf" {
		t.Errorf("SourceKind = %q, want pdf", doc.SourceKind)
	}
	if doc.Metadata["source"] != "url" {
		t.Errorf("metadata source = %v, want url", doc.Metadata["source"])
	}
}

// TestFetchURL_HTMLReadability confirms the extraction keeps article
// text, drops nav/footer/script noise, converts to markdown, and picks
// the og:title.
func TestFetchURL_HTMLReadability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(articleHTML))
	}))
	defer srv.Close()

	doc, err := testClient().FetchURL(context.Background(), srv.URL+"/post")
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if doc.ContentType != "text/markdown" {
		t.Errorf("ContentType = %q, want text/markdown", doc.ContentType)
	}
	md := string(doc.Body)
	for _, want := range []string{
		"Attention is the scarce resource",
		"second paragraph carries the substantive argument",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	for _, drop := range []string{"trackUser", "Subscribe to our newsletter", "Copyright 2026", "Site banner"} {
		if strings.Contains(md, drop) {
			t.Errorf("markdown kept noise %q:\n%s", drop, md)
		}
	}
	if doc.Title != "The Attention Economy" {
		t.Errorf("Title = %q, want og:title value", doc.Title)
	}
}

// TestFetchURL_PlainText pins the text passthrough.
func TestFetchURL_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("just some notes\n"))
	}))
	defer srv.Close()

	doc, err := testClient().FetchURL(context.Background(), srv.URL+"/notes.txt")
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if doc.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want text/plain", doc.ContentType)
	}
	if string(doc.Body) != "just some notes\n" {
		t.Errorf("body = %q", doc.Body)
	}
}

// TestFetchURL_InvalidInputs rejects non-http(s) and relative URLs
// with ErrInvalidURL before any network I/O.
func TestFetchURL_InvalidInputs(t *testing.T) {
	for _, in := range []string{"", "ftp://example.com/x", "file:///etc/passwd", "/relative/path", "example.com/no-scheme"} {
		if _, err := testClient().FetchURL(context.Background(), in); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("FetchURL(%q) err = %v, want ErrInvalidURL", in, err)
		}
	}
}

// TestFetchURL_UpstreamError maps a 404 to a plain (non-sentinel)
// error the route layer turns into 502.
func TestFetchURL_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := testClient().FetchURL(context.Background(), srv.URL+"/gone")
	if err == nil {
		t.Fatal("expected error for 404 upstream")
	}
	if errors.Is(err, ErrInvalidURL) || errors.Is(err, ErrUnsupportedContent) {
		t.Errorf("404 should not map to a 400-class sentinel; got %v", err)
	}
}

// TestFetchURL_TooLarge enforces the byte cap.
func TestFetchURL_TooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	c := testClient()
	c.MaxBytes = 1024
	if _, err := c.FetchURL(context.Background(), srv.URL); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

// TestFetchDOI_ResolvesToPDF exercises the happy cascade: doi.org-style
// redirect to a landing PDF.
func TestFetchDOI_ResolvesToPDF(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/10.1234/direct", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/pdf/direct.pdf", http.StatusFound)
	})
	mux.HandleFunc("/pdf/direct.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(fakePDF))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient()
	c.DOIBase = srv.URL
	doc, err := c.FetchDOI(context.Background(), "10.1234/direct")
	if err != nil {
		t.Fatalf("FetchDOI: %v", err)
	}
	if doc.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf", doc.ContentType)
	}
	if doc.Metadata["doi"] != "10.1234/direct" {
		t.Errorf("metadata doi = %v", doc.Metadata["doi"])
	}
}

// TestFetchDOI_CitationPDFURL exercises the landing-page hop: HTML page
// carrying citation_pdf_url (relative) -> the PDF wins over the HTML.
func TestFetchDOI_CitationPDFURL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/10.1234/landing", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/articles/42", http.StatusFound)
	})
	mux.HandleFunc("/articles/42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head>
			<meta name="citation_pdf_url" content="/articles/42.pdf">
			<title>Landing</title></head>
			<body><article><p>Abstract only.</p></article></body></html>`))
	})
	mux.HandleFunc("/articles/42.pdf", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(fakePDF))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient()
	c.DOIBase = srv.URL
	doc, err := c.FetchDOI(context.Background(), "10.1234/landing")
	if err != nil {
		t.Fatalf("FetchDOI: %v", err)
	}
	if doc.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q, want application/pdf (citation_pdf_url hop)", doc.ContentType)
	}
	if string(doc.Body) != fakePDF {
		t.Errorf("expected the linked PDF body, got %d bytes", len(doc.Body))
	}
	if doc.Metadata["pdf_url"] == nil {
		t.Error("metadata missing pdf_url provenance")
	}
}

// TestFetchDOI_HTMLFallback: landing page without citation_pdf_url
// falls back to readability extraction as markdown.
func TestFetchDOI_HTMLFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/10.1234/htmlonly", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(articleHTML))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := testClient()
	c.DOIBase = srv.URL
	doc, err := c.FetchDOI(context.Background(), "10.1234/htmlonly")
	if err != nil {
		t.Fatalf("FetchDOI: %v", err)
	}
	if doc.ContentType != "text/markdown" {
		t.Errorf("ContentType = %q, want text/markdown", doc.ContentType)
	}
	if !strings.Contains(string(doc.Body), "scarce resource") {
		t.Errorf("markdown missing article text:\n%s", doc.Body)
	}
	if doc.Title != "The Attention Economy" {
		t.Errorf("Title = %q", doc.Title)
	}
}

// TestFetchDOI_Invalid rejects malformed DOIs without network I/O.
func TestFetchDOI_Invalid(t *testing.T) {
	c := testClient()
	c.DOIBase = "http://127.0.0.1:1" // would fail if dialed
	if _, err := c.FetchDOI(context.Background(), "banana"); !errors.Is(err, ErrInvalidDOI) {
		t.Errorf("err = %v, want ErrInvalidDOI", err)
	}
}

// TestExtractReadable_BodyFallback: a page with no article/main root
// still extracts from body.
func TestExtractReadable_BodyFallback(t *testing.T) {
	md, title, err := ExtractReadable(
		`<html><head><title>Bare Page</title></head><body><p>Hello content world.</p></body></html>`,
		"fallback")
	if err != nil {
		t.Fatalf("ExtractReadable: %v", err)
	}
	if !strings.Contains(md, "Hello content world.") {
		t.Errorf("markdown = %q", md)
	}
	if title != "Bare Page" {
		t.Errorf("title = %q, want Bare Page", title)
	}
}

// TestExtractReadable_Empty errors on a page with no text so the
// pipeline never sees a zero-chunk body from this path.
func TestExtractReadable_Empty(t *testing.T) {
	if _, _, err := ExtractReadable(`<html><body><script>x()</script></body></html>`, "t"); err == nil {
		t.Error("expected error for empty extraction")
	}
}

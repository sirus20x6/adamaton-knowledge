package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

func quietLogger() *logrus.Logger {
	lg := logrus.New()
	lg.SetOutput(io.Discard)
	return lg
}

// newSourcesRouter registers the sources routes with nil pool/embedder/
// chunker/blobs — safe for tests that only exercise validation branches
// which return before any of those are touched.
func newSourcesRouter() *mux.Router {
	r := mux.NewRouter()
	RegisterSources(r, nil, nil, nil, nil, quietLogger())
	return r
}

func postIngest(t *testing.T, r *mux.Router, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/platform/sources/ingest", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestSourcesIngest_InvalidDOI: a doi: target that isn't a DOI is a 400
// before any network or DB I/O (the 501 this endpoint used to return is
// gone).
func TestSourcesIngest_InvalidDOI(t *testing.T) {
	rec := postIngest(t, newSourcesRouter(), `{"target":"doi:banana"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid DOI") {
		t.Errorf("body = %s, want invalid DOI message", rec.Body.String())
	}
}

// TestSourcesIngest_InvalidURLScheme: url: targets must be absolute
// http(s). ftp/file/relative are 400.
func TestSourcesIngest_InvalidURLScheme(t *testing.T) {
	r := newSourcesRouter()
	for _, target := range []string{
		`url:ftp://example.com/x.pdf`,
		`url:file:///etc/passwd`,
		`url:/relative/only`,
	} {
		rec := postIngest(t, r, `{"target":"`+target+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("target %q: status = %d, want 400; body: %s", target, rec.Code, rec.Body.String())
		}
	}
}

// TestSourcesIngest_DoiUrlNoLonger501 pins the card's acceptance
// criterion: the doi/url schemes must not return 501 anymore.
func TestSourcesIngest_DoiUrlNoLonger501(t *testing.T) {
	r := newSourcesRouter()
	for _, body := range []string{
		`{"target":"doi:banana"}`,
		`{"target":"url:not a url"}`,
	} {
		rec := postIngest(t, r, body)
		if rec.Code == http.StatusNotImplemented {
			t.Errorf("body %s: still returns 501", body)
		}
	}
}

// TestSourcesIngest_UnsupportedUpstreamContent: a url: target that
// serves a content type the pipeline can't parse maps to 400 (caller
// error — they pointed us at a zip), not 502.
func TestSourcesIngest_UnsupportedUpstreamContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write([]byte("PK\x03\x04 definitely a zip"))
	}))
	defer srv.Close()

	rec := postIngest(t, newSourcesRouter(), `{"target":"url:`+srv.URL+`/archive.zip"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unsupported content type") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// TestSourcesIngest_URLFetchFailure: unreachable upstream maps to 502.
func TestSourcesIngest_URLFetchFailure(t *testing.T) {
	// Reserved-but-closed port: dial fails fast.
	rec := postIngest(t, newSourcesRouter(), `{"target":"url:http://127.0.0.1:9/nope"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body: %s", rec.Code, rec.Body.String())
	}
}

// TestSourcesIngest_UnknownScheme is unchanged behaviour but worth
// pinning next to the new branches.
func TestSourcesIngest_UnknownScheme(t *testing.T) {
	rec := postIngest(t, newSourcesRouter(), `{"target":"gopher:something"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

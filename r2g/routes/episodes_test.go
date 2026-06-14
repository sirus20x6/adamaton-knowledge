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

// TestEpisodes_PostValidation covers the request-shape contract on
// POST /v3/episodes. These rejections happen before any store access,
// so a nil pool/embedder is safe — the handler must never reach them.
func TestEpisodes_PostValidation(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard) // silence

	r := mux.NewRouter()
	RegisterEpisodes(r, nil, nil, logger)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing content", `{"kind":"chunk"}`, http.StatusBadRequest},
		{"missing kind", `{"content":"hi"}`, http.StatusBadRequest},
		{"empty both", `{}`, http.StatusBadRequest},
		{"invalid json", `{not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v3/episodes", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

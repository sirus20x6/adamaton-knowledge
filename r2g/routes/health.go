// Package routes registers HTTP handlers on a gorilla/mux router. One
// file per logical endpoint family (documents, chunks, search, etc.).
// During the strangler-fig migration, only routes that have been
// ported live here — everything Caddy doesn't route to r2g still hits
// the Python R2R container.
package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// HealthDeps names the optional sidecars that /ready probes. nil
// entries mean "not configured, don't probe" — matching the
// startup behaviour where r2g can run without an embedder or
// LLM if the operator only wants the read-only routes live.
type HealthDeps struct {
	BGEURL string // probed with GET /healthz
	LLMURL string // probed with GET /v1/models (OpenAI shape)
}

// RegisterHealth wires /health and /ready. /health is a process-liveness
// check (always 200 if the binary is up); /ready additionally pings
// postgres and each configured sidecar, returning a per-component
// status map so a load balancer (Caddy, or whatever else fronts r2g)
// can distinguish "process up but degraded" from "fully serving".
//
// Mirrors the convention used by skills-worker / reindex-worker but
// adds the per-component map so the dashboard's /api/v1/system/status
// proxy can show fine-grained state.
func RegisterHealth(r *mux.Router, pool *pgxpool.Pool, deps HealthDeps, logger *logrus.Logger) {
	r.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}).Methods(http.MethodGet)

	r.HandleFunc("/ready", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
		defer cancel()

		checks := map[string]string{}
		anyDown := false

		// Run the probes in parallel — one slow sidecar shouldn't
		// pile-up the response. A failed probe records the error
		// string; ok is the literal "ok".
		var wg sync.WaitGroup
		var mu sync.Mutex
		report := func(name string, err error) {
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				checks[name] = "ok"
				return
			}
			checks[name] = err.Error()
			anyDown = true
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			report("postgres", pool.Ping(ctx))
		}()
		if deps.BGEURL != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				report("bge_sidecar", httpProbe(ctx, deps.BGEURL+"/healthz"))
			}()
		}
		if deps.LLMURL != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				report("llm", httpProbe(ctx, deps.LLMURL+"/models"))
			}()
		}
		wg.Wait()

		status := http.StatusOK
		body := map[string]any{
			"status": "ready",
			"checks": checks,
		}
		if anyDown {
			status = http.StatusServiceUnavailable
			body["status"] = "degraded"
			logger.WithField("checks", checks).Warn("/ready: degraded")
		}
		writeJSON(w, status, body)
	}).Methods(http.MethodGet)
}

// httpProbe does a quick GET; non-2xx + transport failures both
// surface as errors. Used by /ready against sidecar liveness paths.
func httpProbe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &httpProbeError{url: url, code: resp.StatusCode}
	}
	return nil
}

type httpProbeError struct {
	url  string
	code int
}

func (e *httpProbeError) Error() string {
	return e.url + ": HTTP " + http.StatusText(e.code)
}

// writeJSON is a shared helper for endpoint handlers in this package.
// Sets the Content-Type, writes the status code, encodes the body.
// Errors during encoding are logged-via-handler-context, not returned
// — the response status has already been written by the time encoding
// fails so there's nothing structured the caller can do.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

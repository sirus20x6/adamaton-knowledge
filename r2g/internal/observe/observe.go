// Package observe wires r2g's HTTP middleware stack: structured
// request-scoped logging, Prometheus latency + counter metrics, and
// a request-ID header that round-trips into structured log fields.
//
// One package because the middleware shares state (request-id key
// type, prometheus collectors) and is always installed together --
// the convention from the dashboard's apiserver.
package observe

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
)

// ctxKey is unexported to avoid collisions with other packages
// stashing things in context.Context.
type ctxKey struct{ name string }

var requestIDKey = ctxKey{"request_id"}

// WithRequestID stashes id on the context. Exported so other
// packages (extract, ingest) can pull it for nested log fields.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID retrieves the current request id, or "" if unset.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// LoggerFromContext returns a logrus entry with the request_id
// field already attached. Call this from handlers when logging
// something request-scoped; the formatter keeps the field consistent.
func LoggerFromContext(ctx context.Context, base *logrus.Logger) *logrus.Entry {
	if id := RequestID(ctx); id != "" {
		return base.WithField("request_id", id)
	}
	return logrus.NewEntry(base)
}

// Metrics holds the Prometheus collectors r2g exposes. Construct
// once at startup with NewMetrics(reg); the middleware closes over
// the instance.
type Metrics struct {
	requestDuration *prometheus.HistogramVec
	requestTotal    *prometheus.CounterVec
	inFlight        prometheus.Gauge
	sidecarErrors   *prometheus.CounterVec
}

// NewMetrics registers the standard collector set under the given
// registry. Histogram buckets target the latency distribution we
// expect:
//   - read-only endpoints (documents GET, chunks GET): 5-50ms p99
//   - search: 50-500ms p99 (embed roundtrip dominates)
//   - ingest: seconds (LaTeX + embed batches)
//   - extract: tens of seconds (LLM dominates)
func NewMetrics(reg prometheus.Registerer) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		requestDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "r2g",
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "HTTP request handler duration.",
			Buckets: []float64{
				0.001, 0.005, 0.01, 0.025, 0.05, 0.1,
				0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
			},
		}, []string{"route", "method", "status_class"}),
		requestTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "r2g",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "HTTP requests by route + status.",
		}, []string{"route", "method", "status"}),
		inFlight: factory.NewGauge(prometheus.GaugeOpts{
			Namespace: "r2g",
			Subsystem: "http",
			Name:      "in_flight",
			Help:      "Currently-executing HTTP handlers.",
		}),
		sidecarErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Namespace: "r2g",
			Subsystem: "sidecar",
			Name:      "errors_total",
			Help:      "Sidecar (embeddings, llm, latex2text, pdftotext) failures by kind.",
		}, []string{"sidecar", "kind"}),
	}
}

// SidecarError increments the per-sidecar failure counter. Called
// from internal/embeddings, internal/llm, internal/parsing on
// non-2xx / transport failure. kind is a short tag ("timeout",
// "http_5xx", "decode") so dashboards can pivot.
func (m *Metrics) SidecarError(sidecar, kind string) {
	if m == nil {
		return
	}
	m.sidecarErrors.WithLabelValues(sidecar, kind).Inc()
}

// statusRecorder wraps a ResponseWriter to capture the status code
// for the metrics + logger middleware. WriteHeader is the only
// fall-through; net/http's tooling assumes 200 when nothing was
// explicitly written.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware composes request-id, structured logging, and metrics
// into a single net/http middleware. Apply once at the root of the
// router; downstream handlers see the wired context + ResponseWriter.
//
// The route label uses gorilla/mux's path template (e.g.
// "/v3/documents/{id}") not the literal URL, so cardinality stays
// bounded -- one bucket per route, not per document.
func Middleware(m *Metrics, base *logrus.Logger) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-Id", id)
			ctx := WithRequestID(r.Context(), id)

			rec := &statusRecorder{ResponseWriter: w, status: 200}
			start := time.Now()
			m.inFlight.Inc()
			defer m.inFlight.Dec()

			route := routeTemplate(r)

			next.ServeHTTP(rec, r.WithContext(ctx))

			dur := time.Since(start)
			statusClass := fmt.Sprintf("%dxx", rec.status/100)
			m.requestDuration.WithLabelValues(route, r.Method, statusClass).Observe(dur.Seconds())
			m.requestTotal.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()

			lvl := logrus.InfoLevel
			if rec.status >= 500 {
				lvl = logrus.ErrorLevel
			} else if rec.status >= 400 {
				lvl = logrus.WarnLevel
			}
			base.WithFields(logrus.Fields{
				"request_id":    id,
				"method":        r.Method,
				"path":          r.URL.Path,
				"route":         route,
				"status":        rec.status,
				"duration_ms":   dur.Milliseconds(),
				"remote":        r.RemoteAddr,
			}).Log(lvl, "request")
		})
	}
}

// routeTemplate returns the mux pattern for the matched route,
// falling back to the literal path when nothing matched. Keeps
// metric cardinality bounded.
func routeTemplate(r *http.Request) string {
	if route := mux.CurrentRoute(r); route != nil {
		if t, err := route.GetPathTemplate(); err == nil {
			return t
		}
	}
	return r.URL.Path
}

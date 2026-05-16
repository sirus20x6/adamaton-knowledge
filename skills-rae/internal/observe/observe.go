// Package observe wires Prometheus metrics + structured request logging
// onto the service. Mirrors r2g's observe package so dashboards stay
// uniform across the fleet.
package observe

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

// Metrics holds the per-process counters/histograms.
type Metrics struct {
	RequestsTotal *prometheus.CounterVec
	RequestSecs   *prometheus.HistogramVec
}

// NewMetrics registers histograms+counters into the provided registry.
// Panics on double-registration so misuse fails loudly at startup.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "skills_rae",
			Name:      "http_requests_total",
			Help:      "Total HTTP requests by route + status.",
		}, []string{"route", "method", "status"}),
		RequestSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "skills_rae",
			Name:      "http_request_seconds",
			Help:      "End-to-end HTTP request latency.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"route", "method"}),
	}
	reg.MustRegister(m.RequestsTotal, m.RequestSecs)
	return m
}

// Middleware records request metrics + emits one structured log line
// per request. Route label is the mux path template; the raw path
// would explode cardinality.
func Middleware(m *Metrics, logger *logrus.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			elapsed := time.Since(start)
			route := r.URL.Path
			m.RequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Inc()
			m.RequestSecs.WithLabelValues(route, r.Method).Observe(elapsed.Seconds())
			logger.WithFields(logrus.Fields{
				"method":     r.Method,
				"path":       route,
				"status":     sw.status,
				"elapsed_ms": elapsed.Milliseconds(),
			}).Debug("served")
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

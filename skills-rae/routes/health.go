// Package routes wires HTTP handlers onto the gorilla/mux router. Each
// file in this package owns one endpoint family. Health is the only one
// that lands in Phase A; /v1/rae/* lands in phases D and E.
package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// RegisterHealth wires GET /healthz. It probes the postgres pool with a
// 2-second deadline so an unhealthy DB shows up as a 503 instead of a
// hang. Used by docker-compose healthcheck + the dashboard's system
// status panel.
func RegisterHealth(r *mux.Router, pool *pgxpool.Pool, logger *logrus.Logger) {
	r.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		status := "ok"
		dbOK := true
		if err := pool.Ping(ctx); err != nil {
			logger.WithError(err).Warn("healthz: db ping failed")
			status = "degraded"
			dbOK = false
		}
		body := map[string]any{
			"status":  status,
			"service": "skills-rae",
			"db":      dbOK,
		}
		w.Header().Set("Content-Type", "application/json")
		if !dbOK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(body)
	}).Methods(http.MethodGet)
}

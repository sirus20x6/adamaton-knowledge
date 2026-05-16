package main

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
)

// traceRecorder implements llm.Recorder by writing one evo.llm_traces
// row per LLM call. Best-effort: failures are logged at debug and
// never bubble up — r2g extraction stays decoupled from the
// nano-research migration's lifecycle, so an unapplied 0007 just
// silently drops trace rows.
//
// Sessions: one llm_sessions row per process, lazily inserted on first
// Record call. External ID is the process PID + start time, which is
// enough to chain a single r2g run's extraction LLM calls without
// coordinating with anything else.
type traceRecorder struct {
	pool      *pgxpool.Pool
	modelID   string
	logger    *logrus.Logger
	sessionID uuid.UUID
	bootOnce  bool
}

func newTraceRecorder(pool *pgxpool.Pool, modelID string, logger *logrus.Logger) *traceRecorder {
	return &traceRecorder{pool: pool, modelID: modelID, logger: logger, sessionID: uuid.New()}
}

func (r *traceRecorder) Record(ctx context.Context, prompt, response, model string, inputTok, outputTok int) {
	if r == nil || r.pool == nil {
		return
	}
	// Fire-and-forget on a fresh ctx; the original may be tied to the
	// HTTP request that's about to return. 5s budget is plenty.
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if !r.bootOnce {
			_, err := r.pool.Exec(c, `
				INSERT INTO evo.llm_sessions (id, kind, external_id)
				VALUES ($1, 'r2g-extract', $2) ON CONFLICT (id) DO NOTHING`,
				r.sessionID, r.sessionID.String())
			if err != nil {
				r.logger.WithError(err).Debug("trace: session insert failed (drop)")
				return
			}
			r.bootOnce = true
		}
		_, err := r.pool.Exec(c, `
			INSERT INTO evo.llm_traces
			  (session_id, model_id, stage, prompt, response, input_tokens, output_tokens, metadata)
			VALUES ($1, $2, 'r2g-extract', $3, $4, NULLIF($5,0), NULLIF($6,0), '{}')`,
			r.sessionID, r.modelID, prompt, response, inputTok, outputTok)
		if err != nil {
			r.logger.WithError(err).Debug("trace: row insert failed (drop)")
		}
	}()
}

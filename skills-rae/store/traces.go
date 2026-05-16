package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InsertTrace writes one row to evo.skill_rae_traces. Best-effort
// telemetry — callers should swallow errors so a trace-write failure
// never blocks the compile response. Variant is "rae" for normal
// calls, "flat" for the (future) parallel telemetry from the legacy
// FormatSkillsBlock path when that gets traced server-side.
func InsertTrace(ctx context.Context, pool *pgxpool.Pool,
	taskID, query, variant string,
	selectedSkills []uuid.UUID,
	rescuedCount, elapsedMs int,
) error {
	if pool == nil {
		return fmt.Errorf("nil pool")
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO evo.skill_rae_traces
		    (task_id, query, variant, selected_skills, rescued_count, elapsed_ms)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		taskID, query, variant, selectedSkills, rescuedCount, elapsedMs)
	return err
}

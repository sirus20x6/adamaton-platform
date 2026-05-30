package health

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgWorkflowFailureSource is the production WorkflowFailureSource: it reads
// the workflow-run store (workflow.runs) to tally failed / completed /
// running executions over rolling windows. The apiserver's workflowstore
// writes a row per workflow run and flips status to "failed" /
// "completed" / "cancelled" with finished_at stamped when the run reaches
// a terminal state (see workflow_endpoints.go:getRun), so a windowed
// count over finished_at is the failure gauge without any extra
// bookkeeping.
//
// It shares the apiserver's evo pool — the same pool the postgres /
// temporal_queue probers use — so no new connection is opened. When the
// pool is nil the source reports an error and the gauge degrades to
// "unknown" rather than failing the whole health snapshot.
type PgWorkflowFailureSource struct {
	Pool *pgxpool.Pool
}

// workflowFailureSQL tallies the failure gauge in a single round-trip. A
// run is "failed" if its terminal status is one of the failure-ish
// states; "completed" if it finished cleanly; "running" if it has not
// finished. The windows are anchored on finished_at for terminal rows and
// on the row simply lacking finished_at for in-flight ones.
//
// status values mirror what getRun persists: "failed" (Temporal Failed /
// TimedOut / Terminated), "completed", "cancelled", and the initial
// "running". We count "cancelled" as neither a failure nor a success — a
// cancel is operator intent, not a fault.
const workflowFailureSQL = `
SELECT
  count(*) FILTER (
    WHERE status = 'failed'
      AND finished_at IS NOT NULL
      AND finished_at > NOW() - INTERVAL '1 hour'
  ) AS failed_1h,
  count(*) FILTER (
    WHERE status = 'completed'
      AND finished_at IS NOT NULL
      AND finished_at > NOW() - INTERVAL '1 hour'
  ) AS completed_1h,
  count(*) FILTER (
    WHERE status = 'running' OR finished_at IS NULL
  ) AS running_now,
  count(*) FILTER (
    WHERE status = 'failed'
      AND finished_at IS NOT NULL
      AND finished_at > NOW() - INTERVAL '24 hours'
  ) AS failed_24h
FROM workflow.runs`

// WorkflowFailureCounts runs the tally. The 3s timeout keeps a slow or
// locked workflow.runs from stalling the whole health refresh.
func (s *PgWorkflowFailureSource) WorkflowFailureCounts(ctx context.Context) (WorkflowFailureCounts, error) {
	if s == nil || s.Pool == nil {
		return WorkflowFailureCounts{}, errNoWorkflowPool
	}
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var c WorkflowFailureCounts
	err := s.Pool.QueryRow(queryCtx, workflowFailureSQL).Scan(
		&c.FailedLastHour, &c.CompletedLastHour, &c.RunningNow, &c.FailedLast24h,
	)
	if err != nil {
		return WorkflowFailureCounts{}, err
	}
	return c, nil
}

// errNoWorkflowPool is returned when the source has no pool; surfaced as
// the gauge's Error field, leaving Status=unknown.
var errNoWorkflowPool = workflowPoolErr("workflow failure source has no postgres pool")

type workflowPoolErr string

func (e workflowPoolErr) Error() string { return string(e) }

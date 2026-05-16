// Package activities holds the Temporal activity implementations for
// the dispatch subsystem. Two activity structs:
//
//   - SelectActivities: SQL into evo.workers, ranks candidates by
//     current load + freshness.
//   - RecordActivities: durable ledger writes against evo.jobs as a
//     job moves through pending → assigned → running → terminal.
//
// All activities take an injected *pgxpool.Pool and a *logrus.Logger;
// the worker constructs them once and registers them with Temporal at
// startup (see dispatch/cmd/dispatch-worker/registrations.go).
package activities

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

// SelectActivities is the activity struct that owns candidate-selection
// queries against evo.workers. The DispatchWorkflow calls
// SelectCandidates exactly once per dispatch attempt; the returned list
// is already ranked (best first) and the workflow picks Candidates[0].
type SelectActivities struct {
	Pool   *pgxpool.Pool
	Logger *logrus.Logger
}

// SelectInput is an alias for the canonical type in dispatch/workflows;
// the workflow and activity share the same input shape and we keep the
// source of truth in one place to avoid drift.
type SelectInput = workflows.SelectInput

// selectCandidatesSQL is the candidate-ranking query. Filters live in
// the WHERE clause; load + freshness fall out of the SELECT list and
// drive the ORDER BY. Parameters (in order):
//
//	$1 req.QueueClass       text
//	$2 req.RequiresGPU      bool
//	$3 req.CPUArch          text
//	$4 req.MinRAMGB         int
//	$5 req.MinVRAMGB        int
//	$6 req.GPUFamily        text
//	$7 req.CPUFeatures      text[]   (NOT NULL — pass {} to disable)
//	$8 req.Permissions      text[]   (NOT NULL — pass {} to disable)
//
// load_score is a correlated subquery counting in-flight jobs for the
// candidate worker; it's expected to be tiny (evo.jobs is bounded and
// the jobs_assigned_worker_idx partial index makes the count cheap).
// We keep it as-is rather than denormalising onto evo.workers — the
// JOIN-free count avoids a 1:N reverse join from jobs.
const selectCandidatesSQL = `
SELECT w.id, $1::text AS queue,
       COALESCE((
         SELECT count(*) FROM evo.jobs j
         WHERE j.assigned_worker = w.id AND j.status IN ('assigned','running')
       ), 0)::int AS load_score,
       EXTRACT(EPOCH FROM (now() - w.last_heartbeat))::int AS staleness_s
FROM evo.workers w
WHERE w.status = 'active'
  AND w.last_heartbeat > now() - interval '90 seconds'
  AND 'execute' = ANY(w.permissions)
  AND $1 = ANY(w.declared_queues)
  AND ($2::bool = false OR w.gpu_model IS NOT NULL)
  AND ($3 = '' OR w.cpu_arch = $3)
  AND ($4::int = 0 OR COALESCE(w.ram_gb, 0) >= $4)
  AND ($5::int = 0 OR COALESCE(w.gpu_vram_gb, 0) >= $5)
  AND ($6 = '' OR w.gpu_model ILIKE '%' || $6 || '%')
  AND (cardinality($7::text[]) = 0 OR $7::text[] <@ w.cpu_features)
  AND (cardinality($8::text[]) = 0 OR $8::text[] <@ w.permissions)
ORDER BY load_score ASC, w.last_heartbeat DESC
LIMIT 5
`

// SelectCandidates runs the ranking query and returns up to 5 matching
// workers. An empty slice (with nil error) means "no workers match"
// — the workflow handles that case by marking the job 'no_workers'.
func (a *SelectActivities) SelectCandidates(ctx context.Context, in SelectInput) ([]workflows.Candidate, error) {
	req := in.Requirements

	features := req.CPUFeatures
	if features == nil {
		features = []string{}
	}
	perms := req.Permissions
	if perms == nil {
		perms = []string{}
	}

	rows, err := a.Pool.Query(ctx, selectCandidatesSQL,
		req.QueueClass,
		req.RequiresGPU,
		req.CPUArch,
		req.MinRAMGB,
		req.MinVRAMGB,
		req.GPUFamily,
		features,
		perms,
	)
	if err != nil {
		return nil, fmt.Errorf("select candidates: %w", err)
	}
	defer rows.Close()

	out := make([]workflows.Candidate, 0, 5)
	for rows.Next() {
		var c workflows.Candidate
		if err := rows.Scan(&c.WorkerID, &c.Queue, &c.LoadScore, &c.StalenessS); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}

	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"queue_class": req.QueueClass,
			"n":           len(out),
		}).Debug("SelectCandidates")
	}
	return out, nil
}

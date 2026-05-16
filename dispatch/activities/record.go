package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamomaton-platform/dispatch/workflows"
)

// RecordActivities is the activity struct that owns durable writes to
// the evo.jobs ledger. Three methods cover the lifecycle:
//
//	RecordJob    — INSERT a 'pending' row at submission time
//	AssignWorker — UPDATE assigned_worker/queue/workflow_id, status='assigned'
//	MarkJobStatus — UPDATE status (running / succeeded / failed / no_workers)
//
// The activity is intentionally thin — no business logic, just SQL.
// Errors are returned bare so Temporal's retry policy can re-attempt
// transient pgxpool failures; the workflow is the only place that
// decides what to do when a write keeps failing.
type RecordActivities struct {
	Pool   *pgxpool.Pool
	Logger *logrus.Logger
}

// Activity input/output types are declared in dispatch/workflows so
// both packages can share them without forming an import cycle. The
// type aliases below give activities-package consumers the familiar
// short names while keeping the source of truth in workflows/types.go.
type (
	RecordJobInput  = workflows.RecordJobInput
	RecordJobResult = workflows.RecordJobResult
	AssignInput     = workflows.AssignInput
	MarkStatusInput = workflows.MarkStatusInput
)

// recordJobSQL inserts the pending ledger row. NULLIF($7,'') maps an
// empty submitted_by string to NULL so the column stays clean when the
// submitter is the dashboard rather than another worker.
const recordJobSQL = `
INSERT INTO evo.jobs (id, kind, spec, requirements, batch_size, priority, status, submitted_by)
VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, $6, 'pending', NULLIF($7,''))
RETURNING id, created_at
`

// RecordJob inserts the pending evo.jobs row and returns the
// authoritative id + created_at.
func (a *RecordActivities) RecordJob(ctx context.Context, in RecordJobInput) (*RecordJobResult, error) {
	specJSON, err := json.Marshal(in.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	reqJSON, err := json.Marshal(in.Spec.Requirements)
	if err != nil {
		return nil, fmt.Errorf("marshal requirements: %w", err)
	}
	batchSize := in.Spec.BatchSize
	if batchSize <= 0 {
		batchSize = 1
	}

	var (
		outID     string
		createdAt time.Time
	)
	row := a.Pool.QueryRow(ctx, recordJobSQL,
		in.JobID,
		in.Spec.Kind,
		string(specJSON),
		string(reqJSON),
		batchSize,
		in.Spec.Priority,
		in.SubmittedBy,
	)
	if err := row.Scan(&outID, &createdAt); err != nil {
		return nil, fmt.Errorf("insert evo.jobs: %w", err)
	}

	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"job_id": outID,
			"kind":   in.Spec.Kind,
		}).Debug("RecordJob")
	}
	return &RecordJobResult{JobID: outID, CreatedAt: createdAt}, nil
}

const assignWorkerSQL = `
UPDATE evo.jobs
   SET assigned_worker = $1, assigned_queue = $2, status = 'assigned',
       assigned_at = NOW(), workflow_id = $3, workflow_run_id = $4, updated_at = NOW()
 WHERE id = $5 AND status = 'pending'
`

// AssignWorker stamps the chosen worker + queue + child workflow IDs on
// the pending ledger row, moving it to 'assigned'. The UPDATE is guarded
// to only apply when the row is still in 'pending' — if the row has
// already advanced (e.g. retried after a terminal-state write), we
// surface a non-retryable error rather than silently re-assigning.
func (a *RecordActivities) AssignWorker(ctx context.Context, in AssignInput) error {
	tag, err := a.Pool.Exec(ctx, assignWorkerSQL,
		in.AssignedWorker,
		in.AssignedQueue,
		in.WorkflowID,
		in.WorkflowRunID,
		in.JobID,
	)
	if err != nil {
		return fmt.Errorf("update evo.jobs (assign): %w", err)
	}
	if tag.RowsAffected() == 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("job not in pending state: id=%s", in.JobID),
			"NotPending", nil,
		)
	}
	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"job_id": in.JobID,
			"worker": in.AssignedWorker,
			"queue":  in.AssignedQueue,
		}).Debug("AssignWorker")
	}
	return nil
}

// markJobStatusSQL refuses to overwrite a terminal status. The
// `status NOT IN (...)` guard makes the UPDATE a no-op when the row
// has already been written to its final state, which protects against
// late activity retries that would otherwise regress a 'succeeded' or
// 'failed' job back to 'running'.
const markJobStatusSQL = `
UPDATE evo.jobs SET status = $1, updated_at = NOW()
 WHERE id = $2 AND status NOT IN ('succeeded','failed','no_workers')
`

// MarkJobStatus mutates evo.jobs.status. Used by DispatchWorkflow to
// flip the row through 'running' (when the child workflow has started)
// and to its terminal state ('succeeded' | 'failed' | 'no_workers').
// A no-op UPDATE (terminal-state guard tripped, OR row missing) is
// returned as a non-retryable error so Temporal doesn't burn retries
// trying to make a sticky state un-stuck.
func (a *RecordActivities) MarkJobStatus(ctx context.Context, in MarkStatusInput) error {
	tag, err := a.Pool.Exec(ctx, markJobStatusSQL, in.Status, in.JobID)
	if err != nil {
		return fmt.Errorf("update evo.jobs (status): %w", err)
	}
	if tag.RowsAffected() == 0 {
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("job row missing or already terminal: id=%s", in.JobID),
			"TerminalOrMissing", nil,
		)
	}
	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"job_id": in.JobID,
			"status": in.Status,
		}).Debug("MarkJobStatus")
	}
	return nil
}

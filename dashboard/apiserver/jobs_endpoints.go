// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

// Job dispatch endpoints. /jobs and /jobs/{id} are read-only views over
// evo.jobs (the dispatch ledger); /jobs/submit kicks off a fresh
// DispatchWorkflow by string name. The DispatchWorkflow owns the
// evo.jobs INSERT — this handler just calls ExecuteWorkflow and
// returns a job_id the caller can poll on.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	dispatchworkflows "github.com/sirus20x6/adamomaton-platform/dispatch/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// Job is the wire shape returned by /api/v1/jobs and /api/v1/jobs/{id}.
// Spec / Requirements are kept as json.RawMessage so the dashboard
// doesn't have to track every workflow-specific payload shape — the
// front-end renders them as pretty-printed JSON in the detail drawer.
type Job struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Spec           json.RawMessage `json:"spec"`
	Requirements   json.RawMessage `json:"requirements"`
	BatchSize      int             `json:"batch_size"`
	Priority       int             `json:"priority"`
	Status         string          `json:"status"`
	SubmittedBy    *string         `json:"submitted_by,omitempty"`
	AssignedWorker *string         `json:"assigned_worker,omitempty"`
	AssignedQueue  *string         `json:"assigned_queue,omitempty"`
	AssignedAt     *time.Time      `json:"assigned_at,omitempty"`
	WorkflowID     *string         `json:"workflow_id,omitempty"`
	WorkflowRunID  *string         `json:"workflow_run_id,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// JobSubmitResponse is the 202 body for POST /jobs/submit. status_url
// points at the dispatch-workflow polling endpoint. The dispatch
// workflow generates its own evo.jobs row server-side; the apiserver
// no longer mints a local job_id (the old client-side UUID was never
// threaded through to the workflow, so polling by it returned 404).
// The job_id field is retained on the wire for backward compatibility
// and is set equal to dispatch_workflow_id.
type JobSubmitResponse struct {
	JobID              string `json:"job_id"`
	DispatchWorkflowID string `json:"dispatch_workflow_id"`
	StatusURL          string `json:"status_url"`
	AlreadyRunning     bool   `json:"already_running,omitempty"`
}

// registerJobsEndpoints mounts the read views + the submit endpoint.
// server.go wires this into the /api/v1 subrouter alongside the other
// registerXEndpoints calls.
func (s *APIServer) registerJobsEndpoints(api *mux.Router) {
	api.HandleFunc("/jobs", s.listJobs).Methods("GET")
	api.HandleFunc("/jobs/submit", s.submitJob).Methods("POST")
	api.HandleFunc("/jobs/{id}", s.getJob).Methods("GET")
}

const jobsSelectSQL = `
SELECT id, kind, spec, requirements, batch_size, priority, status,
       submitted_by, assigned_worker, assigned_queue, assigned_at,
       workflow_id, workflow_run_id, created_at, updated_at
FROM evo.jobs
`

func (s *APIServer) listJobs(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	qb := strings.Builder{}
	qb.WriteString(jobsSelectSQL)
	qb.WriteString(" WHERE 1=1")
	args := []interface{}{}
	placeholder := func(v interface{}) string {
		args = append(args, v)
		return "$" + itoa(len(args))
	}
	// Filter param values are user-controlled — cap each to 256 chars
	// before threading into the query builder. Anything longer is
	// either malformed or hostile; either way 256 is well past any
	// real status / kind / worker identifier we mint.
	capParam := func(v string) string {
		if len(v) > 256 {
			return v[:256]
		}
		return v
	}
	if v := capParam(strings.TrimSpace(r.URL.Query().Get("status"))); v != "" {
		qb.WriteString(" AND status = " + placeholder(v))
	}
	if v := capParam(strings.TrimSpace(r.URL.Query().Get("worker"))); v != "" {
		qb.WriteString(" AND assigned_worker = " + placeholder(v))
	}
	if v := capParam(strings.TrimSpace(r.URL.Query().Get("kind"))); v != "" {
		qb.WriteString(" AND kind = " + placeholder(v))
	}
	limit, offset := parseLimitOffset(r, 100, 500, 100000)
	qb.WriteString(" ORDER BY created_at DESC LIMIT " + placeholder(limit) + " OFFSET " + placeholder(offset))

	rows, err := s.evoPool.Query(ctx, qb.String(), args...)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

func (s *APIServer) getJob(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row := s.evoPool.QueryRow(ctx, jobsSelectSQL+` WHERE id = $1`, id)
	j, err := scanJob(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "job not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
		return
	}
	writeEvoJSON(w, j)
}

// submitJob accepts a dispatchworkflows.JobSpec on the wire and kicks
// off a DispatchWorkflow. The dispatch workflow owns the evo.jobs
// INSERT (and mints the real job_id server-side); the apiserver only
// validates the spec shape, hands it off, and returns 202 with a
// polling URL anchored on the Temporal workflow ID.
//
// SubmittedBy is forced to "dashboard" so a client can't spoof which
// surface kicked off the work — the dashboard is the only legitimate
// caller of this endpoint, and the workflow uses SubmittedBy for
// audit-style metadata.
func (s *APIServer) submitJob(w http.ResponseWriter, r *http.Request) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}

	// Bound the request body — JobSpec.Payload is opaque to us but a
	// runaway POST shouldn't tank the apiserver. 1MB is generous for
	// any reasonable workflow input.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var spec dispatchworkflows.JobSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	spec.Kind = strings.TrimSpace(spec.Kind)
	spec.Requirements.QueueClass = strings.TrimSpace(spec.Requirements.QueueClass)
	if spec.Kind == "" {
		writeEvoErr(w, http.StatusBadRequest, "kind is required")
		return
	}
	if spec.Requirements.QueueClass == "" {
		writeEvoErr(w, http.StatusBadRequest, "requirements.queue_class is required")
		return
	}
	// Force the audit field regardless of what the client sent —
	// /jobs/submit is the dashboard's surface and submitters from
	// other origins should use their own MCP / API path.
	spec.SubmittedBy = "dashboard"

	// Temporal workflow ID for the dispatch workflow. The dispatch
	// workflow internally generates its own job_id (and uses that as
	// the evo.jobs primary key), so we don't mint one here — there's
	// no way to thread it through and the previous code's local UUID
	// returned 404s when polled. Use a fresh UUID for workflow-ID
	// uniqueness; callers poll by dispatch_workflow_id.
	dispatchWorkflowID := "dispatch-" + uuid.New().String()

	// Fire-and-forget against Temporal: 10s context so a wedged frontend
	// doesn't pin us indefinitely, but we don't tie scheduling to
	// r.Context() — the workflow is owned by Temporal once accepted, and
	// a client hangup shouldn't cancel it.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := s.temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        dispatchWorkflowID,
		TaskQueue: dispatchworkflows.TaskQueue,
	}, dispatchworkflows.WorkflowDispatch, spec)
	if err != nil {
		// WorkflowExecutionAlreadyStarted is an idempotent success —
		// the caller asked us to schedule a workflow already running
		// under the same ID. Return 202 with already_running=true
		// instead of escalating into the 500 retry-storm path.
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			s.logger.WithField("workflow_id", dispatchWorkflowID).
				Info("submitJob: DispatchWorkflow already running — returning existing workflow ID")
			writeEvoJSONStatus(w, http.StatusAccepted, JobSubmitResponse{
				JobID:              dispatchWorkflowID,
				DispatchWorkflowID: dispatchWorkflowID,
				StatusURL:          "/api/v1/jobs/dispatch/" + dispatchWorkflowID,
				AlreadyRunning:     true,
			})
			return
		}
		s.logger.WithError(err).WithField("workflow_id", dispatchWorkflowID).
			Error("submitJob: failed to start DispatchWorkflow")
		writeEvoErr(w, http.StatusInternalServerError, "execute workflow: "+err.Error())
		return
	}

	writeEvoJSONStatus(w, http.StatusAccepted, JobSubmitResponse{
		JobID:              dispatchWorkflowID,
		DispatchWorkflowID: dispatchWorkflowID,
		StatusURL:          "/api/v1/jobs/dispatch/" + dispatchWorkflowID,
	})
}

// scanJob keeps the column list aligned between list + get. Spec /
// Requirements are scanned as raw bytes and re-wrapped as RawMessage
// so the JSON encoder copies them through untouched.
func scanJob(rs rowScanner) (Job, error) {
	var j Job
	var specBytes, reqBytes []byte
	if err := rs.Scan(
		&j.ID, &j.Kind, &specBytes, &reqBytes, &j.BatchSize, &j.Priority, &j.Status,
		&j.SubmittedBy, &j.AssignedWorker, &j.AssignedQueue, &j.AssignedAt,
		&j.WorkflowID, &j.WorkflowRunID, &j.CreatedAt, &j.UpdatedAt,
	); err != nil {
		return Job{}, err
	}
	if len(specBytes) > 0 {
		j.Spec = json.RawMessage(specBytes)
	} else {
		j.Spec = json.RawMessage("null")
	}
	if len(reqBytes) > 0 {
		j.Requirements = json.RawMessage(reqBytes)
	} else {
		j.Requirements = json.RawMessage("null")
	}
	return j, nil
}
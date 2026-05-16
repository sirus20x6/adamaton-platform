// Package workflows holds the Temporal workflow definitions for the
// dispatch subsystem: a pre-dispatch router (DispatchWorkflow) and a
// signal-driven batch coordinator (BatchCoordinator).
//
// Every job submission to POST /api/v1/jobs/submit becomes a
// DispatchWorkflow run keyed by the submission's job_id (UUID). The
// workflow:
//
//	1. RecordJob   — INSERT evo.jobs row with status='pending'
//	2. SelectCandidates — SQL query against evo.workers with capability
//	   filters from the JobSpec.Requirements struct
//	3. AssignWorker — stamp assigned_worker + assigned_queue
//	4. ExecuteChildWorkflow(spec.Kind, spec.Payload) on the chosen queue
//	   — OR signal a BatchCoordinator when BatchSize > 1
//
// Workflow IDs are deterministic so re-trigger semantics are obvious:
//   dispatch-{job_id}                 — one per submission
//   batch-{queue}-{batch_size}        — one long-lived per (queue, size)
package workflows

import (
	"encoding/json"
	"time"
)

// TaskQueue is the Temporal task queue this module's worker polls.
// Every DispatchWorkflow + BatchCoordinator runs on this queue
// regardless of where the underlying child workflow ends up.
const TaskQueue = "dispatch"

const (
	WorkflowDispatch         = "DispatchWorkflow"
	WorkflowBatchCoordinator = "BatchCoordinator"

	// SignalJob is the signal a DispatchWorkflow sends to a
	// BatchCoordinator to enqueue a job into its buffer. Payload: JobSpec.
	SignalJob = "job"

	// SignalBatchResult is the reply signal a BatchCoordinator sends
	// back to the originating DispatchWorkflow once its batch fires.
	// Payload: BatchSlotResult.
	SignalBatchResult = "batch-result"

	// QueryProgress is exposed by long-running coordinators so the
	// dashboard can poll without affecting workflow state.
	QueryProgress = "progress"
)

// Job status constants. These are the canonical string values written
// to evo.jobs.status throughout the dispatch flow; use these instead of
// stringly-typed literals so the compiler catches typos.
const (
	StatusPending    = "pending"
	StatusAssigned   = "assigned"
	StatusRunning    = "running"
	StatusSucceeded  = "succeeded"
	StatusFailed     = "failed"
	StatusNoWorkers  = "no_workers"
)

// JobSpec is the wire shape submitted via POST /api/v1/jobs/submit
// and the input to DispatchWorkflow. Crypto-related fields are nullable
// so the v1 path produces valid JobSpecs without a signing layer.
type JobSpec struct {
	// Kind is the registered name of the target workflow to execute,
	// e.g. "EvolutionWorkflow", "SyncSkillToR2RWorkflow". The
	// DispatchWorkflow runs this as an ExecuteChildWorkflow on the
	// chosen queue, passing Payload as the sole positional argument.
	Kind string `json:"kind"`

	// Payload is passed through verbatim to the target workflow.
	// json.RawMessage so dispatch never has to know the target's input
	// shape — that's between the submitter and the worker.
	Payload json.RawMessage `json:"payload"`

	Requirements Requirements `json:"requirements"`

	// BatchSize: 0 or 1 = immediate one-shot dispatch. N > 1 routes
	// through a BatchCoordinator that collects up to N JobSpecs before
	// firing as a single batched child workflow.
	BatchSize int `json:"batch_size,omitempty"`

	// BatchMaxAge: the batch coordinator fires when buffer is full OR
	// when the oldest enqueued JobSpec is older than this. Defaults to
	// 60s when BatchSize > 1 and BatchMaxAge is zero.
	BatchMaxAge time.Duration `json:"batch_max_age,omitempty"`

	// Priority: higher = preferred during candidate selection ties.
	// Default 0. Negative values are valid (deprioritized).
	Priority int `json:"priority,omitempty"`

	// SubmittedBy: worker_id of the submitter for in-stack submissions
	// ("evo-worker@pi" etc.); empty when the dashboard is the
	// submitter. Future crypto layer pins this to a pubkey.
	SubmittedBy string `json:"submitted_by,omitempty"`

	// SignedAttestation is reserved for the future signed-job-spec
	// layer. Empty in v1; the SelectCandidates SQL doesn't filter on
	// it. When set, it'll be a detached signature over
	// (kind, payload, requirements, batch_size).
	SignedAttestation string `json:"signed_attestation,omitempty"`
}

// Requirements is the capability filter the dispatcher uses to pick
// workers. v1 matching is loose: queue class + GPU presence + RAM/VRAM
// thresholds + CPU arch are checked; CPUFeatures and Permissions are
// advisory (workers may run jobs they don't strictly match) until the
// v2 signing layer makes the claim trustable.
type Requirements struct {
	// QueueClass: the worker's declared_queues[] must contain this.
	// Required — empty string is invalid.
	QueueClass string `json:"queue_class"`

	MinRAMGB    int  `json:"min_ram_gb,omitempty"`
	MinVRAMGB   int  `json:"min_vram_gb,omitempty"`
	RequiresGPU bool `json:"requires_gpu,omitempty"`

	// GPUFamily: substring match against worker.gpu_model in v1.
	// "blackwell" matches "NVIDIA RTX Pro 6000 Blackwell"; "ampere"
	// matches "NVIDIA A100"; empty disables the check.
	GPUFamily string `json:"gpu_family,omitempty"`

	// CPUArch: exact match against runtime.GOARCH on the worker side
	// (amd64 | arm64 | riscv64 | ...). Empty disables the check.
	CPUArch string `json:"cpu_arch,omitempty"`

	// CPUFeatures: worker.cpu_features[] must be a superset.
	// Example: ["avx2"], ["neon"], ["avx512f", "avx512vl"].
	CPUFeatures []string `json:"cpu_features,omitempty"`

	// Permissions: worker.permissions[] must contain all of these.
	// Default in v1 is implicit ["execute"]; admin-only routes might
	// pass ["execute", "admin"] later.
	Permissions []string `json:"permissions,omitempty"`
}

// DispatchResult is the workflow's return value. Visible via
// GetWorkflow().Get() and surfaced by the dashboard's
// /api/v1/jobs/{id} endpoint.
type DispatchResult struct {
	JobID          string `json:"job_id"`
	AssignedWorker string `json:"assigned_worker,omitempty"`
	AssignedQueue  string `json:"assigned_queue,omitempty"`
	ChildWorkflow  string `json:"child_workflow_id,omitempty"`
	ChildRunID     string `json:"child_run_id,omitempty"`
	Status         string `json:"status"` // "succeeded" | "failed" | "no_workers"
	Error          string `json:"error,omitempty"`
}

// BatchCoordinatorInput is the input for a per-(queue, batch_size)
// long-running coordinator workflow.
type BatchCoordinatorInput struct {
	Queue       string        `json:"queue"`
	BatchSize   int           `json:"batch_size"`
	BatchMaxAge time.Duration `json:"batch_max_age"`
}

// BatchSlotResult is the reply payload the coordinator signals back
// to a waiting DispatchWorkflow once its batch has fired.
type BatchSlotResult struct {
	BatchID       string `json:"batch_id"`
	ChildWorkflow string `json:"child_workflow_id"`
	ChildRunID    string `json:"child_run_id"`
	SlotIndex     int    `json:"slot_index"`
	Error         string `json:"error,omitempty"`
}

// BatchProgress is what BatchCoordinator's QueryProgress handler
// returns — used by /api/v1/jobs to surface live batch state.
type BatchProgress struct {
	Queue          string    `json:"queue"`
	BatchSize      int       `json:"batch_size"`
	Buffered       int       `json:"buffered"`         // currently waiting in the buffer
	BatchesFired   int       `json:"batches_fired"`    // cumulative
	OldestEnqueued time.Time `json:"oldest_enqueued,omitempty"`
	StartedAt      time.Time `json:"started_at"`
}

// Candidate is the activity-return shape from SelectCandidates. The
// workflow picks Candidates[0] (the SQL already ORDER BYs by best
// match), then calls AssignWorker with the chosen one.
type Candidate struct {
	WorkerID   string `json:"worker_id"`
	Queue      string `json:"queue"`
	LoadScore  int    `json:"load_score"`
	StalenessS int    `json:"staleness_s"`
}

// Activity input/output types live in this package (alongside the
// workflow definitions) rather than in dispatch/activities so the
// workflow can pass typed structs without forming an import cycle —
// activities already imports this package for JobSpec, Requirements,
// etc. The activity package re-uses these types directly on its method
// signatures.

// RecordJobInput is the input for the RecordJob activity. The workflow
// generates the JobID via SideEffect (deterministic across retries) and
// passes the canonical UUID string here.
type RecordJobInput struct {
	JobID       string  `json:"job_id"`
	Spec        JobSpec `json:"spec"`
	SubmittedBy string  `json:"submitted_by"`
}

// RecordJobResult is what RecordJob returns. JobID echoes the input
// (handy when the workflow logs the activity's return value); CreatedAt
// is the row's authoritative timestamp from Postgres.
type RecordJobResult struct {
	JobID     string    `json:"job_id"`
	CreatedAt time.Time `json:"created_at"`
}

// AssignInput is the input for the AssignWorker activity. WorkflowID
// is the child workflow's deterministic ID ("child-{jobID}");
// WorkflowRunID can be empty when the row is stamped before the child
// workflow starts (the dispatcher fills it in via a follow-up update
// if needed).
type AssignInput struct {
	JobID          string `json:"job_id"`
	AssignedWorker string `json:"assigned_worker"`
	AssignedQueue  string `json:"assigned_queue"`
	WorkflowID     string `json:"workflow_id"`
	WorkflowRunID  string `json:"workflow_run_id"`
}

// MarkStatusInput is the input for the MarkJobStatus activity. Status
// is one of the StatusRunning / StatusSucceeded / StatusFailed /
// StatusNoWorkers constants — RecordJob already covers the pending
// case and AssignWorker covers assigned.
type MarkStatusInput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// SelectInput is the activity input for SelectCandidates. Wraps the
// JobSpec's Requirements so the activity is a pure function of its
// inputs (no implicit context lookups).
type SelectInput struct {
	Requirements Requirements `json:"requirements"`
}

// EnsureBatchCoordinatorInput is the input for the
// EnsureBatchCoordinator activity. (Queue, BatchSize) uniquely identifies
// the coordinator workflow ID; BatchMaxAge is forwarded as the
// coordinator's input only on the cold start — once a coordinator is
// running, its existing BatchMaxAge wins. Envelope is the first job to
// enqueue into the coordinator: piggy-backing it on the
// SignalWithStartWorkflow eliminates the race between "start the
// coordinator" and "deliver the first job".
type EnsureBatchCoordinatorInput struct {
	Queue       string        `json:"queue"`
	BatchSize   int           `json:"batch_size"`
	BatchMaxAge time.Duration `json:"batch_max_age"`
	Envelope    BatchEnvelope `json:"envelope"`
}

// EnsureBatchCoordinatorResult echoes back the coordinator's workflow
// ID and run ID so the caller can log the coordinator it ended up
// addressing. RunID is "" if the workflow was already running.
type EnsureBatchCoordinatorResult struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

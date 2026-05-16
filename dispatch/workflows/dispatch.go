package workflows

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Activity names registered by the dispatch-worker. We reference
// activities by string here so the workflow package doesn't have to
// import dispatch/activities (which would form a cycle — activities
// imports workflows for the shared input/output structs).
const (
	activityRecordJob              = "RecordJob"
	activityAssignWorker           = "AssignWorker"
	activityMarkJobStatus          = "MarkJobStatus"
	activitySelectCandidates       = "SelectCandidates"
	activityEnsureBatchCoordinator = "EnsureBatchCoordinator"
)

// replyTimeoutMultiplier bounds how long DispatchWorkflow waits for the
// coordinator's reply signal. We sleep for up to BatchMaxAge*this — long
// enough for the coordinator to flush a partial batch plus a small fudge
// for child workflow startup — before declaring the coordinator dead
// and failing the job.
const replyTimeoutMultiplier = 4

// replyTimeoutFloor is the minimum reply window, used when BatchMaxAge
// is 0 / unset on the JobSpec. Keeps a dispatcher from giving up in
// milliseconds against a coordinator that hasn't even started ticking.
const replyTimeoutFloor = 10 * time.Minute

// dispatchActivityOptions is the shared retry policy for all dispatch
// activity calls. 10s start-to-close is plenty for a Postgres write or
// the candidate-ranking query; 3 attempts on a 5s/2x/30s schedule
// covers transient pgxpool reconnect blips without burning workflow
// history on a permanently-down database.
func dispatchActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
}

// DispatchWorkflow is the entry point for every POST /api/v1/jobs/submit.
// It records the job in evo.jobs, selects a candidate worker, stamps
// the assignment, and either:
//
//   - launches a child workflow directly on the chosen queue (one-shot
//     dispatch when BatchSize <= 1), or
//   - signals a long-running BatchCoordinator and awaits a reply
//     signal carrying the slot result (BatchSize > 1).
//
// Returns *DispatchResult on success. On "no workers match" the
// workflow returns a non-retryable application error AND a populated
// DispatchResult so the caller can see the rejection reason in the
// dashboard even when GetWorkflow().Get errors.
func DispatchWorkflow(ctx workflow.Context, spec JobSpec) (*DispatchResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("DispatchWorkflow start", "kind", spec.Kind, "batch_size", spec.BatchSize)

	ctx = workflow.WithActivityOptions(ctx, dispatchActivityOptions())

	// Generate a stable job UUID. SideEffect guarantees the value is
	// the same across workflow replays — uuid.New() inside the
	// workflow body would re-roll on every history rebuild.
	var jobID string
	if err := workflow.SideEffect(ctx, func(workflow.Context) interface{} {
		return uuid.New().String()
	}).Get(&jobID); err != nil {
		return nil, err
	}

	res := &DispatchResult{JobID: jobID}

	// 1. RecordJob — INSERT the pending ledger row.
	var rec RecordJobResult
	recordIn := RecordJobInput{
		JobID:       jobID,
		Spec:        spec,
		SubmittedBy: spec.SubmittedBy,
	}
	if err := workflow.ExecuteActivity(ctx, activityRecordJob, recordIn).Get(ctx, &rec); err != nil {
		logger.Warn("RecordJob failed", "error", err)
		return res, err
	}

	// 2. SelectCandidates — rank workers matching spec.Requirements.
	var candidates []Candidate
	selectIn := SelectInput{
		Requirements: spec.Requirements,
	}
	if err := workflow.ExecuteActivity(ctx, activitySelectCandidates, selectIn).Get(ctx, &candidates); err != nil {
		logger.Warn("SelectCandidates failed", "error", err)
		return res, err
	}

	// 3. Reject "no workers match" with a non-retryable error so the
	//    dashboard surfaces it as a permanent dispatch failure rather
	//    than spinning forever on retries.
	if len(candidates) == 0 {
		_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
			JobID:  jobID,
			Status: StatusNoWorkers,
		}).Get(ctx, nil)
		res.Status = StatusNoWorkers
		res.Error = "no workers match requirements"
		return res, temporal.NewNonRetryableApplicationError(
			"no workers match requirements", "NoWorkers", nil,
		)
	}

	chosen := candidates[0]
	childWFID := "child-" + jobID
	res.AssignedWorker = chosen.WorkerID
	res.AssignedQueue = chosen.Queue
	res.ChildWorkflow = childWFID

	// 4. AssignWorker — stamp the row with the chosen worker + queue.
	assignIn := AssignInput{
		JobID:          jobID,
		AssignedWorker: chosen.WorkerID,
		AssignedQueue:  chosen.Queue,
		WorkflowID:     childWFID,
		WorkflowRunID:  "",
	}
	if err := workflow.ExecuteActivity(ctx, activityAssignWorker, assignIn).Get(ctx, nil); err != nil {
		logger.Warn("AssignWorker failed", "error", err)
		return res, err
	}

	// 5a. Batched path — route through the BatchCoordinator.
	if spec.BatchSize > 1 {
		return runBatched(ctx, spec, chosen, jobID, res)
	}

	// 5b. One-shot path — launch the child workflow directly on the
	//     chosen worker's queue. The child's own retry policy is its
	//     concern; the parent uses default options so a child failure
	//     surfaces immediately rather than being retried at this level.
	childOpts := workflow.ChildWorkflowOptions{
		WorkflowID:               childWFID,
		TaskQueue:                chosen.Queue,
		WorkflowExecutionTimeout: 24 * time.Hour,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}
	childCtx := workflow.WithChildOptions(ctx, childOpts)

	// Decode spec.Payload into a generic map BEFORE passing to
	// ExecuteChildWorkflow. spec.Payload is json.RawMessage = []byte;
	// Temporal's default payload converter chain picks the
	// ByteSlicePayloadConverter (encoding/binary) BEFORE the JSON
	// converter, so passing the bytes directly delivers a `binary/plain`
	// argument that the typed child workflow can't unmarshal back into
	// its struct input. json.Unmarshal is deterministic so calling it
	// in workflow code is fine.
	var childArg map[string]any
	if len(spec.Payload) > 0 {
		if err := json.Unmarshal(spec.Payload, &childArg); err != nil {
			_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
				JobID:  jobID,
				Status: StatusFailed,
			}).Get(ctx, nil)
			res.Status = StatusFailed
			res.Error = "invalid payload JSON: " + err.Error()
			return res, temporal.NewNonRetryableApplicationError(
				"invalid payload JSON", "BadPayload", err,
			)
		}
	}

	// Mark running once we have the child's ID. This is best-effort:
	// if it fails we still proceed to wait on the child.
	_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
		JobID:  jobID,
		Status: StatusRunning,
	}).Get(ctx, nil)

	childFuture := workflow.ExecuteChildWorkflow(childCtx, spec.Kind, childArg)

	// Wait for the child's execution to start so we can capture its
	// run id. Then wait for the result.
	var childExec workflow.Execution
	if err := childFuture.GetChildWorkflowExecution().Get(ctx, &childExec); err == nil {
		res.ChildRunID = childExec.RunID
	}

	var childResult interface{}
	childErr := childFuture.Get(ctx, &childResult)
	if childErr != nil {
		_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
			JobID:  jobID,
			Status: StatusFailed,
		}).Get(ctx, nil)
		res.Status = StatusFailed
		res.Error = childErr.Error()
		logger.Warn("DispatchWorkflow child failed", "error", childErr)
		return res, childErr
	}

	_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
		JobID:  jobID,
		Status: StatusSucceeded,
	}).Get(ctx, nil)
	res.Status = StatusSucceeded
	logger.Info("DispatchWorkflow done",
		"job_id", jobID,
		"worker", chosen.WorkerID,
		"child", childWFID,
	)
	return res, nil
}

// runBatched is the BatchSize>1 path. The DispatchWorkflow uses
// EnsureBatchCoordinator (a SignalWithStartWorkflow wrapper) to
// atomically start the coordinator if it isn't running AND deliver the
// first envelope. The coordinator buffers envelopes until the batch is
// full OR the BatchMaxAge timer fires, then runs a single batched
// child workflow on the chosen queue and signals each origin back with
// a BatchSlotResult.
func runBatched(ctx workflow.Context, spec JobSpec, chosen Candidate, jobID string, res *DispatchResult) (*DispatchResult, error) {
	logger := workflow.GetLogger(ctx)

	originID := workflow.GetInfo(ctx).WorkflowExecution.ID
	envelope := BatchEnvelope{Spec: spec, OriginWorkflowID: originID, JobID: jobID}

	// Atomically (1) start the BatchCoordinator if it isn't running
	// and (2) deliver the first envelope as a SignalJob. The activity
	// performs the SignalWithStartWorkflow — that's a client-side API
	// not callable from workflow code directly.
	var ensureRes EnsureBatchCoordinatorResult
	ensureIn := EnsureBatchCoordinatorInput{
		Queue:       chosen.Queue,
		BatchSize:   spec.BatchSize,
		BatchMaxAge: spec.BatchMaxAge,
		Envelope:    envelope,
	}
	if err := workflow.ExecuteActivity(ctx, activityEnsureBatchCoordinator, ensureIn).Get(ctx, &ensureRes); err != nil {
		logger.Warn("ensure batch coordinator failed", "queue", chosen.Queue, "error", err)
		_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
			JobID:  jobID,
			Status: StatusFailed,
		}).Get(ctx, nil)
		res.Status = StatusFailed
		res.Error = err.Error()
		return res, err
	}

	// Mark running while we wait on the coordinator's reply.
	_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
		JobID:  jobID,
		Status: StatusRunning,
	}).Get(ctx, nil)

	// Wait for the coordinator's reply signal, OR a timeout. The
	// signal name is per-job so multiple in-flight dispatches in the
	// same workflow (shouldn't happen — but defence in depth) can be
	// told apart. A dead coordinator otherwise wedges this future
	// forever; the timer caps the wait at replyTimeoutMultiplier *
	// BatchMaxAge (or replyTimeoutFloor, whichever is larger).
	replyCh := workflow.GetSignalChannel(ctx, SignalBatchResult+"-"+jobID)

	replyWindow := spec.BatchMaxAge * replyTimeoutMultiplier
	if replyWindow < replyTimeoutFloor {
		replyWindow = replyTimeoutFloor
	}
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	timer := workflow.NewTimer(timerCtx, replyWindow)

	var slot BatchSlotResult
	var timedOut bool
	selector := workflow.NewSelector(ctx)
	selector.AddReceive(replyCh, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, &slot)
	})
	selector.AddFuture(timer, func(f workflow.Future) {
		// Get() the timer future so a CanceledError doesn't leak.
		_ = f.Get(ctx, nil)
		timedOut = true
	})
	selector.Select(ctx)

	if timedOut {
		_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
			JobID:  jobID,
			Status: StatusFailed,
		}).Get(ctx, nil)
		msg := "timeout waiting for batch coordinator reply"
		res.Status = StatusFailed
		res.Error = msg
		logger.Warn("DispatchWorkflow batch reply timeout",
			"job_id", jobID,
			"coordinator", ensureRes.WorkflowID,
			"window", replyWindow.String(),
		)
		return res, temporal.NewNonRetryableApplicationError(msg, "BatchReplyTimeout", nil)
	}

	res.ChildWorkflow = slot.ChildWorkflow
	res.ChildRunID = slot.ChildRunID
	if slot.Error != "" {
		_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
			JobID:  jobID,
			Status: StatusFailed,
		}).Get(ctx, nil)
		res.Status = StatusFailed
		res.Error = slot.Error
		return res, temporal.NewApplicationError(slot.Error, "BatchSlotFailed")
	}

	_ = workflow.ExecuteActivity(ctx, activityMarkJobStatus, MarkStatusInput{
		JobID:  jobID,
		Status: StatusSucceeded,
	}).Get(ctx, nil)
	res.Status = StatusSucceeded
	logger.Info("DispatchWorkflow batched done",
		"job_id", jobID,
		"coordinator", ensureRes.WorkflowID,
		"batch_id", slot.BatchID,
	)
	return res, nil
}

// BatchEnvelope is the wire shape DispatchWorkflow signals to a
// BatchCoordinator. The coordinator can't reach back to a dispatch by
// JobSpec alone — JobSpec has no identity — so we wrap each signal
// with the origin workflow's ID and the dispatch-assigned JobID. The
// coordinator uses OriginWorkflowID to SignalExternalWorkflow the
// reply and JobID to name the reply signal channel.
//
// Lives in the workflows package (not activities) because both
// dispatch.go and batch.go need to reference it.
type BatchEnvelope struct {
	Spec             JobSpec `json:"spec"`
	OriginWorkflowID string  `json:"origin_workflow_id"`
	JobID            string  `json:"job_id"`
}

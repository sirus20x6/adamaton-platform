package workflows

// TrainExperimentWorkflow drives a single experiment from launch to
// terminal status. The apiserver creates the platform.experiments row
// (status=pending) and signals here with experiment_id + dataset_version
// + split + the remote train cmd; this workflow:
//
//  1. ExperimentMarkRunning  — stamp status=running + workflow_id
//  2. DispatchTrainOnBlackwell — ssh to blackwell, run cmd, scp metrics.json
//  3. IngestExperimentMetrics — parse metrics.json, bulk INSERT
//  4. ExperimentFinalize — stamp status=succeeded|failed + val_bpb + finished_at
//
// Step 2 failure flips status=failed via step 4 with finalErr surfaced;
// step 3 failure is non-fatal (we still finalize, just with metrics_count=0
// and a notes append) since "training ran but we couldn't parse metrics"
// is a degraded-but-not-broken outcome the operator may want to inspect.
//
// All activities live in platform/temporal/activities; the worker
// (cmd/adamaton-worker reg_experiments.go) wires the deps + task queue.

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamaton-platform/temporal/activities"
	"github.com/sirus20x6/adamaton-platform/temporal/errclass"
)

// TaskQueueTrainExperiment is the Temporal task queue this workflow uses.
// The apiserver passes the same constant to ExecuteWorkflow so the queue
// name stays consistent across the binary boundary.
const TaskQueueTrainExperiment = "experiments-train"

// TrainExperimentInput is the apiserver-supplied payload. ExperimentID
// MUST already exist in platform.experiments (caller INSERTed it). Cmd
// is the argv vector executed remotely; DatasetVersionID + Split are
// passed through to the script as env / args at the caller's choice.
type TrainExperimentInput struct {
	ExperimentID     string   `json:"experiment_id"`
	DatasetVersionID string   `json:"dataset_version_id,omitempty"`
	Split            string   `json:"split,omitempty"`
	Cmd              []string `json:"cmd"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
}

// TrainExperimentOutput is the terminal-state report. PointsIngested is
// the count of platform.experiment_metrics rows the IngestMetrics
// activity inserted. ValBPB is the final scalar pulled out of metrics.json
// (the last 'val_bpb' sample if present, else nil).
type TrainExperimentOutput struct {
	ExperimentID   string   `json:"experiment_id"`
	Status         string   `json:"status"` // succeeded | failed
	ExitCode       int      `json:"exit_code"`
	PointsIngested int      `json:"points_ingested"`
	ValBPB         *float64 `json:"val_bpb,omitempty"`
	StderrTail     string   `json:"stderr_tail,omitempty"`
}

func TrainExperimentWorkflow(ctx workflow.Context, in TrainExperimentInput) (*TrainExperimentOutput, error) {
	if in.ExperimentID == "" {
		return nil, fmt.Errorf("TrainExperimentWorkflow: experiment_id required")
	}
	if len(in.Cmd) == 0 {
		return nil, fmt.Errorf("TrainExperimentWorkflow: cmd required")
	}

	// Default activity options. DispatchTrain has its own override below
	// because training runs can legitimately last hours.
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	})

	info := workflow.GetInfo(ctx)

	// Method-by-reflection dispatch: a nil pointer is enough for the SDK
	// to derive the activity name; the registered *ExperimentDeps instance
	// on the worker side receives the call.
	var a *activities.ExperimentDeps

	// Step 1 — mark running + stamp workflow_id. Best-effort: if this
	// fails (DB down) we still try to dispatch; the operator just sees
	// status=pending while the run is live.
	_ = workflow.ExecuteActivity(ctx, a.ExperimentMarkRunning, activities.ExperimentMarkRunningInput{
		ExperimentID: in.ExperimentID,
		WorkflowID:   info.WorkflowExecution.ID,
	}).Get(ctx, nil)

	// Step 2 — dispatch. Long timeout, single attempt (training isn't
	// idempotent — retrying a partial GPU run wastes hours and can corrupt
	// metrics.json mid-write).
	dispatchCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Hour,
		HeartbeatTimeout:    90 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	})
	var dispatch activities.DispatchTrainOutput
	dispatchErr := workflow.ExecuteActivity(dispatchCtx, a.DispatchTrainOnBlackwell, activities.DispatchTrainInput{
		ExperimentID:     in.ExperimentID,
		DatasetVersionID: in.DatasetVersionID,
		Split:            in.Split,
		Cmd:              in.Cmd,
		TimeoutSeconds:   in.TimeoutSeconds,
	}).Get(dispatchCtx, &dispatch)

	// Step 3 — ingest metrics (only if dispatch produced a metrics file).
	// Failure here is non-fatal; finalize still runs so the row reflects
	// terminal state.
	var ingest activities.IngestMetricsOutput
	if dispatchErr == nil && dispatch.MetricsJSON != "" {
		if err := workflow.ExecuteActivity(ctx, a.IngestExperimentMetrics, activities.IngestMetricsInput{
			ExperimentID: in.ExperimentID,
			MetricsJSON:  dispatch.MetricsJSON,
		}).Get(ctx, &ingest); err != nil {
			workflow.GetLogger(ctx).Warn("ingest metrics failed; finalizing without points",
				"experiment_id", in.ExperimentID, "error", err.Error())
		}
	}

	// Step 4 — finalize. status mirrors dispatch outcome, val_bpb is
	// the last val_bpb sample from the ingested metrics (may be nil).
	status := "succeeded"
	if dispatchErr != nil || dispatch.ExitCode != 0 {
		status = "failed"
	}
	finalizeInput := activities.ExperimentFinalizeInput{
		ExperimentID: in.ExperimentID,
		Status:       status,
		ValBPB:       ingest.ValBPB,
		ExitCode:     dispatch.ExitCode,
		StderrTail:   dispatch.StderrTail,
	}
	if dispatchErr != nil {
		finalizeInput.StderrTail = dispatchErr.Error()
	}
	if err := workflow.ExecuteActivity(ctx, a.ExperimentFinalize, finalizeInput).Get(ctx, nil); err != nil {
		workflow.GetLogger(ctx).Warn("finalize failed; experiment row may be stuck in running",
			"experiment_id", in.ExperimentID, "error", err.Error())
	}

	out := &TrainExperimentOutput{
		ExperimentID:   in.ExperimentID,
		Status:         status,
		ExitCode:       dispatch.ExitCode,
		PointsIngested: ingest.PointsIngested,
		ValBPB:         ingest.ValBPB,
		StderrTail:     dispatch.StderrTail,
	}
	if dispatchErr != nil {
		workflow.GetLogger(ctx).Error("TrainExperimentWorkflow failed",
			"experiment_id", in.ExperimentID,
			"error", dispatchErr,
			"error_class", errclass.RecordWorkflowFailure(ctx, "TrainExperimentWorkflow", dispatchErr),
		)
		return out, dispatchErr
	}
	return out, nil
}

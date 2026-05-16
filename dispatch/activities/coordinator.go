package activities

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamomaton-platform/dispatch/workflows"
)

// CoordinatorActivities wraps the Temporal client so the DispatchWorkflow
// can ensure a BatchCoordinator is running before signalling it. The
// workflow itself cannot call client.SignalWithStartWorkflow directly —
// that's a non-deterministic operation — so we wrap it in an activity.
type CoordinatorActivities struct {
	Client client.Client
	Logger *logrus.Logger
}

// Activity input/output types live in dispatch/workflows to avoid an
// import cycle (this activity needs workflows.BatchEnvelope; the
// workflow needs the input shape on the other side of the call).
type (
	EnsureBatchCoordinatorInput  = workflows.EnsureBatchCoordinatorInput
	EnsureBatchCoordinatorResult = workflows.EnsureBatchCoordinatorResult
)

// EnsureBatchCoordinator starts a BatchCoordinator for (queue, batch_size)
// if one isn't already running, and atomically delivers a JobSignal to
// it. Uses SignalWithStartWorkflow so the first job posted to a (queue,
// batch_size) doesn't lose a race against a not-yet-started coordinator.
//
// WorkflowIDReusePolicy is ALLOW_DUPLICATE_FAILED_ONLY: we want
// SignalWithStartWorkflow to attach to a still-running coordinator, but
// if a previous coordinator terminated with success/failure we want to
// re-create it rather than skip the signal silently.
func (a *CoordinatorActivities) EnsureBatchCoordinator(ctx context.Context, in EnsureBatchCoordinatorInput) (*EnsureBatchCoordinatorResult, error) {
	if a.Client == nil {
		return nil, fmt.Errorf("EnsureBatchCoordinator: temporal client not configured")
	}
	if in.Queue == "" {
		return nil, fmt.Errorf("EnsureBatchCoordinator: queue is required")
	}
	if in.BatchSize <= 1 {
		return nil, fmt.Errorf("EnsureBatchCoordinator: batch_size must be > 1")
	}

	coordinatorID := fmt.Sprintf("batch-%s-%d", in.Queue, in.BatchSize)

	opts := client.StartWorkflowOptions{
		ID:                       coordinatorID,
		TaskQueue:                workflows.TaskQueue,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		WorkflowExecutionTimeout: 0, // long-running; bounded by continue-as-new
	}

	coordInput := workflows.BatchCoordinatorInput{
		Queue:       in.Queue,
		BatchSize:   in.BatchSize,
		BatchMaxAge: in.BatchMaxAge,
	}

	we, err := a.Client.SignalWithStartWorkflow(
		ctx,
		coordinatorID,
		workflows.SignalJob,
		in.Envelope,
		opts,
		workflows.WorkflowBatchCoordinator,
		coordInput,
	)
	if err != nil {
		return nil, fmt.Errorf("signal-with-start coordinator %s: %w", coordinatorID, err)
	}

	if a.Logger != nil {
		a.Logger.WithFields(logrus.Fields{
			"coordinator": coordinatorID,
			"run_id":      we.GetRunID(),
			"job_id":      in.Envelope.JobID,
		}).Debug("EnsureBatchCoordinator")
	}
	return &EnsureBatchCoordinatorResult{
		WorkflowID: coordinatorID,
		RunID:      we.GetRunID(),
	}, nil
}

package workflows

import (
	"fmt"
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"
)

// continueAsNewEvery bounds the BatchCoordinator's history size by
// continuing-as-new after this many batches have fired. The exact
// number matters less than "small enough that the history snapshot
// stays cheap, large enough that we're not continuing every minute" —
// 1000 batches at 60s of dwell time is roughly a 17-hour run, which
// keeps continue-as-new traffic on the dashboard sparse.
const continueAsNewEvery = 1000

// defaultBatchMaxAge is the fallback when the first JobSpec to land on
// a coordinator omits BatchMaxAge. 60s is what JobSpec docs promise.
const defaultBatchMaxAge = 60 * time.Second

// BatchCoordinator is a long-running per-(queue, batch_size) workflow
// that buffers JobSpec envelopes from DispatchWorkflow signals and
// fires a single batched child workflow per N envelopes or when the
// oldest buffered envelope reaches BatchMaxAge.
//
// Lifecycle:
//
//  1. Receive a BatchEnvelope on SignalJob → append to buffer; if it's
//     the first envelope, start the staleness timer.
//  2. When buffer reaches BatchSize OR the staleness timer fires →
//     execute a child workflow named "BatchedJob" on in.Queue with the
//     buffered envelopes' specs as the payload.
//  3. After the child completes, signal each envelope's
//     OriginWorkflowID with SignalBatchResult-{jobID} carrying a
//     BatchSlotResult.
//  4. Empty the buffer; loop.
//  5. Every continueAsNewEvery batches, return ContinueAsNewError to
//     keep history bounded.
//
// The workflow has no top-level retry policy — it's meant to run
// indefinitely. Individual activity calls retry per their own options.
func BatchCoordinator(ctx workflow.Context, in BatchCoordinatorInput) error {
	logger := workflow.GetLogger(ctx)

	if in.BatchMaxAge <= 0 {
		in.BatchMaxAge = defaultBatchMaxAge
	}
	if in.BatchSize <= 1 {
		// Defensive: BatchSize 1 wouldn't make sense here, but treat
		// as "fire every envelope individually" rather than wedging.
		in.BatchSize = 1
	}

	progress := &BatchProgress{
		Queue:     in.Queue,
		BatchSize: in.BatchSize,
		StartedAt: workflow.Now(ctx),
	}

	// Register the query handler so the dashboard can poll buffer
	// state without affecting workflow execution.
	if err := workflow.SetQueryHandler(ctx, QueryProgress, func() (*BatchProgress, error) {
		// Return a copy so callers can't mutate workflow state.
		snapshot := *progress
		return &snapshot, nil
	}); err != nil {
		return err
	}

	jobCh := workflow.GetSignalChannel(ctx, SignalJob)

	logger.Info("BatchCoordinator start",
		"queue", in.Queue,
		"batch_size", in.BatchSize,
		"batch_max_age", in.BatchMaxAge.String(),
	)

	var buffer []BatchEnvelope
	var batchOldestAt time.Time

	for {
		// Build a fresh selector each iteration. Timer futures
		// can't be cancelled cleanly mid-loop without re-deriving the
		// context, so this is the simplest correct pattern.
		selector := workflow.NewSelector(ctx)

		var receivedEnvelope BatchEnvelope
		var receivedAny bool
		selector.AddReceive(jobCh, func(c workflow.ReceiveChannel, more bool) {
			c.Receive(ctx, &receivedEnvelope)
			receivedAny = true
		})

		// Only arm the staleness timer when there's something in
		// the buffer — otherwise we'd burn timer futures spinning
		// against an empty buffer.
		var timerFired bool
		if len(buffer) > 0 {
			elapsed := workflow.Now(ctx).Sub(batchOldestAt)
			remaining := in.BatchMaxAge - elapsed
			if remaining < 0 {
				remaining = 0
			}
			t := workflow.NewTimer(ctx, remaining)
			selector.AddFuture(t, func(f workflow.Future) {
				timerFired = true
			})
		}

		selector.Select(ctx)

		if receivedAny {
			buffer = append(buffer, receivedEnvelope)
			if len(buffer) == 1 {
				batchOldestAt = workflow.Now(ctx)
				progress.OldestEnqueued = batchOldestAt
			}
			progress.Buffered = len(buffer)
		}

		// Fire conditions: buffer full, or timer says the oldest
		// envelope has waited long enough. The "len > 0 && timer"
		// branch covers partial batches that aged out.
		fire := len(buffer) >= in.BatchSize ||
			(timerFired && len(buffer) > 0)

		if !fire {
			continue
		}

		// Drain the buffer into a fresh slice we hand to the child;
		// reset the live buffer immediately so any signal received
		// during the child's execution goes into the next batch.
		drained := buffer
		buffer = nil
		batchOldestAt = time.Time{}
		progress.Buffered = 0
		progress.OldestEnqueued = time.Time{}

		batchIndex := progress.BatchesFired
		// Include a slice of the RunID in the batch ID so the index
		// counter resetting after ContinueAsNew doesn't collide with
		// child workflow IDs from a previous run of the coordinator
		// — Temporal's ALLOW_DUPLICATE_FAILED_ONLY would otherwise
		// reject the reused ID and wedge the coordinator.
		info := workflow.GetInfo(ctx)
		runIDFragment := info.WorkflowExecution.RunID
		if len(runIDFragment) > 8 {
			runIDFragment = runIDFragment[:8]
		}
		batchID := fmt.Sprintf("%s-%s-%d", info.WorkflowExecution.ID, runIDFragment, batchIndex)

		specs := make([]JobSpec, len(drained))
		for i, env := range drained {
			specs[i] = env.Spec
		}

		// Execute the batched child workflow. The receiving worker
		// implements "BatchedJob" with a []JobSpec input; for v1 we
		// don't enforce a return shape — any per-slot reporting is
		// deferred to the child's own ledger writes. If the child
		// fails as a whole we still report back to every origin so
		// the dispatchers don't hang waiting for a reply that never
		// comes.
		childOpts := workflow.ChildWorkflowOptions{
			WorkflowID:               batchID,
			TaskQueue:                in.Queue,
			WorkflowExecutionTimeout: 24 * time.Hour,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		}
		childCtx := workflow.WithChildOptions(ctx, childOpts)
		childFuture := workflow.ExecuteChildWorkflow(childCtx, "BatchedJob", specs)

		var childExec workflow.Execution
		childRunID := ""
		if err := childFuture.GetChildWorkflowExecution().Get(ctx, &childExec); err == nil {
			childRunID = childExec.RunID
		}

		var childResult interface{}
		childErr := childFuture.Get(ctx, &childResult)
		errMsg := ""
		if childErr != nil {
			errMsg = childErr.Error()
			logger.Warn("BatchCoordinator child failed",
				"batch_id", batchID,
				"error", childErr,
			)
		}

		// Signal each envelope back with its slot result. We use a
		// best-effort fan-out — a single failed reply shouldn't keep
		// the other dispatches waiting forever, but we DO Get() each
		// future so a bad workflow id surfaces in this run's history.
		for i, env := range drained {
			slot := BatchSlotResult{
				BatchID:       batchID,
				ChildWorkflow: batchID,
				ChildRunID:    childRunID,
				SlotIndex:     i,
				Error:         errMsg,
			}
			sigName := SignalBatchResult + "-" + env.JobID
			f := workflow.SignalExternalWorkflow(ctx, env.OriginWorkflowID, "", sigName, slot)
			if err := f.Get(ctx, nil); err != nil {
				logger.Warn("BatchCoordinator reply signal failed",
					"origin", env.OriginWorkflowID,
					"job_id", env.JobID,
					"error", err,
				)
			}
		}

		progress.BatchesFired++
		logger.Info("BatchCoordinator batch fired",
			"batch_id", batchID,
			"slots", len(drained),
			"batches_fired", progress.BatchesFired,
		)

		// Continue-as-new keeps history bounded. We do it AFTER
		// firing a batch (rather than at the top of the loop) so the
		// buffer is guaranteed empty — anything carried over would
		// be lost across the boundary.
		if progress.BatchesFired >= continueAsNewEvery {
			logger.Info("BatchCoordinator continuing as new",
				"queue", in.Queue,
				"batches_fired", progress.BatchesFired,
			)
			return workflow.NewContinueAsNewError(ctx, BatchCoordinator, in)
		}
	}
}

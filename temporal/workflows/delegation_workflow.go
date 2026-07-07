package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamaton-platform/temporal/activities"
	"github.com/sirus20x6/adamaton-platform/temporal/errclass"
)

// DelegationWorkflow runs a single CLI delegation through the
// orchestrator activity. Used by both:
//
//   - one-shot ExecuteWorkflow calls (rare; the MCP delegate_task tool
//     keeps the in-process fast path), and
//   - Temporal Schedules created via schedule_recurring_task — each cron
//     fire spawns a fresh DelegationWorkflow execution.
//
// Single-activity by design — the orchestrator already handles routing,
// subprocess lifecycle, and budget reporting. Wrapping it in a workflow
// only adds Temporal's durability and visibility on top.
func DelegationWorkflow(ctx workflow.Context, in activities.DelegationInput) (*activities.DelegationOutput, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("DelegationWorkflow start",
		"agent_hint", in.AgentHint,
		"difficulty", in.Difficulty,
		"priority", in.Priority,
	)

	ctx = workflow.WithActivityOptions(ctx, delegationActivityOptions(in.TimeoutSecs))

	var out *activities.DelegationOutput
	var a *activities.DelegationActivities // method receiver used only for type inference
	err := workflow.ExecuteActivity(ctx, a.InvokeDelegation, in).Get(ctx, &out)
	if err != nil {
		logger.Warn("DelegationWorkflow activity failed",
			"error", err,
			"error_class", errclass.RecordWorkflowFailure(ctx, "DelegationWorkflow", err),
		)
		return out, err
	}
	logger.Info("DelegationWorkflow done", "task_id", out.TaskID, "status", out.Status)
	return out, nil
}

// delegationActivityOptions tunes the activity timeout to comfortably
// exceed the user's requested CLI timeout (default 5 minutes; capped at
// 30 minutes for sanity). HeartbeatTimeout is 30s — the activity's
// heartbeat pump fires every 10s, well inside that.
//
// Retry policy: 2 attempts. The activity classifies its own permanent
// errors (unknown agent, budget exhausted, exit-code-nonzero) as
// non-retryable via temporal.NewNonRetryableApplicationError, so the
// retry budget is reserved for transient subprocess timeouts. We don't
// want infinite retry on a stuck CLI.
func delegationActivityOptions(timeoutSecs int) workflow.ActivityOptions {
	if timeoutSecs <= 0 {
		timeoutSecs = 300
	}
	if timeoutSecs > 1800 {
		timeoutSecs = 1800
	}
	startToClose := time.Duration(timeoutSecs+30) * time.Second
	return workflow.ActivityOptions{
		StartToCloseTimeout:    startToClose,
		ScheduleToCloseTimeout: startToClose + 5*time.Minute,
		HeartbeatTimeout:       30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    2,
		},
	}
}

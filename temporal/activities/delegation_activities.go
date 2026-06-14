package activities

import (
	"context"
	"fmt"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// DelegationOrchestrator is the narrow interface this activity needs.
// Defined here so the activities package doesn't import internal/delegator
// — that direction would create a cycle (executor → activities →
// delegator → executor). The real *delegator.Orchestrator satisfies this
// implicitly; tests can supply a fake.
type DelegationOrchestrator interface {
	DelegateSync(ctx context.Context, req DelegateRequestLike) (DelegationTaskLike, error)
}

// DelegateRequestLike mirrors delegator.DelegateRequest's fields. Defined
// here so callers don't import the delegator package via the activities
// package. The orchestrator's DelegateSync accepts a typed concrete value;
// the concrete delegator.DelegateRequest's field tags happen to match —
// see the adapter in internal/delegator that wraps the orchestrator into
// this interface.
type DelegateRequestLike struct {
	// TaskID, when non-empty, pins the task's id instead of letting the
	// orchestrator generate one. The durable delegate_task path sets it so
	// the caller's returned task_id equals the Temporal WorkflowID and the
	// store row. Empty for recurring schedules (fresh id per fire).
	TaskID      string
	Prompt      string
	Difficulty  string
	Priority    string
	AgentHint   string
	WorkingDir  string
	Model       string
	TimeoutSecs int
}

// DelegationTaskLike is the subset of delegator.Task the activity reads
// after DelegateSync returns.
type DelegationTaskLike struct {
	ID       string
	Agent    string
	Provider string
	Status   string
	ExitCode int
	Output   string
	Error    string
}

// DelegationActivities groups the activity methods that drive recurring
// delegations. The orchestrator handle is captured so workflows can call
// into the same in-process delegate path the MCP delegate_task tool uses;
// no separate routing/spawn code path means recurring tasks pick up budget
// updates and quota guardrails for free.
type DelegationActivities struct {
	Orchestrator DelegationOrchestrator
}

// DelegationInput is the workflow→activity payload. Mirrors the user-
// supplied MCP delegate_task fields so a recurring schedule "feels" like
// a Claude-Code-issued delegation when it fires.
type DelegationInput struct {
	// TaskID pins the delegation's task id when set (durable delegate_task
	// passes the id it also uses as the Temporal WorkflowID, so the row the
	// activity writes is the same id the caller polls). Empty for recurring
	// schedules — each fire gets a fresh orchestrator-generated id.
	TaskID      string `json:"task_id,omitempty"`
	Prompt      string `json:"prompt"`
	Difficulty  string `json:"difficulty,omitempty"`
	Priority    string `json:"priority,omitempty"`
	AgentHint   string `json:"agent_hint,omitempty"`
	WorkingDir  string `json:"working_dir,omitempty"`
	Model       string `json:"model,omitempty"`
	TimeoutSecs int    `json:"timeout_seconds,omitempty"`
}

// DelegationOutput is what InvokeDelegation returns when the underlying
// CLI finishes. Keeps the data minimal — full output is in the sqlite
// task store for the UI to read.
type DelegationOutput struct {
	TaskID    string `json:"task_id"`
	Agent     string `json:"agent"`
	Provider  string `json:"provider"`
	Status    string `json:"status"`
	ExitCode  int    `json:"exit_code"`
	Truncated string `json:"output_preview,omitempty"`
}

// InvokeDelegation runs the CLI delegation synchronously and returns when
// it reaches a terminal state. Errors that look like budget exhaustion
// or unknown-agent are non-retryable; transient subprocess failures fall
// through to Temporal's retry policy. The activity records heartbeats so
// long-running delegations don't get killed mid-flight.
func (a *DelegationActivities) InvokeDelegation(ctx context.Context, in DelegationInput) (*DelegationOutput, error) {
	if a.Orchestrator == nil {
		return nil, temporal.NewNonRetryableApplicationError(
			"delegation orchestrator not configured", "ConfigError", nil,
		)
	}

	req := DelegateRequestLike{
		TaskID:      in.TaskID,
		Prompt:      in.Prompt,
		Difficulty:  in.Difficulty,
		Priority:    in.Priority,
		AgentHint:   in.AgentHint,
		WorkingDir:  in.WorkingDir,
		Model:       in.Model,
		TimeoutSecs: in.TimeoutSecs,
	}

	// Heartbeat every 10s so workflows.HeartbeatTimeout=30s never trips
	// while a slow CLI is still producing output.
	heartbeatStop := startHeartbeat(ctx, 10)
	defer heartbeatStop()

	task, err := a.Orchestrator.DelegateSync(ctx, req)
	if err != nil {
		// Classify common up-front failures so Temporal doesn't burn
		// retries on them. Routing errors and "unknown agent" come from
		// chooseAgent before the subprocess spawns and won't get better.
		if isPermanentDelegationErr(err) {
			return nil, temporal.NewNonRetryableApplicationError(
				err.Error(), "DelegationConfigError", err,
			)
		}
		return nil, fmt.Errorf("delegate sync: %w", err)
	}

	out := &DelegationOutput{
		TaskID:   task.ID,
		Agent:    task.Agent,
		Provider: task.Provider,
		Status:   task.Status,
		ExitCode: task.ExitCode,
	}
	if len(task.Output) > 500 {
		out.Truncated = task.Output[:500] + "…"
	} else {
		out.Truncated = task.Output
	}

	// Failed/timed-out tasks become Temporal failures so the schedule's
	// "last run failed" surface is meaningful. Subprocess timeouts are
	// retryable (could be transient); failed exits are not (the prompt
	// likely needs to change).
	switch task.Status {
	case "timed_out":
		return out, fmt.Errorf("delegation timed out: task=%s", task.ID)
	case "failed":
		return out, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("delegation exited %d: %s", task.ExitCode, task.Error),
			"DelegationFailed", nil,
		)
	case "cancelled":
		return out, temporal.NewCanceledError("delegation cancelled")
	}
	return out, nil
}

// isPermanentDelegationErr returns true for orchestrator errors that
// won't get better with retry: unknown agent hint, budget exhausted, no
// matching provider. Heuristic-only — we look at the message because the
// orchestrator currently uses fmt.Errorf without typed sentinels.
func isPermanentDelegationErr(err error) bool {
	msg := err.Error()
	for _, sub := range []string{
		"unknown agent",
		"unmapped provider",
		"no agent mapping",
		"no CLI spec",
		"no providers available",
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// startHeartbeat fires activity.RecordHeartbeat every intervalSecs seconds
// until the returned stop func is called. Mirrors the heartbeat-pump
// pattern in pr_review_activities.go.
func startHeartbeat(ctx context.Context, intervalSecs int) func() {
	if intervalSecs <= 0 {
		intervalSecs = 10
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := newSecondsTicker(intervalSecs)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C():
				activity.RecordHeartbeat(ctx, nil)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

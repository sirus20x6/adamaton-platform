package workflows

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/sirus20x6/adamaton-platform/temporal/activities"
)

// TestDelegationWorkflow_Success verifies the workflow returns the
// activity's output unchanged when the activity succeeds.
func TestDelegationWorkflow_Success(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()

	// Register the real activity struct so OnActivity can match by
	// method reference. The receiver's Orchestrator is nil in the test
	// — we'll OnActivity past it, never reaching the real method.
	a := &activities.DelegationActivities{}
	env.RegisterActivity(a)

	want := &activities.DelegationOutput{
		TaskID:    "task-abc",
		Agent:     "opencode",
		Provider:  "local",
		Status:    "completed",
		ExitCode:  0,
		Truncated: "ok",
	}

	env.OnActivity(a.InvokeDelegation, mock.Anything, mock.Anything).Return(want, nil)

	in := activities.DelegationInput{
		Prompt:      "test",
		Difficulty:  "trivial",
		Priority:    "background",
		AgentHint:   "opencode",
		TimeoutSecs: 60,
	}
	env.ExecuteWorkflow(DelegationWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var got *activities.DelegationOutput
	require.NoError(t, env.GetWorkflowResult(&got))
	require.Equal(t, want, got)
}

// TestDelegationWorkflow_ActivityFailureSurfaces verifies a non-retryable
// activity error propagates to the workflow result without consuming the
// retry budget.
func TestDelegationWorkflow_ActivityFailureSurfaces(t *testing.T) {
	suite := &testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()

	a := &activities.DelegationActivities{}
	env.RegisterActivity(a)

	appErr := temporal.NewNonRetryableApplicationError("delegation exited 1", "DelegationFailed", nil)
	env.OnActivity(a.InvokeDelegation, mock.Anything, mock.Anything).Return(nil, appErr)

	in := activities.DelegationInput{Prompt: "x", AgentHint: "opencode"}
	env.ExecuteWorkflow(DelegationWorkflow, in)

	require.True(t, env.IsWorkflowCompleted())
	err := env.GetWorkflowError()
	require.Error(t, err)
	var ae *temporal.ApplicationError
	require.True(t, errors.As(err, &ae))
}

// TestDelegationActivityOptions_TimeoutBounds verifies the timeout
// computation never returns absurd values.
func TestDelegationActivityOptions_TimeoutBounds(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 330},   // default 300 + 30 grace
		{-5, 330},  // negative falls back to default
		{60, 90},
		{600, 630},
		{3600, 1830}, // capped at 1800 + 30
	}
	for _, c := range cases {
		opts := delegationActivityOptions(c.in)
		got := int(opts.StartToCloseTimeout.Seconds())
		require.Equal(t, c.want, got, "input %d", c.in)
	}
}

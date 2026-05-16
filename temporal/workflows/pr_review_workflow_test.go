package workflows

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamomaton-platform/temporal/activities"
	"github.com/sirus20x6/adamomaton-platform/temporal/gitea"
)

func TestMakeEnhancedDecision(t *testing.T) {
	tests := []struct {
		name           string
		results        []activities.CheckResult
		expectedAction string
		expectedPass   int
		expectedFail   int
	}{
		{
			name: "All agents pass - low risk",
			results: []activities.CheckResult{
				{Agent: "Performance", Verdict: "PASS", Rationale: "No issues"},
				{Agent: "Const", Verdict: "PASS", Rationale: "No issues"},
				{Agent: "Style", Verdict: "PASS", Rationale: "No issues"},
			},
			expectedAction: "MERGE",
			expectedPass:   3,
			expectedFail:   0,
		},
		{
			name: "Security failure triggers critical risk",
			results: []activities.CheckResult{
				{Agent: "Security", Verdict: "FAIL", Rationale: "SQL injection found"},
				{Agent: "Performance", Verdict: "PASS", Rationale: "No issues"},
				{Agent: "Const", Verdict: "PASS", Rationale: "No issues"},
			},
			expectedAction: "REQUEST_REVIEW",
			expectedPass:   2,
			expectedFail:   1,
		},
		{
			name: "All agents fail",
			results: []activities.CheckResult{
				{Agent: "Security", Verdict: "FAIL", Rationale: "Vulnerability found"},
				{Agent: "Performance", Verdict: "FAIL", Rationale: "Slow code"},
				{Agent: "Const", Verdict: "FAIL", Rationale: "Mutable state"},
			},
			expectedAction: "REQUEST_REVIEW",
			expectedPass:   0,
			expectedFail:   3,
		},
		{
			name: "Two pass one fail non-security",
			results: []activities.CheckResult{
				{Agent: "Performance", Verdict: "FAIL", Rationale: "Slow"},
				{Agent: "Const", Verdict: "PASS", Rationale: "OK"},
				{Agent: "Style", Verdict: "PASS", Rationale: "OK"},
			},
			expectedAction: "MERGE",
			expectedPass:   2,
			expectedFail:   1,
		},
		{
			name: "Warning counts as soft pass for merge eligibility",
			results: []activities.CheckResult{
				{Agent: "Security", Verdict: "WARNING", Rationale: "Minor concern"},
				{Agent: "Performance", Verdict: "PASS", Rationale: "OK"},
				{Agent: "Const", Verdict: "PASS", Rationale: "OK"},
			},
			expectedAction: "MERGE",
			expectedPass:   2,
			expectedFail:   0,
		},
		{
			name: "Results with metrics - critical files with failure triggers high risk",
			results: []activities.CheckResult{
				{Agent: "Performance", Verdict: "FAIL", Rationale: "Slow", Metrics: &activities.AnalysisMetrics{
					CriticalFiles: []string{"main.go"},
				}},
				{Agent: "Const", Verdict: "PASS", Rationale: "OK"},
				{Agent: "Style", Verdict: "PASS", Rationale: "OK"},
			},
			expectedAction: "REQUEST_REVIEW",
			expectedPass:   2,
			expectedFail:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := makeEnhancedDecision(tt.results)

			if decision.Action != tt.expectedAction {
				t.Errorf("Expected action %s, got %s", tt.expectedAction, decision.Action)
			}
			if decision.PassCount != tt.expectedPass {
				t.Errorf("Expected pass count %d, got %d", tt.expectedPass, decision.PassCount)
			}
			if decision.FailCount != tt.expectedFail {
				t.Errorf("Expected fail count %d, got %d", tt.expectedFail, decision.FailCount)
			}
		})
	}
}

func TestCalculateRiskLevel(t *testing.T) {
	tests := []struct {
		name             string
		passCount        int
		failCount        int
		criticalFailures []string
		metrics          *activities.AnalysisMetrics
		expected         string
	}{
		{
			name:             "Critical failures",
			passCount:        2,
			failCount:        1,
			criticalFailures: []string{"Security vulnerability detected"},
			metrics:          nil,
			expected:         "CRITICAL",
		},
		{
			name:             "Critical files modified with failures",
			passCount:        2,
			failCount:        1,
			criticalFailures: nil,
			metrics:          &activities.AnalysisMetrics{CriticalFiles: []string{"main.go"}},
			expected:         "HIGH",
		},
		{
			name:             "High complexity with failures",
			passCount:        2,
			failCount:        1,
			criticalFailures: nil,
			metrics:          &activities.AnalysisMetrics{ComplexityScore: 4},
			expected:         "HIGH",
		},
		{
			name:             "High complexity but no failures is LOW",
			passCount:        3,
			failCount:        0,
			criticalFailures: nil,
			metrics:          &activities.AnalysisMetrics{ComplexityScore: 3},
			expected:         "LOW",
		},
		{
			name:             "Multiple failures without metrics",
			passCount:        1,
			failCount:        2,
			criticalFailures: nil,
			metrics:          nil,
			expected:         "MEDIUM",
		},
		{
			name:             "Low risk simple change",
			passCount:        3,
			failCount:        0,
			criticalFailures: nil,
			metrics:          nil,
			expected:         "LOW",
		},
		{
			name:             "Single failure no metrics",
			passCount:        2,
			failCount:        1,
			criticalFailures: nil,
			metrics:          nil,
			expected:         "LOW",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateRiskLevel(tt.passCount, tt.failCount, tt.criticalFailures, tt.metrics)
			if result != tt.expected {
				t.Errorf("Expected risk level %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestDetermineAction(t *testing.T) {
	tests := []struct {
		name      string
		passCount int
		failCount int
		riskLevel string
		metrics   *activities.AnalysisMetrics
		expected  string
	}{
		{
			name:      "Critical risk always requests review",
			passCount: 3,
			failCount: 0,
			riskLevel: "CRITICAL",
			metrics:   nil,
			expected:  "REQUEST_REVIEW",
		},
		{
			name:      "High risk always requests review",
			passCount: 3,
			failCount: 0,
			riskLevel: "HIGH",
			metrics:   nil,
			expected:  "REQUEST_REVIEW",
		},
		{
			name:      "Medium risk with all passing merges",
			passCount: 3,
			failCount: 0,
			riskLevel: "MEDIUM",
			metrics:   nil,
			expected:  "MERGE",
		},
		{
			name:      "Medium risk with failures requests review",
			passCount: 2,
			failCount: 1,
			riskLevel: "MEDIUM",
			metrics:   nil,
			expected:  "REQUEST_REVIEW",
		},
		{
			name:      "Low risk with 2/3 majority merges",
			passCount: 2,
			failCount: 1,
			riskLevel: "LOW",
			metrics:   nil,
			expected:  "MERGE",
		},
		{
			name:      "Low risk with minority pass requests review",
			passCount: 1,
			failCount: 2,
			riskLevel: "LOW",
			metrics:   nil,
			expected:  "REQUEST_REVIEW",
		},
		{
			name:      "Low risk all pass with production changes but no tests merges",
			passCount: 3,
			failCount: 0,
			riskLevel: "LOW",
			metrics: &activities.AnalysisMetrics{
				AffectedFiles: []string{"main.go"},
				TestFiles:     []string{},
			},
			expected: "MERGE",
		},
		{
			name:      "Low risk with failures and production changes but no tests requests review",
			passCount: 2,
			failCount: 1,
			riskLevel: "LOW",
			metrics: &activities.AnalysisMetrics{
				AffectedFiles: []string{"main.go"},
				TestFiles:     []string{},
			},
			expected: "REQUEST_REVIEW",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := determineAction(tt.passCount, tt.failCount, tt.riskLevel, tt.metrics)
			if result != tt.expected {
				t.Errorf("Expected action %s, got %s", tt.expected, result)
			}
		})
	}
}

// TestAgentEnabledHelper covers the gate function that the workflow uses to
// honor the dashboard's per-agent toggles. The crucial property is that an
// unconfigured AgentEnabledFlags value (the zero value, which older callers
// will pass) keeps every agent enabled — otherwise updating the field type
// would silently turn into a stop-the-world deployment.
//
// strict=true mirrors the current path (the agent-enabled-strict version
// has been bumped to 1); strict=false documents the legacy behavior that
// in-flight workflows will replay when GetVersion returns DefaultVersion.
func TestAgentEnabledHelper(t *testing.T) {
	// allTwelve enables every known types.AgentType so we can assert that
	// the helper handles the full set, not just the original three.
	allTwelve := map[string]bool{
		"Security":        true,
		"Performance":     true,
		"Const":           true,
		"Documentation":   true,
		"Testing":         true,
		"Architecture":    true,
		"Accessibility":   true,
		"Compliance":      true,
		"Dependencies":    true,
		"Style":           true,
		"Maintainability": true,
		"BusinessLogic":   true,
	}

	cases := []struct {
		name   string
		flags  AgentEnabledFlags
		agent  string
		strict bool
		want   bool
	}{
		// Configured=false -> all agents on, regardless of strict.
		{"unconfigured -> enabled (Security)", AgentEnabledFlags{}, "Security", true, true},
		{"unconfigured -> enabled (Performance)", AgentEnabledFlags{}, "Performance", true, true},
		{"unconfigured -> enabled (Const)", AgentEnabledFlags{}, "Const", true, true},
		{"unconfigured -> enabled (Style)", AgentEnabledFlags{}, "Style", true, true},
		{"unconfigured -> enabled (BusinessLogic)", AgentEnabledFlags{}, "BusinessLogic", true, true},
		{"unconfigured -> enabled (unknown)", AgentEnabledFlags{}, "FuturAgent", true, true},
		{"unconfigured (legacy) -> enabled (unknown)", AgentEnabledFlags{}, "FuturAgent", false, true},

		// Configured=true with the full 12-agent allowlist set to true ->
		// every named agent runs in strict mode. This is the bug-fix case:
		// pre-fix the helper hard-coded a three-case switch, so 9 of these
		// would have returned false despite Enabled[name]=true.
		{"strict all 12 enabled (Security)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Security", true, true},
		{"strict all 12 enabled (Performance)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Performance", true, true},
		{"strict all 12 enabled (Const)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Const", true, true},
		{"strict all 12 enabled (Documentation)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Documentation", true, true},
		{"strict all 12 enabled (Testing)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Testing", true, true},
		{"strict all 12 enabled (Architecture)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Architecture", true, true},
		{"strict all 12 enabled (Accessibility)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Accessibility", true, true},
		{"strict all 12 enabled (Compliance)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Compliance", true, true},
		{"strict all 12 enabled (Dependencies)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Dependencies", true, true},
		{"strict all 12 enabled (Style)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Style", true, true},
		{"strict all 12 enabled (Maintainability)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "Maintainability", true, true},
		{"strict all 12 enabled (BusinessLogic)", AgentEnabledFlags{Configured: true, Enabled: allTwelve}, "BusinessLogic", true, true},

		// Configured=true with selective opt-in. Names absent from the map
		// must read as disabled in strict mode.
		{"strict only Security enabled (Security on)", AgentEnabledFlags{Configured: true, Enabled: map[string]bool{"Security": true}}, "Security", true, true},
		{"strict only Security enabled (Performance off by default)", AgentEnabledFlags{Configured: true, Enabled: map[string]bool{"Security": true}}, "Performance", true, false},
		{"strict explicit false beats default", AgentEnabledFlags{Configured: true, Enabled: map[string]bool{"Security": false, "Performance": true}}, "Security", true, false},

		// Defense-in-depth (strict mode): when the operator has explicitly
		// configured the flag set, an unknown agent name is treated as
		// DISABLED. A typo'd name from an out-of-date dashboard, or a new
		// agent the gate helper has not been updated to recognize, must
		// not silently fall through to "run it" and bypass the configured
		// allowlist.
		{"configured unknown agent rejected (strict)", AgentEnabledFlags{Configured: true}, "FuturAgent", true, false},
		{"configured nil-Enabled (strict) -> all off", AgentEnabledFlags{Configured: true}, "Security", true, false},

		// Legacy mode (strict=false) — what GetVersion returns DefaultVersion
		// should look like for in-flight workflow replay. The unknown-agent
		// flip introduced by pr-review/agent-enabled-strict must NOT take
		// effect on pre-change history. This locks down the OLD path.
		{"configured unknown agent allowed (legacy)", AgentEnabledFlags{Configured: true}, "FuturAgent", false, true},
		{"configured legacy missing-from-map -> true", AgentEnabledFlags{Configured: true, Enabled: map[string]bool{"Security": false}}, "Performance", false, true},
		{"configured legacy explicit false honored", AgentEnabledFlags{Configured: true, Enabled: map[string]bool{"Security": false}}, "Security", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := agentEnabled(c.flags, c.agent, c.strict)
			if got != c.want {
				t.Errorf("agentEnabled(%+v, %q, strict=%v) = %v, want %v", c.flags, c.agent, c.strict, got, c.want)
			}
		})
	}
}

// TestMakeEnhancedDecisionMetricsCopy verifies that combinedMetrics is a
// COPY of the first non-nil result.Metrics, not an alias. Mutating the
// returned decision's metrics must not change the input slice's data.
//
// This locks down the fix for the audit finding where combinedMetrics
// took a pointer to whichever check's Metrics was non-nil first; the
// alias is currently inert (all checks compute the same metrics) but
// would surface as a real bug the moment metrics diverged across checks.
func TestMakeEnhancedDecisionMetricsCopy(t *testing.T) {
	originalFiles := []string{"main.go"}
	results := []activities.CheckResult{
		{
			Agent:   "Security",
			Verdict: "PASS",
			Metrics: &activities.AnalysisMetrics{
				ComplexityScore: 1,
				AffectedFiles:   originalFiles,
			},
		},
		{Agent: "Performance", Verdict: "PASS"},
		{Agent: "Const", Verdict: "PASS"},
	}
	decision := makeEnhancedDecision(results)
	require.NotNil(t, decision.Metrics, "expected combined metrics to be populated")

	// Mutate the decision's metrics. The input slice's metrics should be
	// untouched because we copied the struct value.
	decision.Metrics.ComplexityScore = 99

	require.Equal(t, 1, results[0].Metrics.ComplexityScore,
		"input metrics were aliased; mutation leaked through")
}

// ---------------------------------------------------------------------------
// Workflow-level tests for state-mutating retry override and agent gating.
// ---------------------------------------------------------------------------

type prSuite struct {
	testsuite.WorkflowTestSuite
}

// TestPRReviewWorkflow_MergeActivityNotRetried verifies that MergeActivity
// is called AT MOST ONCE even when it returns a transient error. The
// audit finding is that the default RetryPolicy{MaximumAttempts: 3} would
// otherwise cause a double-merge if the upstream service received the
// request before the network blip.
//
// We make all three check activities return PASS so the workflow chooses
// the MERGE path, then make MergeActivity return an error and assert that
// the call counter ends at exactly 1.
func TestPRReviewWorkflow_MergeActivityNotRetried(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS", Rationale: "ok"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	var mergeCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			atomic.AddInt32(&mergeCalls, 1)
			// Transient-looking error. With the default 3-attempt policy
			// the workflow would call this 3 times; with the
			// mutatingActivityOptions override it must be called once.
			return errors.New("upstream connection reset")
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  1,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(),
		"workflow should propagate the merge error rather than swallow it")
	got := atomic.LoadInt32(&mergeCalls)
	require.Equal(t, int32(1), got,
		"MergeActivity was called %d times; expected 1 (retry policy override broken)", got)
}

// TestPRReviewWorkflow_AgentEnabledFlagsHonored verifies that disabling an
// agent via AgentEnabledFlags actually skips the check activity instead of
// running it and ignoring the result. We register Security to fail; if
// Security is disabled, the workflow should reach the merge path with two
// passing checks (PerformanceCheckActivity + ConstCheckActivity) and never
// invoke SecurityCheckActivity.
func TestPRReviewWorkflow_AgentEnabledFlagsHonored(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)

	var securityCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			atomic.AddInt32(&securityCalls, 1)
			return activities.CheckResult{Agent: "Security", Verdict: "FAIL", Rationale: "would block merge"}, nil
		},
		activity.RegisterOptions{Name: "SecurityCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Performance", Verdict: "PASS"}, nil
		},
		activity.RegisterOptions{Name: "PerformanceCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Const", Verdict: "PASS"}, nil
		},
		activity.RegisterOptions{Name: "ConstCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error { return nil },
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  2,
		RepoOwner: "octo",
		RepoName:  "demo",
		Agents: AgentEnabledFlags{
			Configured: true,
			Enabled: map[string]bool{
				"Security":    false, // disabled
				"Performance": true,
				"Const":       true,
			},
		},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"workflow with security disabled and 2/3 passing should merge cleanly")
	require.Equal(t, int32(0), atomic.LoadInt32(&securityCalls),
		"SecurityCheckActivity ran despite Agents.Security=false")
}

// TestPRReviewWorkflow_AllAgentsDisabled verifies the corner case where
// the dashboard has flipped every agent off: no check runs, and the
// workflow exits cleanly as a no-op. The previous behavior was to fall
// through to makeEnhancedDecision with an empty results slice, which
// synthesized a REQUEST_REVIEW with no failingReasons and posted a
// "automated agents flagged issues" comment that was actively
// misleading (no agent had run, much less flagged anything).
//
// New contract: when len(results) == 0 the workflow logs a warning and
// returns nil — no merge, no comment, run recorded as completed-no-op.
func TestPRReviewWorkflow_AllAgentsDisabled(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	failCheck := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			t.Errorf("%s ran despite all agents disabled", agent)
			return activities.CheckResult{}, nil
		}
	}
	env.RegisterActivityWithOptions(failCheck("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			t.Error("CommentForHumanReviewActivity ran despite all agents disabled — should be a no-op")
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			t.Error("MergeActivity ran despite all agents disabled — should be a no-op")
			return nil
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  3,
		RepoOwner: "octo",
		RepoName:  "demo",
		Agents: AgentEnabledFlags{
			Configured: true,
			Enabled: map[string]bool{
				"Security":    false,
				"Performance": false,
				"Const":       false,
			},
		},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"all-agents-disabled should complete cleanly without comment or merge")
}

// TestPRReviewWorkflow_EmptyDiff_RequestsReview — Pass 11 / E2E trace #13.
// When FetchDiff returns "" or whitespace-only the workflow used to feed
// every check zero text and silently auto-merge. The fix short-circuits to
// REQUEST_REVIEW; this test pins the new behavior so a future refactor
// cannot regress it.
func TestPRReviewWorkflow_EmptyDiff_RequestsReview(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			// Whitespace-only counts as empty after TrimSpace — the
			// short-circuit must catch this case too, not just literal "".
			return "   \n  \t  ", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	failCheck := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			t.Errorf("%sCheckActivity ran despite empty diff short-circuit", agent)
			return activities.CheckResult{}, nil
		}
	}
	env.RegisterActivityWithOptions(failCheck("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			t.Error("MergeActivity ran for empty diff; should be REQUEST_REVIEW")
			return nil
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)
	var commentCalls int32
	var capturedReasons []string
	env.RegisterActivityWithOptions(
		func(_ context.Context, args activities.CommentArgs) error {
			atomic.AddInt32(&commentCalls, 1)
			capturedReasons = args.FailingReasons
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  4,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, int32(1), atomic.LoadInt32(&commentCalls),
		"CommentForHumanReviewActivity should run exactly once for the empty-diff path")
	require.Len(t, capturedReasons, 1)
	require.Contains(t, capturedReasons[0], "diff was empty",
		"comment should mention the empty-diff cause so the human knows what to do")
}

// TestPRReviewWorkflow_AlreadyMerged_TreatedAsSuccess — Pass 11. When the
// merge activity returns gitea.ErrAlreadyMerged (or wraps it), the
// workflow must NOT propagate the error to Temporal as a failure. The
// PR is already in the desired state; failing would just create
// noise + a duplicate retry storm. Today MergeActivity goes through
// MCP, but the workflow's errors.Is dispatch is the same, so this
// sentinel applies the moment any future direct-Gitea fallback surfaces it.
func TestPRReviewWorkflow_AlreadyMerged_TreatedAsSuccess(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			// Cross-activity-boundary equivalent of returning the sentinel:
			// Stream B's contract wraps this with ApplicationError + the
			// "already_merged" reason string so the workflow's
			// errors.As(*temporal.ApplicationError) check picks it up after
			// Temporal collapses sentinel identity in the data converter.
			return temporal.NewApplicationErrorWithCause(
				"PR is already merged", "already_merged", gitea.ErrAlreadyMerged)
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  15,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"ErrAlreadyMerged must be swallowed in PRReviewWorkflow too")
}

// TestPRReviewWorkflow_MissingHeadSHA_LogsAndProceeds — when the caller
// does not populate args.HeadSHA, the workflow logs a warning but does
// NOT fail. We can't easily intercept the workflow logger here, so the
// test asserts the observable outcome: the merge still happens with
// MergeArgs.HeadSHA="".
func TestPRReviewWorkflow_MissingHeadSHA_LogsAndProceeds(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	var capturedSHA string
	var mergeCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, args activities.MergeArgs) error {
			atomic.AddInt32(&mergeCalls, 1)
			capturedSHA = args.HeadSHA
			return nil
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  16,
		RepoOwner: "octo",
		RepoName:  "demo",
		// HeadSHA intentionally left empty
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, int32(1), atomic.LoadInt32(&mergeCalls))
	require.Equal(t, "", capturedSHA,
		"missing args.HeadSHA must propagate as empty MergeArgs.HeadSHA — the activity decides the rest")
}

// TestMakeReviewDecisionForEmptyDiff is a focused unit test for the helper.
// It locks in the synthetic decision shape so a refactor can't quietly
// change the merge action or risk level.
func TestMakeReviewDecisionForEmptyDiff(t *testing.T) {
	dec := makeReviewDecisionForEmptyDiff(PRReviewArgs{PRNumber: 7, RepoOwner: "octo", RepoName: "demo"})
	require.Equal(t, "REQUEST_REVIEW", dec.Action,
		"empty-diff decision must be REQUEST_REVIEW — auto-merging zero text is the bug we're fixing")
	require.Equal(t, "MEDIUM", dec.RiskLevel,
		"empty-diff risk must be MEDIUM (not LOW) so determineAction would still avoid auto-merge")
	require.Equal(t, 0, dec.PassCount)
	require.Equal(t, 0, dec.FailCount)
	require.Equal(t, 0, dec.WarningCount)
	require.Len(t, dec.FailingReasons, 1)
	require.Contains(t, dec.FailingReasons[0], "diff was empty")
	require.Nil(t, dec.Metrics, "no analysis ran, so there's no metric to attach")
}

// ---------------------------------------------------------------------------
// GiteaPRReviewWorkflow — Pass 11 brought this from 0% coverage. Each test
// mirrors the PRReviewWorkflow case it parallels, with Gitea-named
// activities registered instead.
// ---------------------------------------------------------------------------

// TestGiteaPRReviewWorkflow_HappyPath — three PASS results route to MERGE
// and GiteaMergeActivity is called exactly once. We also assert that
// args.HeadSHA flows through into MergeArgs.HeadSHA so the Gitea SHA pin
// path actually receives the commit hash.
func TestGiteaPRReviewWorkflow_HappyPath(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS", Rationale: "ok"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	var mergeCalls int32
	var capturedSHA string
	env.RegisterActivityWithOptions(
		func(_ context.Context, args activities.MergeArgs) error {
			atomic.AddInt32(&mergeCalls, 1)
			capturedSHA = args.HeadSHA
			return nil
		},
		activity.RegisterOptions{Name: "GiteaMergeActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  10,
		RepoOwner: "octo",
		RepoName:  "demo",
		HeadSHA:   "deadbeefcafe",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, int32(1), atomic.LoadInt32(&mergeCalls),
		"GiteaMergeActivity must be called exactly once on the happy path")
	require.Equal(t, "deadbeefcafe", capturedSHA,
		"workflow must propagate args.HeadSHA into MergeArgs.HeadSHA so the SHA pin actually reaches Gitea")
}

// TestGiteaPRReviewWorkflow_FetchDiffFailsPropagates — if the fetch
// activity returns an error, the workflow fails; merge/comment must not
// run. This pins the "fail-fast on upstream failure" contract.
func TestGiteaPRReviewWorkflow_FetchDiffFailsPropagates(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "", errors.New("upstream gitea timeout")
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	failCheck := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			t.Errorf("%sCheckActivity ran despite fetch failure", agent)
			return activities.CheckResult{}, nil
		}
	}
	env.RegisterActivityWithOptions(failCheck("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  11,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(),
		"workflow should propagate the fetch-diff error rather than swallow it")
}

// TestGiteaPRReviewWorkflow_AgentEnabledFlagsHonored — disabling Security
// via AgentEnabledFlags must skip the activity and exclude it from the
// pass/fail tally. With Performance + Const passing, the workflow takes
// the LOW-risk merge branch.
func TestGiteaPRReviewWorkflow_AgentEnabledFlagsHonored(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)

	var securityCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			atomic.AddInt32(&securityCalls, 1)
			// Returning FAIL would block the merge if the gate were
			// broken — we need the test to fail loudly if Security
			// runs by mistake.
			return activities.CheckResult{Agent: "Security", Verdict: "FAIL", Rationale: "would block"}, nil
		},
		activity.RegisterOptions{Name: "SecurityCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Performance", Verdict: "PASS"}, nil
		},
		activity.RegisterOptions{Name: "PerformanceCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Const", Verdict: "PASS"}, nil
		},
		activity.RegisterOptions{Name: "ConstCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error { return nil },
		activity.RegisterOptions{Name: "GiteaMergeActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  12,
		RepoOwner: "octo",
		RepoName:  "demo",
		Agents: AgentEnabledFlags{
			Configured: true,
			Enabled: map[string]bool{
				"Security":    false,
				"Performance": true,
				"Const":       true,
			},
		},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"workflow with security disabled and 2/2 passing should merge cleanly")
	require.Equal(t, int32(0), atomic.LoadInt32(&securityCalls),
		"SecurityCheckActivity ran despite Agents.Security=false")
}

// TestGiteaPRReviewWorkflow_EmptyDiff_RequestsReview — same as the
// PRReviewWorkflow variant, but for the Gitea path. An empty diff returned
// by GiteaFetchDiffActivity must short-circuit to REQUEST_REVIEW so the
// human re-triggers once Gitea has finished computing.
func TestGiteaPRReviewWorkflow_EmptyDiff_RequestsReview(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	failCheck := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			t.Errorf("%sCheckActivity ran despite empty-diff short-circuit", agent)
			return activities.CheckResult{}, nil
		}
	}
	env.RegisterActivityWithOptions(failCheck("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(failCheck("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			t.Error("GiteaMergeActivity ran for empty diff; should be REQUEST_REVIEW")
			return nil
		},
		activity.RegisterOptions{Name: "GiteaMergeActivity"},
	)
	var commentCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, args activities.CommentArgs) error {
			atomic.AddInt32(&commentCalls, 1)
			require.Len(t, args.FailingReasons, 1,
				"empty-diff comment should carry exactly one rationale")
			require.Contains(t, args.FailingReasons[0], "diff was empty")
			return nil
		},
		activity.RegisterOptions{Name: "GiteaCommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  13,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, int32(1), atomic.LoadInt32(&commentCalls),
		"GiteaCommentForHumanReviewActivity should run exactly once for the empty-diff path")
}

// TestGiteaPRReviewWorkflow_AlreadyMerged_TreatedAsSuccess — Pass 11.
// gitea.ErrAlreadyMerged is a benign-success signal: a maintainer merged
// the PR while the AI review was running. The workflow must NOT propagate
// the error — the desired end state (PR merged) is already in place.
//
// We wrap the sentinel via errors.Join to also exercise the errors.Is
// path: the workflow calls errors.Is(err, gitea.ErrAlreadyMerged), which
// must succeed even when the error chain has been augmented with extra
// context.
func TestGiteaPRReviewWorkflow_AlreadyMerged_TreatedAsSuccess(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			// Stream B's contract: ApplicationError with reason
			// "already_merged" wrapping the typed sentinel.
			return temporal.NewApplicationErrorWithCause(
				"upstream rejected merge: already merged", "already_merged",
				gitea.ErrAlreadyMerged)
		},
		activity.RegisterOptions{Name: "GiteaMergeActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  14,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"ErrAlreadyMerged must be swallowed: PR is already in the desired state")
}

// TestGiteaPRReviewWorkflow_HeadSHAMismatch_PostsCommentAndSucceeds —
// regression test for Bug 1: when Gitea rejects a merge with
// gitea.ErrHeadSHAMismatch (force-push during AI review), the workflow
// must NOT propagate the error. The verdict was computed against a
// different commit and no longer applies. Expected behavior:
//   - log a warning (not asserted here; we'd need a custom workflow logger)
//   - post a comment via the comment activity asking for a re-trigger
//   - return success, so the workflow run completes without retry storms
//
// The comment activity receives the dedicated re-trigger message; the
// failingReasons coming from a regular review verdict are NOT relevant
// because no verdict applies anymore.
func TestGiteaPRReviewWorkflow_HeadSHAMismatch_PostsCommentAndSucceeds(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	var mergeCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			atomic.AddInt32(&mergeCalls, 1)
			// Stream B's contract: ApplicationError with reason
			// "head_sha_mismatch". The workflow's isHeadSHAMismatchErr
			// helper uses errors.As(*temporal.ApplicationError) +
			// .Type() == "head_sha_mismatch", which is the only signal
			// guaranteed to survive the activity boundary.
			return temporal.NewApplicationErrorWithCause(
				"upstream rejected merge: head moved",
				"head_sha_mismatch", gitea.ErrHeadSHAMismatch)
		},
		activity.RegisterOptions{Name: "GiteaMergeActivity"},
	)

	var commentCalls int32
	var capturedReasons []string
	env.RegisterActivityWithOptions(
		func(_ context.Context, args activities.CommentArgs) error {
			atomic.AddInt32(&commentCalls, 1)
			capturedReasons = args.FailingReasons
			return nil
		},
		activity.RegisterOptions{Name: "GiteaCommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  20,
		RepoOwner: "octo",
		RepoName:  "demo",
		HeadSHA:   "deadbeefcafe",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"ErrHeadSHAMismatch must NOT propagate as an error — the verdict no longer applies")
	require.Equal(t, int32(1), atomic.LoadInt32(&mergeCalls),
		"merge should have been attempted exactly once")
	require.Equal(t, int32(1), atomic.LoadInt32(&commentCalls),
		"head-SHA-mismatch path must post a 're-trigger' comment")
	require.Len(t, capturedReasons, 1)
	require.Contains(t, capturedReasons[0], "head moved",
		"comment must explain the cause so the human knows to re-trigger")
}

// TestPRReviewWorkflow_HeadSHAMismatch_PostsCommentAndSucceeds — same
// regression as the Gitea variant but for the GitHub-MCP path. Both
// executeDecision and executeGiteaDecision must handle the sentinel; if
// either one drifts the test fails.
func TestPRReviewWorkflow_HeadSHAMismatch_PostsCommentAndSucceeds(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			// Stream B's wrapping contract.
			return temporal.NewApplicationErrorWithCause(
				"head SHA mismatch", "head_sha_mismatch",
				gitea.ErrHeadSHAMismatch)
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	var commentCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, args activities.CommentArgs) error {
			atomic.AddInt32(&commentCalls, 1)
			require.Len(t, args.FailingReasons, 1)
			require.Contains(t, args.FailingReasons[0], "head moved")
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber:  21,
		RepoOwner: "octo",
		RepoName:  "demo",
		HeadSHA:   "feedface",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"ErrHeadSHAMismatch must be treated as benign in PRReviewWorkflow too")
	require.Equal(t, int32(1), atomic.LoadInt32(&commentCalls),
		"head-SHA-mismatch path must post a 're-trigger' comment via CommentForHumanReviewActivity")
}

// TestPRReviewWorkflow_LegacyEmptyDiffPathTakenOnDefaultVersion — Bug 1
// regression. When workflow.GetVersion returns DefaultVersion (in-flight
// workflow replaying pre-change history) the empty-diff branch must NOT
// fire. Instead the workflow should fall through to running the agent
// checks against the empty diff string, exactly mirroring the legacy
// behavior captured in history.
//
// We mock GetVersion("pr-review/empty-diff-guard") to DefaultVersion and
// expect the check activities to run (and produce a normal MERGE on the
// 3xPASS path) — proving the empty-diff fast path was bypassed.
func TestPRReviewWorkflow_LegacyEmptyDiffPathTakenOnDefaultVersion(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.OnGetVersion("pr-review/empty-diff-guard", workflow.DefaultVersion, 1).
		Return(workflow.DefaultVersion)

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "", nil // empty diff — would normally short-circuit
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	var checkRuns int32
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			atomic.AddInt32(&checkRuns, 1)
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error { return nil },
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber: 100, RepoOwner: "octo", RepoName: "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, int32(3), atomic.LoadInt32(&checkRuns),
		"on DefaultVersion replay the empty-diff guard must NOT fire — all agents should run")
}

// TestPRReviewWorkflow_LegacyAllAgentsDisabledFallThrough — Bug 1
// regression for the all-agents-disabled-static version key. On
// DefaultVersion replay, an empty results slice must NOT short-circuit;
// the workflow falls through to the comment activity (legacy path).
func TestPRReviewWorkflow_LegacyAllAgentsDisabledFallThrough(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.OnGetVersion("pr-review/all-agents-disabled-static", workflow.DefaultVersion, 1).
		Return(workflow.DefaultVersion)

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	// All checks registered — none will run (Configured=true, all flags false).
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{}, nil
		},
		activity.RegisterOptions{Name: "SecurityCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{}, nil
		},
		activity.RegisterOptions{Name: "PerformanceCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{}, nil
		},
		activity.RegisterOptions{Name: "ConstCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error { return nil },
		activity.RegisterOptions{Name: "MergeActivity"},
	)
	var commentCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			atomic.AddInt32(&commentCalls, 1)
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber: 101, RepoOwner: "octo", RepoName: "demo",
		Agents: AgentEnabledFlags{Configured: true},
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	// On DefaultVersion replay the legacy path falls through to
	// makeEnhancedDecision with an empty results slice, which synthesizes
	// REQUEST_REVIEW and posts a comment. The comment running is the
	// observable signal that the early-return branch was bypassed.
	require.Equal(t, int32(1), atomic.LoadInt32(&commentCalls),
		"on DefaultVersion replay the all-agents-disabled early return must NOT fire")
}

// TestPRReviewWorkflow_LegacyHeadSHAMismatchPropagatesOnDefaultVersion —
// Bug 1 regression. On DefaultVersion replay the new head-sha-mismatch
// handling branch must NOT fire; the merge error propagates.
func TestPRReviewWorkflow_LegacyHeadSHAMismatchPropagatesOnDefaultVersion(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.OnGetVersion("pr-review/head-sha-mismatch-handling", workflow.DefaultVersion, 1).
		Return(workflow.DefaultVersion)

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			return temporal.NewApplicationErrorWithCause(
				"head SHA mismatch", "head_sha_mismatch", gitea.ErrHeadSHAMismatch)
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			t.Error("comment activity must not run on DefaultVersion replay (mismatch branch is gated)")
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber: 102, RepoOwner: "octo", RepoName: "demo", HeadSHA: "abc",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(),
		"on DefaultVersion replay the mismatch branch is bypassed; the error must propagate as in legacy history")
}

// TestPRReviewWorkflow_HeadSHAMismatchCommentRetriesOnTransientFailure —
// Bug 2 regression. The head-SHA-mismatch comment uses the dedicated
// notificationActivityOptions (MaxAttempts=3), not mutatingActivityOptions
// (MaxAttempts=1). A single network blip must NOT drop the user-visible
// "please re-trigger" notification.
//
// We make the comment activity fail twice and succeed on the third call;
// the workflow must still report success (which it would on the legacy
// MaxAttempts=1 path too, but only because the error is best-effort) AND
// the call counter must end at 3.
func TestPRReviewWorkflow_HeadSHAMismatchCommentRetriesOnTransientFailure(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			return temporal.NewApplicationErrorWithCause(
				"head SHA mismatch", "head_sha_mismatch", gitea.ErrHeadSHAMismatch)
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)

	var commentCalls int32
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			n := atomic.AddInt32(&commentCalls, 1)
			if n < 3 {
				// Transient: would be retried under MaxAttempts=3 but
				// dropped under MaxAttempts=1.
				return errors.New("transient comment-API blip")
			}
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber: 103, RepoOwner: "octo", RepoName: "demo", HeadSHA: "feedface",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"workflow should still report success on the mismatch path")
	require.Equal(t, int32(3), atomic.LoadInt32(&commentCalls),
		"mismatch comment must retry 3x on transient errors (notificationActivityOptions); got %d", atomic.LoadInt32(&commentCalls))
}

// TestPRReviewWorkflow_PRClosed_TreatedAsBenignSuccess — Bug 6
// regression. When the merge activity returns ErrPRClosed (PR was closed
// during review), the workflow treats it as benign success: log info,
// skip merge, no comment, return nil.
func TestPRReviewWorkflow_PRClosed_TreatedAsBenignSuccess(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(PRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "FetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			return temporal.NewApplicationErrorWithCause(
				"PR is closed", "pr_closed", gitea.ErrPRClosed)
		},
		activity.RegisterOptions{Name: "MergeActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			t.Error("comment activity must not run when PR is closed (would be noise)")
			return nil
		},
		activity.RegisterOptions{Name: "CommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(PRReviewWorkflow, PRReviewArgs{
		PRNumber: 104, RepoOwner: "octo", RepoName: "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"ErrPRClosed must be benign — PR is in a terminal state, workflow cleans up silently")
}

// TestGiteaPRReviewWorkflow_PRClosedDuringComment — Bug 6 regression for
// the comment-side handling. The merge succeeds (or the workflow goes
// down the REQUEST_REVIEW path); the comment activity then fails with
// ErrPRClosed because the PR was closed between merge attempt and the
// comment. The workflow must NOT fail.
func TestGiteaPRReviewWorkflow_PRClosedDuringComment(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	// Two FAILs + one PASS = REQUEST_REVIEW path. We want to exercise the
	// comment activity, not merge.
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Security", Verdict: "FAIL", Rationale: "vuln"}, nil
		},
		activity.RegisterOptions{Name: "SecurityCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Performance", Verdict: "FAIL", Rationale: "slow"}, nil
		},
		activity.RegisterOptions{Name: "PerformanceCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: "Const", Verdict: "PASS"}, nil
		},
		activity.RegisterOptions{Name: "ConstCheckActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			return temporal.NewApplicationErrorWithCause(
				"PR is closed", "pr_closed", gitea.ErrPRClosed)
		},
		activity.RegisterOptions{Name: "GiteaCommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber: 105, RepoOwner: "octo", RepoName: "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"comment-side ErrPRClosed must be benign — PR is in a terminal state")
}

// TestGiteaPRReviewWorkflow_HeadSHAMismatch_CommentFailureStillSucceeds —
// the mismatch path is best-effort: even if the follow-up comment activity
// also fails (e.g. comment API is also down), the workflow should still
// return success. Failing the workflow on a comment-best-effort error
// would just create the same retry storm we're trying to avoid.
func TestGiteaPRReviewWorkflow_HeadSHAMismatch_CommentFailureStillSucceeds(t *testing.T) {
	var s prSuite
	env := s.NewTestWorkflowEnvironment()

	env.RegisterWorkflow(GiteaPRReviewWorkflow)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.FetchDiffArgs) (string, error) {
			return "diff text", nil
		},
		activity.RegisterOptions{Name: "GiteaFetchDiffActivity"},
	)
	pass := func(agent string) func(context.Context, string) (activities.CheckResult, error) {
		return func(_ context.Context, _ string) (activities.CheckResult, error) {
			return activities.CheckResult{Agent: agent, Verdict: "PASS"}, nil
		}
	}
	env.RegisterActivityWithOptions(pass("Security"), activity.RegisterOptions{Name: "SecurityCheckActivity"})
	env.RegisterActivityWithOptions(pass("Performance"), activity.RegisterOptions{Name: "PerformanceCheckActivity"})
	env.RegisterActivityWithOptions(pass("Const"), activity.RegisterOptions{Name: "ConstCheckActivity"})
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.MergeArgs) error {
			return temporal.NewApplicationErrorWithCause(
				"head SHA mismatch", "head_sha_mismatch",
				gitea.ErrHeadSHAMismatch)
		},
		activity.RegisterOptions{Name: "GiteaMergeActivity"},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ activities.CommentArgs) error {
			return errors.New("comment API also down")
		},
		activity.RegisterOptions{Name: "GiteaCommentForHumanReviewActivity"},
	)

	env.ExecuteWorkflow(GiteaPRReviewWorkflow, PRReviewArgs{
		PRNumber:  22,
		RepoOwner: "octo",
		RepoName:  "demo",
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(),
		"comment failure on the head-SHA-mismatch path is best-effort; workflow must still succeed")
}

// /thearray/gogents/workflows/pr_review_workflow.go - Main Temporal workflow for PR review with enhanced metrics
//
// Behavioral changes between versions are gated via workflow.GetVersion so
// in-flight workflows mid-replay after a worker upgrade keep deterministic
// history. Adding a new behavioral change requires a new version key — never
// repurpose an existing one or remove the DefaultVersion branch until every
// in-flight workflow predating the change has drained. See
// https://docs.temporal.io/dev-guide/go/versioning for guidance.
//
// Active version keys (each with version 1 == NEW path):
//   - pr-review/empty-diff-guard
//   - pr-review/all-agents-disabled-static
//   - pr-review/all-agents-disabled-gitea
//   - pr-review/agent-enabled-strict
//   - pr-review/head-sha-mismatch-handling
//   - pr-review/pr-closed-handling
package workflows

import (
	"errors"
	"strings"
	"time"

	"github.com/sirus20x6/adamomaton-platform/temporal/activities"
	"github.com/sirus20x6/adamomaton-platform/temporal/gitea"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// AgentEnabledFlags toggles individual agent checks. The Enabled map is
// keyed by types.AgentType (or its string equivalent — e.g. "Security",
// "Performance", "Const", "Documentation", ...).
//
//   - Configured == false: legacy mode; the Enabled map is ignored, all
//     registered agents run. Preserves replay-safety for in-flight
//     workflows started before the strict-mode change and for callers
//     (like cmd/start-workflow) that have no config loader to populate
//     the map.
//   - Configured == true: strict mode; an agent runs ONLY if its name is
//     present in Enabled with value true. Unknown names default to false
//     so a typo'd agent name (or a newly-added agent the dashboard has
//     not yet been told about) does not silently bypass the operator's
//     allowlist.
//
// The map carries strings (agent type names) rather than the typed
// types.AgentType so it serializes cleanly through Temporal's data
// converter without dragging the types package into the workflow's
// JSON contract. Callers that have access to types.AgentType should
// pass `string(types.AgentSecurity)` etc.
//
// Adding a new agent requires:
//  1. A new types.AgentType constant in internal/types/types.go,
//  2. A new gate in PRReviewWorkflow / GiteaPRReviewWorkflow,
//  3. Populating Enabled[<name>] at every PRReviewArgs construction site.
type AgentEnabledFlags struct {
	// Configured indicates the caller explicitly populated the Enabled map.
	// If false, all known agents are treated as enabled. This sentinel keeps
	// older callers (which leave the field zeroed) running every check.
	Configured bool `json:"configured,omitempty"`

	// Enabled is the per-agent allowlist used when Configured is true. The
	// map is keyed by agent name (types.AgentType string form). When
	// Configured is false the map is ignored entirely.
	Enabled map[string]bool `json:"enabled,omitempty"`
}

// PRReviewArgs contains the input parameters for the PR review workflow
type PRReviewArgs struct {
	PRNumber    int    `json:"pr_number"`
	RepoOwner   string `json:"repo_owner"`
	RepoName    string `json:"repo_name"`
	MergeMethod string `json:"merge_method,omitempty"` // "merge", "squash", "rebase"

	// HeadSHA, when non-empty, pins the merge to the commit observed at
	// review start. Gitea will reject the merge with ErrHeadSHAMismatch if
	// the PR head moves before the merge call lands — closing the
	// force-push race during AI review (E2E trace #28). When empty, the
	// workflow logs a warning and proceeds without the pin so older
	// callers continue to function.
	HeadSHA string `json:"head_sha,omitempty"`

	// Agents lets the caller disable individual agent checks at runtime so the
	// dashboard's per-agent enable/disable switches actually take effect. When
	// Agents.Configured is false (the zero value), the workflow runs every
	// check — older callers that don't set this field continue to behave as
	// before.
	Agents AgentEnabledFlags `json:"agents,omitempty"`
}

// Cross-stream contract (Stream B): activity-side merge errors are wrapped
// with temporal.NewApplicationErrorWithCause(msg, reason, originalErr) where
// `reason` is one of:
//
//	"already_merged"     for gitea.ErrAlreadyMerged
//	"head_sha_mismatch"  for gitea.ErrHeadSHAMismatch
//	"pr_closed"          for gitea.ErrPRClosed
//
// The detection helpers below take two layers:
//
//  1. errors.Is against the typed sentinel for the in-process case (local
//     activity, or a future direct call that bypasses the Temporal data
//     converter — and same-process tests).
//  2. errors.As against *temporal.ApplicationError, checking the .Type()
//     reason string for the cross-activity case where Temporal has
//     collapsed sentinel identity to a serialized string.
//
// We deliberately do NOT fall back to a substring match on err.Error():
// the activity boundary is a trust boundary, and a hostile MCP could spoof
// the original sentinel's text. If the activity didn't wrap with a
// well-known reason, the workflow fails loudly.

// isAlreadyMergedErr reports whether err signals "PR is already merged".
func isAlreadyMergedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gitea.ErrAlreadyMerged) {
		return true
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == "already_merged" {
		return true
	}
	return false
}

// isHeadSHAMismatchErr reports whether err signals "PR head moved during
// review". When this returns true the merge is no longer applicable — the
// verdict was computed against a different commit — and the workflow
// must NOT propagate the error (which would surface a noisy failure for
// what is really "diff has moved on, please re-trigger").
func isHeadSHAMismatchErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gitea.ErrHeadSHAMismatch) {
		return true
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == "head_sha_mismatch" {
		return true
	}
	return false
}

// isPRClosedErr reports whether err signals "PR is closed". Treated by the
// workflow as a benign success: the PR cannot be merged or commented on,
// so the workflow logs and returns nil rather than retrying noisily.
func isPRClosedErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gitea.ErrPRClosed) {
		return true
	}
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == "pr_closed" {
		return true
	}
	return false
}

// makeReviewDecisionForEmptyDiff returns a synthetic decision used when
// FetchDiff / GiteaFetchDiff returns an empty body. Gitea sometimes serves an
// empty diff when it has not yet computed the merge base — proceeding into
// the AI checks with no input would silently produce 3× PASS verdicts (each
// agent sees zero issues in zero text) and trip the auto-merge path. We
// instead force REQUEST_REVIEW with a MEDIUM risk so a human re-triggers the
// review or the upstream Gitea has time to compute the diff.
//
// args is accepted for symmetry with the rest of the decision-building code,
// even though we don't currently embed any of its fields in the synthetic
// decision. Future enhancements (e.g. linking the comment text back to a PR
// number) can pull from it without changing the call site.
func makeReviewDecisionForEmptyDiff(_ PRReviewArgs) ReviewDecision {
	reason := "diff was empty — Gitea may not have computed it yet, please trigger another review"
	return ReviewDecision{
		Action:           "REQUEST_REVIEW",
		PassCount:        0,
		FailCount:        0,
		WarningCount:     0,
		CriticalFailures: nil,
		ComplexityScore:  0,
		RiskLevel:        "MEDIUM",
		FailingReasons:   []string{reason},
		Metrics:          nil,
	}
}

// ReviewDecision represents the final decision with comprehensive analysis
type ReviewDecision struct {
	Action           string                      `json:"action"` // "MERGE" or "REQUEST_REVIEW"
	PassCount        int                         `json:"pass_count"`
	FailCount        int                         `json:"fail_count"`
	WarningCount     int                         `json:"warning_count"`
	CriticalFailures []string                    `json:"critical_failures"`
	ComplexityScore  int                         `json:"complexity_score"`
	RiskLevel        string                      `json:"risk_level"` // "LOW", "MEDIUM", "HIGH", "CRITICAL"
	FailingReasons   []string                    `json:"failing_reasons"`
	Metrics          *activities.AnalysisMetrics `json:"metrics,omitempty"`
}

// agentEnabled returns whether a specific agent is enabled given the flag
// struct. The contract is documented on AgentEnabledFlags: if Configured is
// false, every agent is enabled regardless of the Enabled map.
//
// `strict` controls the behavior for an unknown agent name when Configured
// is true:
//   - strict == false (legacy / DefaultVersion replay): unknown name returns
//     true (run it). This mirrors the pre-refactor behavior so in-flight
//     workflows replaying through GetVersion(DefaultVersion) keep producing
//     the same activity-launch pattern Temporal recorded in their history.
//   - strict == true (current): unknown name returns false (defense in
//     depth — the configured allowlist is authoritative; a typo'd or new
//     agent name should not silently bypass it).
//
// In the legacy (Configured=false) path the historical "all enabled"
// behavior is preserved so unbumped callers keep running every check;
// strict only affects the Configured=true branch.
func agentEnabled(flags AgentEnabledFlags, name string, strict bool) bool {
	if !flags.Configured {
		return true // legacy mode: all agents enabled
	}
	if !strict {
		// DefaultVersion replay path: preserve old behavior. Look up the
		// explicit value; default to true if the name isn't in the map so
		// pre-strict-mode history replays the same activity launches it
		// recorded.
		v, ok := flags.Enabled[name]
		if !ok {
			return true
		}
		return v
	}
	// Strict mode: a missing name reads as false (zero-value of bool from
	// a missing map entry). Operators must opt agents in explicitly.
	return flags.Enabled[name]
}

// defaultActivityOptions returns the standard activity options used for
// idempotent / read-only check activities. State-mutating activities should
// override these via mutatingActivityOptions to disable retries.
func defaultActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    3 * time.Minute, // Increased for enhanced analysis
		ScheduleToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:       30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    1 * time.Minute,
			MaximumAttempts:    3,
		},
	}
}

// mutatingActivityOptions returns activity options for state-mutating
// activities (merge, comment). Retries are disabled (MaximumAttempts: 1)
// because the activity is not safe to run twice — a network blip after the
// upstream service received the request would otherwise cause a double-merge
// or duplicate comment. Operators who want retries must layer their own
// idempotency (e.g. an idempotency key) at the activity implementation
// level.
func mutatingActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    3 * time.Minute,
		ScheduleToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:       30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    1 * time.Minute,
			MaximumAttempts:    1,
		},
	}
}

// notificationActivityOptions returns activity options for the head-SHA-
// mismatch / PR-closed notification comment. Unlike mutatingActivityOptions
// (MaximumAttempts=1), this enables retries (MaximumAttempts=3) because:
//
//   - The user-facing "please re-trigger" notification is the ONLY signal
//     the human gets that the workflow noticed the force-push. Dropping it
//     on a single network blip means the PR sits in limbo with the AI
//     review verdict silently invalidated.
//   - The downside of duplicate notification comments (the human sees two
//     "please re-trigger" comments in a row) is far smaller than the
//     downside of silent failure.
//
// Used ONLY for this comment posting site — the regular merge-failure
// comment path keeps mutatingActivityOptions to avoid duplicate comment
// noise on the typical retry case.
func notificationActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    3 * time.Minute,
		ScheduleToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:       30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    1 * time.Minute,
			MaximumAttempts:    3,
		},
	}
}

// PRReviewWorkflow orchestrates the parallel AI agent reviews with enhanced decision making.
//
// Each enabled agent in args.Agents runs as its own activity. If an agent is
// explicitly disabled (via AgentEnabledFlags) the corresponding check is
// skipped entirely and not counted toward the pass/fail tally. When
// args.Agents.Configured is false (the historical default for callers that
// have not been updated to populate per-agent toggles), every agent runs.
func PRReviewWorkflow(ctx workflow.Context, args PRReviewArgs) error {
	// Configure activity options with extended timeouts for enhanced analysis.
	// Read-only / idempotent activities get the default policy with retries.
	// State-mutating activities (Merge, Comment) override below at their call
	// sites to disable retries.
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions())

	// Step 1: Fetch the PR diff
	var diff string
	fetchArgs := activities.FetchDiffArgs{
		PRNumber:  args.PRNumber,
		RepoOwner: args.RepoOwner,
		RepoName:  args.RepoName,
	}
	err := workflow.ExecuteActivity(ctx, activities.FetchDiffActivity, fetchArgs).Get(ctx, &diff)
	if err != nil {
		return err
	}

	// E2E trace #13: an empty diff would leave each agent producing PASS on
	// zero text and quietly auto-merge. Short-circuit to REQUEST_REVIEW so a
	// human can re-trigger once Gitea has actually computed the diff.
	//
	// Gated by version "pr-review/empty-diff-guard" so in-flight workflows
	// from before this change replay deterministically (they fall through
	// to the regular check pipeline).
	emptyDiffVer := workflow.GetVersion(ctx, "pr-review/empty-diff-guard", workflow.DefaultVersion, 1)
	if emptyDiffVer >= 1 && strings.TrimSpace(diff) == "" {
		workflow.GetLogger(ctx).Warn(
			"empty diff received from upstream — skipping checks and requesting human review",
			"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
		return executeDecision(ctx, args, makeReviewDecisionForEmptyDiff(args))
	}

	// Step 2: Run parallel agent checks with enhanced analysis. Each agent is
	// gated on the AgentEnabledFlags carried by args; disabled agents are
	// skipped entirely (not counted toward pass/fail).
	//
	// Gated by version "pr-review/agent-enabled-strict": when at NEW path,
	// an unknown agent name with Configured=true is treated as disabled.
	// On replay of pre-change history this returns true (legacy behavior)
	// so the same activity-launch pattern is reproduced.
	strictVer := workflow.GetVersion(ctx, "pr-review/agent-enabled-strict", workflow.DefaultVersion, 1)
	strict := strictVer >= 1
	type pendingCheck struct {
		name   string
		future workflow.Future
	}
	var pending []pendingCheck

	if agentEnabled(args.Agents, "Security", strict) {
		pending = append(pending, pendingCheck{
			name:   "Security",
			future: workflow.ExecuteActivity(ctx, activities.SecurityCheckActivity, diff),
		})
	}
	if agentEnabled(args.Agents, "Performance", strict) {
		pending = append(pending, pendingCheck{
			name:   "Performance",
			future: workflow.ExecuteActivity(ctx, activities.PerformanceCheckActivity, diff),
		})
	}
	if agentEnabled(args.Agents, "Const", strict) {
		pending = append(pending, pendingCheck{
			name:   "Const",
			future: workflow.ExecuteActivity(ctx, activities.ConstCheckActivity, diff),
		})
	}

	// Wait for all results, returning on the first error so we don't
	// orphan futures (Temporal already awaits them on workflow exit).
	results := make([]activities.CheckResult, 0, len(pending))
	for _, p := range pending {
		var r activities.CheckResult
		if err := p.future.Get(ctx, &r); err != nil {
			return err
		}
		results = append(results, r)
	}

	// Empty-results guard: if every agent was disabled (Configured=true with
	// all per-agent flags false, or Configured=true with only unknown
	// agent names) no checks ran and there is nothing to report. Falling
	// through to makeEnhancedDecision would synthesize a REQUEST_REVIEW
	// with an empty failingReasons list and post a confusing comment that
	// claimed agents flagged issues. Exit cleanly: log, no comment, no
	// merge, run completes as a no-op.
	//
	// Gated by version "pr-review/all-agents-disabled-static" — pre-change
	// history replays into the legacy fall-through path.
	allDisabledVer := workflow.GetVersion(ctx, "pr-review/all-agents-disabled-static", workflow.DefaultVersion, 1)
	if allDisabledVer >= 1 && len(results) == 0 {
		workflow.GetLogger(ctx).Warn(
			"all agents disabled, skipping review",
			"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
		return nil
	}

	// Step 3: Perform enhanced decision analysis
	decision := makeEnhancedDecision(results)

	// Log decision for debugging
	workflow.GetLogger(ctx).Info("Review decision made",
		"action", decision.Action,
		"pass_count", decision.PassCount,
		"risk_level", decision.RiskLevel,
		"complexity_score", decision.ComplexityScore)

	// Step 4: Execute decision with enhanced logic
	return executeDecision(ctx, args, decision)
}

// makeEnhancedDecision analyzes results and makes an intelligent decision based on enhanced metrics
func makeEnhancedDecision(results []activities.CheckResult) ReviewDecision {
	passCount := 0
	failCount := 0
	warningCount := 0
	var failingReasons []string
	var criticalFailures []string
	var combinedMetrics *activities.AnalysisMetrics
	maxComplexityScore := 0

	// Analyze each result
	for _, result := range results {
		switch result.Verdict {
		case "PASS":
			passCount++
		case "WARNING":
			warningCount++
			failingReasons = append(failingReasons, result.Agent+" (warning): "+result.Rationale)
		default: // "FAIL" or unknown
			failCount++
			failingReasons = append(failingReasons, result.Agent+": "+result.Rationale)

			// Check for critical failures
			if result.Agent == "Security" {
				criticalFailures = append(criticalFailures, "Security vulnerability detected")
			}
		}

		// Collect metrics from results. Copy the first non-nil metrics
		// struct rather than aliasing the pointer: aliasing would let
		// later code (or a future divergence between agents' metrics)
		// silently mutate one agent's result through `combinedMetrics`.
		if result.Metrics != nil {
			if combinedMetrics == nil {
				cp := *result.Metrics
				combinedMetrics = &cp
			}
			if result.Metrics.ComplexityScore > maxComplexityScore {
				maxComplexityScore = result.Metrics.ComplexityScore
			}
		}
	}

	// Determine risk level based on hard pass/fail counts only.
	// Warnings are excluded here so risk assessment stays conservative.
	riskLevel := calculateRiskLevel(passCount, failCount, criticalFailures, combinedMetrics)

	// Make merge decision using warnings as soft passes for eligibility.
	// This means warnings don't escalate risk but do count toward the 2/3 merge threshold.
	action := determineAction(passCount+warningCount, failCount, riskLevel, combinedMetrics)

	return ReviewDecision{
		Action:           action,
		PassCount:        passCount,
		FailCount:        failCount,
		WarningCount:     warningCount,
		CriticalFailures: criticalFailures,
		ComplexityScore:  maxComplexityScore,
		RiskLevel:        riskLevel,
		FailingReasons:   failingReasons,
		Metrics:          combinedMetrics,
	}
}

// calculateRiskLevel determines risk based on multiple factors
func calculateRiskLevel(passCount, failCount int, criticalFailures []string, metrics *activities.AnalysisMetrics) string {
	// Critical risk conditions
	if len(criticalFailures) > 0 {
		return "CRITICAL"
	}

	if metrics != nil {
		// High risk if critical files are modified with failures
		if len(metrics.CriticalFiles) > 0 && failCount > 0 {
			return "HIGH"
		}

		// High risk for very complex changes with failures
		if metrics.ComplexityScore >= 4 && failCount > 0 {
			return "HIGH"
		}

		// Medium risk for multiple failures or high complexity with failures
		if failCount >= 2 || (metrics.ComplexityScore >= 3 && failCount > 0) {
			return "MEDIUM"
		}
	} else {
		// No metrics: assess based on failure count alone
		if failCount >= 2 {
			return "MEDIUM"
		}
	}

	// Low risk for simple changes with minimal failures
	return "LOW"
}

// determineAction decides whether to merge or request review based on enhanced criteria
func determineAction(passCount, failCount int, riskLevel string, metrics *activities.AnalysisMetrics) string {
	// Never auto-merge critical or high-risk changes
	if riskLevel == "CRITICAL" || riskLevel == "HIGH" {
		return "REQUEST_REVIEW"
	}

	// For medium risk, require all agents to pass
	if riskLevel == "MEDIUM" && passCount < 3 {
		return "REQUEST_REVIEW"
	}

	// For low risk, use traditional 2/3 majority
	if passCount >= 2 {
		// Additional safety check for test files - only when not all agents passed
		if failCount > 0 && metrics != nil && len(metrics.TestFiles) == 0 && len(metrics.AffectedFiles) > 0 {
			// No tests modified/added for production code changes - request review
			nonTestFiles := 0
			for _, file := range metrics.AffectedFiles {
				if !activities.IsTestFile(file) {
					nonTestFiles++
				}
			}
			if nonTestFiles > 0 {
				return "REQUEST_REVIEW"
			}
		}

		return "MERGE"
	}

	return "REQUEST_REVIEW"
}

// headSHAMismatchComment returns the comment body that the workflow posts
// when Gitea rejects a merge because the PR's head moved during the AI
// review. The text is shared between executeDecision and
// executeGiteaDecision so the two paths cannot drift.
const headSHAMismatchComment = "PR head moved during review — please re-trigger the AI review against the new commit."

// executeDecision executes the final decision with appropriate activities.
//
// State-mutating activities (MergeActivity, CommentForHumanReviewActivity)
// run with mutatingActivityOptions which disables retries — a network blip
// after the upstream service received the request would otherwise cause a
// double-merge or duplicate comment, which is far worse than a one-off
// failure that requires a human re-trigger.
//
// gitea.ErrAlreadyMerged is treated as a benign success: another actor
// merged the PR out-of-band while the AI review was running. The workflow
// returns nil (and logs) rather than failing because the desired end state
// is already in place.
//
// gitea.ErrHeadSHAMismatch (force-push during review) is also treated as a
// benign outcome: the verdict was computed against a different commit and
// no longer applies. The workflow logs a warning, posts a comment asking
// the human to re-trigger, and returns success rather than failing the run
// (which would otherwise generate noise + retry storms).
func executeDecision(ctx workflow.Context, args PRReviewArgs, decision ReviewDecision) error {
	mutateCtx := workflow.WithActivityOptions(ctx, mutatingActivityOptions())
	// Version gates for behavioral changes that alter post-decision history
	// shape. Pre-change replays take the legacy (DefaultVersion) path.
	mismatchVer := workflow.GetVersion(ctx, "pr-review/head-sha-mismatch-handling", workflow.DefaultVersion, 1)
	prClosedVer := workflow.GetVersion(ctx, "pr-review/pr-closed-handling", workflow.DefaultVersion, 1)

	if decision.Action == "MERGE" {
		if args.HeadSHA == "" {
			workflow.GetLogger(ctx).Warn(
				"merging without SHA pin — vulnerable to force-push race",
				"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
		}
		mergeArgs := activities.MergeArgs{
			PRNumber:    args.PRNumber,
			RepoOwner:   args.RepoOwner,
			RepoName:    args.RepoName,
			MergeMethod: args.MergeMethod,
			HeadSHA:     args.HeadSHA,
		}
		err := workflow.ExecuteActivity(mutateCtx, activities.MergeActivity, mergeArgs).Get(mutateCtx, nil)
		if isAlreadyMergedErr(err) {
			workflow.GetLogger(ctx).Info(
				"PR was already merged out-of-band; AI review concurred",
				"pr", args.PRNumber, "verdict", decision.Action)
			return nil
		}
		// PR closed during review: the merge cannot proceed and a comment
		// would be noise on a closed PR. Log and return nil. Gated so
		// pre-change replay takes the legacy "return err" path.
		if prClosedVer >= 1 && isPRClosedErr(err) {
			workflow.GetLogger(ctx).Info(
				"PR closed during review — skipping merge",
				"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
			return nil
		}
		if mismatchVer >= 1 && isHeadSHAMismatchErr(err) {
			workflow.GetLogger(ctx).Warn(
				"PR head moved during review (force-push race) — verdict no longer applies, asking for re-trigger",
				"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName,
				"head_sha", args.HeadSHA, "error", err.Error())
			commentArgs := activities.CommentArgs{
				PRNumber:       args.PRNumber,
				RepoOwner:      args.RepoOwner,
				RepoName:       args.RepoName,
				FailingReasons: []string{headSHAMismatchComment},
			}
			// Use notificationActivityOptions (MaxAttempts=3) for this
			// specific comment: dropping the user-visible "please re-
			// trigger" notification on a single network blip is a worse
			// failure mode than the duplicate-comment downside that
			// motivated mutatingActivityOptions for the regular path.
			notifyCtx := workflow.WithActivityOptions(ctx, notificationActivityOptions())
			cerr := workflow.ExecuteActivity(notifyCtx, activities.CommentForHumanReviewActivity, commentArgs).Get(notifyCtx, nil)
			if cerr != nil {
				if prClosedVer >= 1 && isPRClosedErr(cerr) {
					// PR closed between the merge attempt and the comment;
					// gracefully treat as benign success (no point posting
					// to a closed PR).
					workflow.GetLogger(ctx).Info(
						"PR closed before head-SHA-mismatch comment could post — proceeding with success",
						"pr", args.PRNumber)
					return nil
				}
				// Best-effort: log and return success. Propagating here
				// would just turn a "PR moved on" signal into a retry
				// storm (the AI review verdict no longer applies).
				workflow.GetLogger(ctx).Warn(
					"failed to post head-SHA-mismatch comment; proceeding with success anyway",
					"pr", args.PRNumber, "error", cerr.Error())
			}
			return nil
		}
		return err
	}

	commentArgs := activities.CommentArgs{
		PRNumber:       args.PRNumber,
		RepoOwner:      args.RepoOwner,
		RepoName:       args.RepoName,
		FailingReasons: decision.FailingReasons,
		Metrics:        decision.Metrics,
	}
	cerr := workflow.ExecuteActivity(mutateCtx, activities.CommentForHumanReviewActivity, commentArgs).Get(mutateCtx, nil)
	if prClosedVer >= 1 && isPRClosedErr(cerr) {
		// Comment activity discovered the PR is closed — that's a benign
		// terminal state, not a workflow failure.
		workflow.GetLogger(ctx).Info(
			"PR closed before review comment could post — workflow exits cleanly",
			"pr", args.PRNumber)
		return nil
	}
	return cerr
}

// GiteaPRReviewWorkflow orchestrates AI agent reviews for Gitea-hosted PRs.
//
// Behavior mirrors PRReviewWorkflow: per-agent enable flags carried by
// args.Agents gate the individual check activities, and the state-mutating
// merge/comment activities run with retries disabled.
//
// TODO(architecture): GiteaPRReviewWorkflow currently still calls the same
// MCP-based check activities as the GitHub workflow. This is intentional
// (the LLM analysis path is upstream-agnostic) but documented here because
// future work to add Gitea-native check activities should not silently
// rewire the dispatch.
func GiteaPRReviewWorkflow(ctx workflow.Context, args PRReviewArgs) error {
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions())

	var diff string
	fetchArgs := activities.FetchDiffArgs{
		PRNumber:  args.PRNumber,
		RepoOwner: args.RepoOwner,
		RepoName:  args.RepoName,
	}
	if err := workflow.ExecuteActivity(ctx, activities.GiteaFetchDiffActivity, fetchArgs).Get(ctx, &diff); err != nil {
		return err
	}

	// E2E trace #13: an empty diff from Gitea (computed-too-early or
	// returns-nothing) would silently produce 3× PASS verdicts because each
	// agent sees zero issues in zero text. Force REQUEST_REVIEW with a
	// MEDIUM risk so the human re-triggers the review.
	//
	// Same version key as the static workflow: the change-point semantics
	// are shared (the helper returns the synthetic REQUEST_REVIEW; the
	// caller's branch is what differs by workflow).
	emptyDiffVer := workflow.GetVersion(ctx, "pr-review/empty-diff-guard", workflow.DefaultVersion, 1)
	if emptyDiffVer >= 1 && strings.TrimSpace(diff) == "" {
		workflow.GetLogger(ctx).Warn(
			"empty diff received from Gitea — skipping checks and requesting human review",
			"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
		return executeGiteaDecision(ctx, args, makeReviewDecisionForEmptyDiff(args))
	}

	strictVer := workflow.GetVersion(ctx, "pr-review/agent-enabled-strict", workflow.DefaultVersion, 1)
	strict := strictVer >= 1
	type pendingCheck struct {
		name   string
		future workflow.Future
	}
	var pending []pendingCheck

	if agentEnabled(args.Agents, "Security", strict) {
		pending = append(pending, pendingCheck{
			name:   "Security",
			future: workflow.ExecuteActivity(ctx, activities.SecurityCheckActivity, diff),
		})
	}
	if agentEnabled(args.Agents, "Performance", strict) {
		pending = append(pending, pendingCheck{
			name:   "Performance",
			future: workflow.ExecuteActivity(ctx, activities.PerformanceCheckActivity, diff),
		})
	}
	if agentEnabled(args.Agents, "Const", strict) {
		pending = append(pending, pendingCheck{
			name:   "Const",
			future: workflow.ExecuteActivity(ctx, activities.ConstCheckActivity, diff),
		})
	}

	results := make([]activities.CheckResult, 0, len(pending))
	for _, p := range pending {
		var r activities.CheckResult
		if err := p.future.Get(ctx, &r); err != nil {
			return err
		}
		results = append(results, r)
	}

	// Empty-results guard — see PRReviewWorkflow for rationale. Distinct
	// version key from the static variant so the two workflows can evolve
	// independently if a future change diverges between them.
	allDisabledVer := workflow.GetVersion(ctx, "pr-review/all-agents-disabled-gitea", workflow.DefaultVersion, 1)
	if allDisabledVer >= 1 && len(results) == 0 {
		workflow.GetLogger(ctx).Warn(
			"all agents disabled, skipping review",
			"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
		return nil
	}

	decision := makeEnhancedDecision(results)
	workflow.GetLogger(ctx).Info("Gitea review decision made",
		"action", decision.Action,
		"pass_count", decision.PassCount,
		"risk_level", decision.RiskLevel,
		"complexity_score", decision.ComplexityScore)

	return executeGiteaDecision(ctx, args, decision)
}

// executeGiteaDecision mirrors executeDecision for Gitea-hosted PRs. The
// merge/comment activities run with retries disabled for the same
// double-mutation reasons documented on executeDecision.
//
// gitea.ErrAlreadyMerged is treated as a benign success: a maintainer
// merged the PR while the AI review was running. We return nil so the
// workflow doesn't fail spuriously.
//
// gitea.ErrHeadSHAMismatch (force-push during review) is treated as a
// benign outcome too: post a "please re-trigger" comment and return nil.
// See executeDecision for the full rationale.
func executeGiteaDecision(ctx workflow.Context, args PRReviewArgs, decision ReviewDecision) error {
	mutateCtx := workflow.WithActivityOptions(ctx, mutatingActivityOptions())
	mismatchVer := workflow.GetVersion(ctx, "pr-review/head-sha-mismatch-handling", workflow.DefaultVersion, 1)
	prClosedVer := workflow.GetVersion(ctx, "pr-review/pr-closed-handling", workflow.DefaultVersion, 1)

	if decision.Action == "MERGE" {
		if args.HeadSHA == "" {
			workflow.GetLogger(ctx).Warn(
				"merging without SHA pin — vulnerable to force-push race",
				"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
		}
		mergeArgs := activities.MergeArgs{
			PRNumber:    args.PRNumber,
			RepoOwner:   args.RepoOwner,
			RepoName:    args.RepoName,
			MergeMethod: args.MergeMethod,
			HeadSHA:     args.HeadSHA,
		}
		err := workflow.ExecuteActivity(mutateCtx, activities.GiteaMergeActivity, mergeArgs).Get(mutateCtx, nil)
		if isAlreadyMergedErr(err) {
			workflow.GetLogger(ctx).Info(
				"PR was already merged out-of-band; AI review concurred",
				"pr", args.PRNumber, "verdict", decision.Action)
			return nil
		}
		if prClosedVer >= 1 && isPRClosedErr(err) {
			workflow.GetLogger(ctx).Info(
				"PR closed during review — skipping merge",
				"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName)
			return nil
		}
		if mismatchVer >= 1 && isHeadSHAMismatchErr(err) {
			workflow.GetLogger(ctx).Warn(
				"PR head moved during review (force-push race) — verdict no longer applies, asking for re-trigger",
				"pr", args.PRNumber, "repo", args.RepoOwner+"/"+args.RepoName,
				"head_sha", args.HeadSHA, "error", err.Error())
			commentArgs := activities.CommentArgs{
				PRNumber:       args.PRNumber,
				RepoOwner:      args.RepoOwner,
				RepoName:       args.RepoName,
				FailingReasons: []string{headSHAMismatchComment},
			}
			notifyCtx := workflow.WithActivityOptions(ctx, notificationActivityOptions())
			cerr := workflow.ExecuteActivity(notifyCtx, activities.GiteaCommentForHumanReviewActivity, commentArgs).Get(notifyCtx, nil)
			if cerr != nil {
				if prClosedVer >= 1 && isPRClosedErr(cerr) {
					workflow.GetLogger(ctx).Info(
						"PR closed before head-SHA-mismatch comment could post — proceeding with success",
						"pr", args.PRNumber)
					return nil
				}
				workflow.GetLogger(ctx).Warn(
					"failed to post head-SHA-mismatch comment; proceeding with success anyway",
					"pr", args.PRNumber, "error", cerr.Error())
			}
			return nil
		}
		return err
	}

	commentArgs := activities.CommentArgs{
		PRNumber:       args.PRNumber,
		RepoOwner:      args.RepoOwner,
		RepoName:       args.RepoName,
		FailingReasons: decision.FailingReasons,
		Metrics:        decision.Metrics,
	}
	cerr := workflow.ExecuteActivity(mutateCtx, activities.GiteaCommentForHumanReviewActivity, commentArgs).Get(mutateCtx, nil)
	if prClosedVer >= 1 && isPRClosedErr(cerr) {
		workflow.GetLogger(ctx).Info(
			"PR closed before review comment could post — workflow exits cleanly",
			"pr", args.PRNumber)
		return nil
	}
	return cerr
}

// Package errclass assigns a stable, low-cardinality error class to
// workflow / activity failures so dashboards can plot "workflows failed
// by error_class" and alerts can key on a fixed vocabulary.
//
// The vocabulary is CLOSED — exactly five values:
//
//	timeout        — the operation ran out of time (Temporal timeouts,
//	                 context deadline, net timeouts).
//	panic          — a panic was converted into an error by the SDK
//	                 (activity or workflow panics).
//	external-api   — an upstream service misbehaved: network/transport
//	                 errors, connection refused/reset, or an
//	                 ApplicationError whose Type marks an upstream API
//	                 (gitea, github, http, llm...).
//	validation     — the input or preconditions were wrong and a retry
//	                 with the same input can never succeed (config
//	                 errors, precondition failures like pr_closed).
//	activity-error — everything else: the activity ran and returned a
//	                 business-logic failure we can't attribute more
//	                 precisely.
//
// Do NOT add values without updating every dashboard/alert that consumes
// the enum; do NOT put error messages, IDs, or types into metric labels —
// that's what logs are for.
package errclass

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The closed error-class vocabulary. Keep in sync with the package doc.
const (
	ClassTimeout       = "timeout"
	ClassPanic         = "panic"
	ClassActivityError = "activity-error"
	ClassExternalAPI   = "external-api"
	ClassValidation    = "validation"
)

// WorkflowFailures counts terminal workflow failures by workflow type and
// error class. workflow_type is the registered workflow function name
// ("PRReviewWorkflow", "DelegationWorkflow", ...) — bounded by the set of
// workflows compiled into the binary. error_class is one of the Class*
// constants — bounded by the closed vocabulary above.
var WorkflowFailures = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gogents_workflow_failures_total",
		Help: "Terminal Temporal workflow failures, by workflow type and error class (timeout|panic|activity-error|external-api|validation).",
	},
	[]string{"workflow_type", "error_class"},
)

func init() {
	prometheus.MustRegister(WorkflowFailures)
}

// validationTypes are ApplicationError Type() values that mean "the input
// or precondition is wrong; retrying with the same input cannot succeed".
// These match the types activities in this repo already emit.
var validationTypes = map[string]bool{
	"validation":            true,
	"invalid_input":         true,
	"ConfigError":           true,
	"DelegationConfigError": true,
	// PR-review merge preconditions (pr_review_activities.go): the world
	// changed under the workflow; the request as-issued is no longer valid.
	"already_merged":    true,
	"head_sha_mismatch": true,
	"pr_closed":         true,
}

// externalAPITypes are ApplicationError Type() values that mean "an
// upstream service failed". Matched case-insensitively.
var externalAPITypes = map[string]bool{
	"external-api": true,
	"external_api": true,
	"http":         true,
	"gitea":        true,
	"github":       true,
	"llm":          true,
	"vllm":         true,
	"mcp":          true,
}

// Classify maps err to one of the five Class* constants. nil maps to "".
// The mapping walks the whole Unwrap chain, so Temporal's wrappers
// (WorkflowExecutionError -> ActivityError -> cause) resolve to the class
// of the underlying cause.
func Classify(err error) string {
	if err == nil {
		return ""
	}

	// Timeouts first: a timed-out activity surfaces as *TimeoutError
	// regardless of what the activity was doing at the time.
	var timeoutErr *temporal.TimeoutError
	if errors.As(err, &timeoutErr) {
		return ClassTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}

	var panicErr *temporal.PanicError
	if errors.As(err, &panicErr) {
		return ClassPanic
	}

	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		t := appErr.Type()
		if validationTypes[t] {
			return ClassValidation
		}
		if externalAPITypes[strings.ToLower(t)] {
			return ClassExternalAPI
		}
		// An untyped / unrecognized ApplicationError is a plain
		// business-logic failure — unless its cause classifies more
		// precisely below.
	}

	// Transport-level failures from upstream calls. *url.Error and
	// *net.OpError both implement net.Error; a timeout flavour of either
	// still counts as timeout.
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ClassTimeout
		}
		return ClassExternalAPI
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return ClassExternalAPI
	}

	return ClassActivityError
}

// RecordWorkflowFailure classifies err, increments WorkflowFailures (a
// side effect, so it is skipped during history replay), and returns the
// class so the caller can attach it to its failure log line. Returns ""
// (and records nothing) for a nil err.
//
// Call this exactly once per terminal workflow failure — at the point the
// workflow function is about to return the error — not on every retryable
// activity hiccup.
func RecordWorkflowFailure(ctx workflow.Context, workflowType string, err error) string {
	class := Classify(err)
	if class == "" {
		return ""
	}
	if !workflow.IsReplaying(ctx) {
		WorkflowFailures.WithLabelValues(workflowType, class).Inc()
	}
	return class
}

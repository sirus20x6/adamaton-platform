package workerentry

// Tests for the dispatch worker wiring: Register must install the two
// workflows and five activities under the exact names the workflow code
// references by string, and the Prometheus interceptor must observe both
// success and failure activity executions without panicking.

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// fakeWorker captures registration calls. It embeds worker.Worker for
// interface satisfaction; only the two ...WithOptions methods are ever
// invoked by Register, so the nil embedded interface is never touched.
type fakeWorker struct {
	worker.Worker
	workflowNames []string
	activityNames []string
}

func (f *fakeWorker) RegisterWorkflowWithOptions(wf interface{}, options workflow.RegisterOptions) {
	f.workflowNames = append(f.workflowNames, options.Name)
}

func (f *fakeWorker) RegisterActivityWithOptions(a interface{}, options activity.RegisterOptions) {
	f.activityNames = append(f.activityNames, options.Name)
}

func TestRegisterInstallsCanonicalNames(t *testing.T) {
	fw := &fakeWorker{}

	// Deps can be zero-valued: registration only constructs the activity
	// structs, it never dials the pool or the Temporal client.
	Register(fw, Deps{})

	wantWorkflows := []string{"BatchCoordinator", "DispatchWorkflow"}
	gotWorkflows := append([]string(nil), fw.workflowNames...)
	sort.Strings(gotWorkflows)
	if len(gotWorkflows) != len(wantWorkflows) {
		t.Fatalf("workflows registered = %v, want %v", gotWorkflows, wantWorkflows)
	}
	for i := range wantWorkflows {
		if gotWorkflows[i] != wantWorkflows[i] {
			t.Errorf("workflow[%d] = %q, want %q", i, gotWorkflows[i], wantWorkflows[i])
		}
	}

	// These names are the string constants dispatch/workflows/dispatch.go
	// uses for ExecuteActivity — a drift here breaks dispatch at runtime,
	// not at compile time.
	wantActivities := []string{
		"AssignWorker",
		"EnsureBatchCoordinator",
		"MarkJobStatus",
		"RecordJob",
		"SelectCandidates",
	}
	gotActivities := append([]string(nil), fw.activityNames...)
	sort.Strings(gotActivities)
	if len(gotActivities) != len(wantActivities) {
		t.Fatalf("activities registered = %v, want %v", gotActivities, wantActivities)
	}
	for i := range wantActivities {
		if gotActivities[i] != wantActivities[i] {
			t.Errorf("activity[%d] = %q, want %q", i, gotActivities[i], wantActivities[i])
		}
	}
}

func TestInterceptorObservesSuccessAndFailure(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.SetWorkerOptions(worker.Options{
		Interceptors: []interceptor.WorkerInterceptor{Interceptor()},
	})

	okActivity := func(ctx context.Context) (string, error) { return "fine", nil }
	failActivity := func(ctx context.Context) error { return errors.New("nope") }
	env.RegisterActivityWithOptions(okActivity, activity.RegisterOptions{Name: "InterceptOK"})
	env.RegisterActivityWithOptions(failActivity, activity.RegisterOptions{Name: "InterceptFail"})

	failuresBefore := readFailureCount(t, "InterceptFail")

	val, err := env.ExecuteActivity(okActivity)
	if err != nil {
		t.Fatalf("ok activity errored through interceptor: %v", err)
	}
	var out string
	if err := val.Get(&out); err != nil || out != "fine" {
		t.Fatalf("ok activity result = %q, %v", out, err)
	}

	if _, err := env.ExecuteActivity(failActivity); err == nil {
		t.Fatal("failing activity returned nil error")
	}

	failuresAfter := readFailureCount(t, "InterceptFail")
	if failuresAfter != failuresBefore+1 {
		t.Errorf("failure counter delta = %v, want +1 (before %v after %v)",
			failuresAfter-failuresBefore, failuresBefore, failuresAfter)
	}
	// The success path must not bump the failure counter.
	if got := readFailureCount(t, "InterceptOK"); got != 0 {
		t.Errorf("failure counter for successful activity = %v, want 0", got)
	}
}

// readFailureCount pulls the current value of the package's failure
// counter for one activity-name label.
func readFailureCount(t *testing.T, name string) float64 {
	t.Helper()
	return testutil.ToFloat64(dispatchActivityFailures.WithLabelValues(name))
}

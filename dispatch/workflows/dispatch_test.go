package workflows_test

// Tests for DispatchWorkflow via the Temporal test suite. Activities are
// registered as stubs under the string names the workflow references, so
// these tests exercise the real workflow decision tree (one-shot vs
// batched, no-workers rejection, bad-payload rejection, child failure,
// coordinator timeout) without a database or a Temporal server.

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

// stubRecorder collects every activity invocation the workflow makes so
// tests can assert on the ledger-write sequence.
type stubRecorder struct {
	mu       sync.Mutex
	statuses []string // MarkJobStatus statuses, in order
	jobID    string   // captured from RecordJob
	assign   *workflows.AssignInput
	ensureIn *workflows.EnsureBatchCoordinatorInput

	candidates []workflows.Candidate
	recordErr  error
	ensureErr  error
}

func (s *stubRecorder) register(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(in workflows.RecordJobInput) (*workflows.RecordJobResult, error) {
		s.mu.Lock()
		s.jobID = in.JobID
		s.mu.Unlock()
		if s.recordErr != nil {
			return nil, s.recordErr
		}
		return &workflows.RecordJobResult{JobID: in.JobID, CreatedAt: time.Unix(1700000000, 0)}, nil
	}, activity.RegisterOptions{Name: "RecordJob"})

	env.RegisterActivityWithOptions(func(in workflows.SelectInput) ([]workflows.Candidate, error) {
		return s.candidates, nil
	}, activity.RegisterOptions{Name: "SelectCandidates"})

	env.RegisterActivityWithOptions(func(in workflows.AssignInput) error {
		s.mu.Lock()
		s.assign = &in
		s.mu.Unlock()
		return nil
	}, activity.RegisterOptions{Name: "AssignWorker"})

	env.RegisterActivityWithOptions(func(in workflows.MarkStatusInput) error {
		s.mu.Lock()
		s.statuses = append(s.statuses, in.Status)
		s.mu.Unlock()
		return nil
	}, activity.RegisterOptions{Name: "MarkJobStatus"})

	env.RegisterActivityWithOptions(func(in workflows.EnsureBatchCoordinatorInput) (*workflows.EnsureBatchCoordinatorResult, error) {
		s.mu.Lock()
		s.ensureIn = &in
		s.mu.Unlock()
		if s.ensureErr != nil {
			return nil, s.ensureErr
		}
		return &workflows.EnsureBatchCoordinatorResult{
			WorkflowID: "batch-" + in.Queue,
			RunID:      "run-1",
		}, nil
	}, activity.RegisterOptions{Name: "EnsureBatchCoordinator"})
}

func (s *stubRecorder) recordedStatuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.statuses))
	copy(out, s.statuses)
	return out
}

func (s *stubRecorder) capturedJobID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobID
}

func newDispatchEnv(t *testing.T, stub *stubRecorder) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(workflows.DispatchWorkflow,
		sdkworkflow.RegisterOptions{Name: workflows.WorkflowDispatch})
	stub.register(env)
	return env
}

func oneCandidate() []workflows.Candidate {
	return []workflows.Candidate{{WorkerID: "worker-a", Queue: "gpu-large", LoadScore: 0, StalenessS: 3}}
}

func baseSpec() workflows.JobSpec {
	return workflows.JobSpec{
		Kind:         "TestChildWorkflow",
		Payload:      json.RawMessage(`{"n": 7}`),
		Requirements: workflows.Requirements{QueueClass: "gpu-large"},
	}
}

func TestDispatchWorkflowOneShotSuccess(t *testing.T) {
	stub := &stubRecorder{candidates: oneCandidate()}
	env := newDispatchEnv(t, stub)

	var gotChildArg map[string]any
	env.RegisterWorkflowWithOptions(func(ctx sdkworkflow.Context, arg map[string]any) (string, error) {
		gotChildArg = arg
		return "child-ok", nil
	}, sdkworkflow.RegisterOptions{Name: "TestChildWorkflow"})

	env.ExecuteWorkflow(workflows.DispatchWorkflow, baseSpec())

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res workflows.DispatchResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != workflows.StatusSucceeded {
		t.Errorf("status = %q, want %q", res.Status, workflows.StatusSucceeded)
	}
	if res.AssignedWorker != "worker-a" || res.AssignedQueue != "gpu-large" {
		t.Errorf("assignment = %q/%q, want worker-a/gpu-large", res.AssignedWorker, res.AssignedQueue)
	}
	if res.JobID == "" {
		t.Error("JobID empty")
	}
	if res.ChildWorkflow != "child-"+res.JobID {
		t.Errorf("ChildWorkflow = %q, want child-%s", res.ChildWorkflow, res.JobID)
	}
	// Payload must be delivered decoded (map), not as raw bytes.
	if got := gotChildArg["n"]; got != float64(7) {
		t.Errorf("child arg n = %v, want 7", got)
	}
	wantStatuses := []string{workflows.StatusRunning, workflows.StatusSucceeded}
	got := stub.recordedStatuses()
	if len(got) != 2 || got[0] != wantStatuses[0] || got[1] != wantStatuses[1] {
		t.Errorf("status sequence = %v, want %v", got, wantStatuses)
	}
	if stub.assign == nil {
		t.Fatal("AssignWorker never called")
	}
	if stub.assign.AssignedWorker != "worker-a" || stub.assign.WorkflowID != "child-"+res.JobID {
		t.Errorf("assign input = %+v", stub.assign)
	}
}

func TestDispatchWorkflowNoWorkers(t *testing.T) {
	stub := &stubRecorder{candidates: nil} // empty candidate list
	env := newDispatchEnv(t, stub)

	env.ExecuteWorkflow(workflows.DispatchWorkflow, baseSpec())

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected ApplicationError, got %T: %v", err, err)
	}
	if appErr.Type() != "NoWorkers" {
		t.Errorf("error type = %q, want NoWorkers", appErr.Type())
	}
	if !appErr.NonRetryable() {
		t.Error("NoWorkers error must be non-retryable")
	}
	got := stub.recordedStatuses()
	if len(got) != 1 || got[0] != workflows.StatusNoWorkers {
		t.Errorf("status sequence = %v, want [no_workers]", got)
	}
}

func TestDispatchWorkflowBadPayload(t *testing.T) {
	stub := &stubRecorder{candidates: oneCandidate()}
	env := newDispatchEnv(t, stub)

	spec := baseSpec()
	// Valid JSON (so the spec itself serializes as workflow input) but
	// not an object — the workflow's map-decode step must reject it.
	spec.Payload = json.RawMessage(`[1, 2, 3]`)
	env.ExecuteWorkflow(workflows.DispatchWorkflow, spec)

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected ApplicationError, got %T: %v", err, err)
	}
	if appErr.Type() != "BadPayload" {
		t.Errorf("error type = %q, want BadPayload", appErr.Type())
	}
	got := stub.recordedStatuses()
	if len(got) != 1 || got[0] != workflows.StatusFailed {
		t.Errorf("status sequence = %v, want [failed]", got)
	}
}

func TestDispatchWorkflowChildFailure(t *testing.T) {
	stub := &stubRecorder{candidates: oneCandidate()}
	env := newDispatchEnv(t, stub)

	env.RegisterWorkflowWithOptions(func(ctx sdkworkflow.Context, arg map[string]any) (string, error) {
		return "", errors.New("boom from child")
	}, sdkworkflow.RegisterOptions{Name: "TestChildWorkflow"})

	env.ExecuteWorkflow(workflows.DispatchWorkflow, baseSpec())

	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected child failure to propagate")
	}
	got := stub.recordedStatuses()
	if len(got) != 2 || got[0] != workflows.StatusRunning || got[1] != workflows.StatusFailed {
		t.Errorf("status sequence = %v, want [running failed]", got)
	}
}

func TestDispatchWorkflowRecordJobFailure(t *testing.T) {
	stub := &stubRecorder{
		candidates: oneCandidate(),
		recordErr: temporal.NewNonRetryableApplicationError(
			"insert exploded", "DBDown", nil),
	}
	env := newDispatchEnv(t, stub)

	env.ExecuteWorkflow(workflows.DispatchWorkflow, baseSpec())

	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected RecordJob failure to fail the workflow")
	}
	if got := stub.recordedStatuses(); len(got) != 0 {
		t.Errorf("no status writes expected after RecordJob failure, got %v", got)
	}
}

func TestDispatchWorkflowBatchedSuccess(t *testing.T) {
	stub := &stubRecorder{candidates: oneCandidate()}
	env := newDispatchEnv(t, stub)

	spec := baseSpec()
	spec.BatchSize = 3
	spec.BatchMaxAge = time.Minute

	// The reply signal name embeds the SideEffect-generated job UUID,
	// which we only know after RecordJob runs. A delayed callback closure
	// reads the captured value at fire time (activities complete at
	// virtual t=0, so 30s in is plenty late).
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalBatchResult+"-"+stub.capturedJobID(), workflows.BatchSlotResult{
			BatchID:       "batch-gpu-large-3-run1-0",
			ChildWorkflow: "batch-gpu-large-3-run1-0",
			ChildRunID:    "child-run-9",
			SlotIndex:     1,
		})
	}, 30*time.Second)

	env.ExecuteWorkflow(workflows.DispatchWorkflow, spec)

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var res workflows.DispatchResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if res.Status != workflows.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
	if res.ChildWorkflow != "batch-gpu-large-3-run1-0" || res.ChildRunID != "child-run-9" {
		t.Errorf("child refs = %q/%q", res.ChildWorkflow, res.ChildRunID)
	}
	if stub.ensureIn == nil {
		t.Fatal("EnsureBatchCoordinator never called")
	}
	if stub.ensureIn.Queue != "gpu-large" || stub.ensureIn.BatchSize != 3 {
		t.Errorf("ensure input = %+v", stub.ensureIn)
	}
	if stub.ensureIn.Envelope.JobID != stub.capturedJobID() {
		t.Errorf("envelope JobID = %q, want %q", stub.ensureIn.Envelope.JobID, stub.capturedJobID())
	}
	got := stub.recordedStatuses()
	if len(got) != 2 || got[0] != workflows.StatusRunning || got[1] != workflows.StatusSucceeded {
		t.Errorf("status sequence = %v, want [running succeeded]", got)
	}
}

func TestDispatchWorkflowBatchedSlotError(t *testing.T) {
	stub := &stubRecorder{candidates: oneCandidate()}
	env := newDispatchEnv(t, stub)

	spec := baseSpec()
	spec.BatchSize = 2
	spec.BatchMaxAge = time.Minute

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalBatchResult+"-"+stub.capturedJobID(), workflows.BatchSlotResult{
			BatchID: "b-1",
			Error:   "batched child exploded",
		})
	}, 30*time.Second)

	env.ExecuteWorkflow(workflows.DispatchWorkflow, spec)

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected ApplicationError, got %T: %v", err, err)
	}
	if appErr.Type() != "BatchSlotFailed" {
		t.Errorf("error type = %q, want BatchSlotFailed", appErr.Type())
	}
	got := stub.recordedStatuses()
	if len(got) != 2 || got[1] != workflows.StatusFailed {
		t.Errorf("status sequence = %v, want [... failed]", got)
	}
}

func TestDispatchWorkflowBatchedReplyTimeout(t *testing.T) {
	stub := &stubRecorder{candidates: oneCandidate()}
	env := newDispatchEnv(t, stub)

	spec := baseSpec()
	spec.BatchSize = 2
	spec.BatchMaxAge = time.Minute // reply window = max(4m, 10m floor) = 10m

	// No reply signal ever arrives; the mock clock auto-advances to the
	// timer.
	env.ExecuteWorkflow(workflows.DispatchWorkflow, spec)

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected ApplicationError, got %T: %v", err, err)
	}
	if appErr.Type() != "BatchReplyTimeout" {
		t.Errorf("error type = %q, want BatchReplyTimeout", appErr.Type())
	}
	if !appErr.NonRetryable() {
		t.Error("BatchReplyTimeout must be non-retryable")
	}
	got := stub.recordedStatuses()
	if len(got) != 2 || got[0] != workflows.StatusRunning || got[1] != workflows.StatusFailed {
		t.Errorf("status sequence = %v, want [running failed]", got)
	}
}

func TestDispatchWorkflowEnsureCoordinatorFailure(t *testing.T) {
	stub := &stubRecorder{
		candidates: oneCandidate(),
		ensureErr: temporal.NewNonRetryableApplicationError(
			"temporal client not configured", "NoClient", nil),
	}
	env := newDispatchEnv(t, stub)

	spec := baseSpec()
	spec.BatchSize = 2

	env.ExecuteWorkflow(workflows.DispatchWorkflow, spec)

	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected error from EnsureBatchCoordinator failure")
	}
	got := stub.recordedStatuses()
	if len(got) != 1 || got[0] != workflows.StatusFailed {
		t.Errorf("status sequence = %v, want [failed]", got)
	}
}

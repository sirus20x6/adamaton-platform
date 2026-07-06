package workflows_test

// Tests for BatchCoordinator via the Temporal test suite. The coordinator
// is an intentionally never-returning workflow, so each test drives it
// with delayed signal callbacks, lets the mock clock run, and asserts on
// (a) the "BatchedJob" child executions it fired and (b) the reply
// signals it sent to origin workflows. ExecuteWorkflow ends via the test
// environment's workflow timeout — that's expected, and the tests assert
// on captured state rather than a workflow return value.

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"
	sdkworkflow "go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

// batchChildCapture records every "BatchedJob" child execution.
type batchChildCapture struct {
	mu      sync.Mutex
	batches [][]workflows.JobSpec
	err     error // returned by the child when set
}

func (b *batchChildCapture) child(ctx sdkworkflow.Context, specs []workflows.JobSpec) (string, error) {
	b.mu.Lock()
	b.batches = append(b.batches, specs)
	b.mu.Unlock()
	if b.err != nil {
		return "", b.err
	}
	return "batched-ok", nil
}

func (b *batchChildCapture) fired() [][]workflows.JobSpec {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]workflows.JobSpec, len(b.batches))
	copy(out, b.batches)
	return out
}

// sentReply is one captured SignalExternalWorkflow call.
type sentReply struct {
	workflowID string
	signalName string
	slot       workflows.BatchSlotResult
}

func envelope(jobID, origin, kind string) workflows.BatchEnvelope {
	return workflows.BatchEnvelope{
		Spec: workflows.JobSpec{
			Kind:         kind,
			Payload:      json.RawMessage(`{}`),
			Requirements: workflows.Requirements{QueueClass: "gpu-large"},
			BatchSize:    2,
		},
		OriginWorkflowID: origin,
		JobID:            jobID,
	}
}

func TestBatchCoordinatorFullBatchFires(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(workflows.BatchCoordinator,
		sdkworkflow.RegisterOptions{Name: workflows.WorkflowBatchCoordinator})

	capture := &batchChildCapture{}
	env.RegisterWorkflowWithOptions(capture.child,
		sdkworkflow.RegisterOptions{Name: "BatchedJob"})

	var mu sync.Mutex
	var replies []sentReply
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			r := sentReply{
				workflowID: args.String(1),
				signalName: args.String(3),
			}
			if slot, ok := args.Get(4).(workflows.BatchSlotResult); ok {
				r.slot = slot
			}
			replies = append(replies, r)
		}).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalJob, envelope("job-1", "origin-1", "KindA"))
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalJob, envelope("job-2", "origin-2", "KindB"))
	}, 2*time.Second)

	env.ExecuteWorkflow(workflows.BatchCoordinator, workflows.BatchCoordinatorInput{
		Queue:       "gpu-large",
		BatchSize:   2,
		BatchMaxAge: time.Minute,
	})

	batches := capture.fired()
	if len(batches) != 1 {
		t.Fatalf("batches fired = %d, want 1", len(batches))
	}
	if len(batches[0]) != 2 {
		t.Fatalf("batch size = %d, want 2", len(batches[0]))
	}
	if batches[0][0].Kind != "KindA" || batches[0][1].Kind != "KindB" {
		t.Errorf("batch specs = %q,%q — order not preserved", batches[0][0].Kind, batches[0][1].Kind)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 2 {
		t.Fatalf("replies sent = %d, want 2", len(replies))
	}
	for i, want := range []struct{ origin, job string }{
		{"origin-1", "job-1"},
		{"origin-2", "job-2"},
	} {
		r := replies[i]
		if r.workflowID != want.origin {
			t.Errorf("reply %d workflowID = %q, want %q", i, r.workflowID, want.origin)
		}
		wantSig := workflows.SignalBatchResult + "-" + want.job
		if r.signalName != wantSig {
			t.Errorf("reply %d signal = %q, want %q", i, r.signalName, wantSig)
		}
		if r.slot.SlotIndex != i {
			t.Errorf("reply %d slot index = %d, want %d", i, r.slot.SlotIndex, i)
		}
		if r.slot.Error != "" {
			t.Errorf("reply %d unexpected error %q", i, r.slot.Error)
		}
		if r.slot.BatchID == "" || r.slot.ChildWorkflow != r.slot.BatchID {
			t.Errorf("reply %d batch/child ids = %q/%q", i, r.slot.BatchID, r.slot.ChildWorkflow)
		}
		// Batch IDs embed the coordinator workflow ID + run-id fragment +
		// index, ending in "-0" for the first batch of this run.
		if !strings.HasSuffix(r.slot.BatchID, "-0") {
			t.Errorf("reply %d batch id %q missing index suffix", i, r.slot.BatchID)
		}
	}

	// Progress query reflects the fired batch and the drained buffer.
	val, err := env.QueryWorkflow(workflows.QueryProgress)
	if err != nil {
		t.Fatalf("query progress: %v", err)
	}
	var prog workflows.BatchProgress
	if err := val.Get(&prog); err != nil {
		t.Fatalf("decode progress: %v", err)
	}
	if prog.BatchesFired != 1 {
		t.Errorf("BatchesFired = %d, want 1", prog.BatchesFired)
	}
	if prog.Buffered != 0 {
		t.Errorf("Buffered = %d, want 0", prog.Buffered)
	}
	if prog.Queue != "gpu-large" || prog.BatchSize != 2 {
		t.Errorf("progress identity = %q/%d", prog.Queue, prog.BatchSize)
	}
}

func TestBatchCoordinatorMaxAgeFiresPartialBatch(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(workflows.BatchCoordinator,
		sdkworkflow.RegisterOptions{Name: workflows.WorkflowBatchCoordinator})

	capture := &batchChildCapture{}
	env.RegisterWorkflowWithOptions(capture.child,
		sdkworkflow.RegisterOptions{Name: "BatchedJob"})

	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil)

	// One envelope into a size-3 batch: only the staleness timer can
	// fire it.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalJob, envelope("job-solo", "origin-solo", "KindSolo"))
	}, time.Second)

	env.ExecuteWorkflow(workflows.BatchCoordinator, workflows.BatchCoordinatorInput{
		Queue:       "cpu-small",
		BatchSize:   3,
		BatchMaxAge: 30 * time.Second,
	})

	batches := capture.fired()
	if len(batches) != 1 {
		t.Fatalf("batches fired = %d, want 1 (timer-driven partial)", len(batches))
	}
	if len(batches[0]) != 1 || batches[0][0].Kind != "KindSolo" {
		t.Errorf("partial batch = %+v", batches[0])
	}
}

func TestBatchCoordinatorChildFailureReportedToOrigins(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(workflows.BatchCoordinator,
		sdkworkflow.RegisterOptions{Name: workflows.WorkflowBatchCoordinator})

	capture := &batchChildCapture{err: errors.New("batched job exploded")}
	env.RegisterWorkflowWithOptions(capture.child,
		sdkworkflow.RegisterOptions{Name: "BatchedJob"})

	var mu sync.Mutex
	var slots []workflows.BatchSlotResult
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Run(func(args mock.Arguments) {
			mu.Lock()
			defer mu.Unlock()
			if slot, ok := args.Get(4).(workflows.BatchSlotResult); ok {
				slots = append(slots, slot)
			}
		}).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalJob, envelope("job-a", "origin-a", "KindA"))
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalJob, envelope("job-b", "origin-b", "KindB"))
	}, 2*time.Second)

	env.ExecuteWorkflow(workflows.BatchCoordinator, workflows.BatchCoordinatorInput{
		Queue:       "gpu-large",
		BatchSize:   2,
		BatchMaxAge: time.Minute,
	})

	mu.Lock()
	defer mu.Unlock()
	if len(slots) != 2 {
		t.Fatalf("replies sent = %d, want 2 (failures still fan out)", len(slots))
	}
	for i, s := range slots {
		if s.Error == "" || !strings.Contains(s.Error, "batched job exploded") {
			t.Errorf("slot %d error = %q, want the child failure", i, s.Error)
		}
	}
}

func TestBatchCoordinatorProgressQueryWhileBuffering(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(workflows.BatchCoordinator,
		sdkworkflow.RegisterOptions{Name: workflows.WorkflowBatchCoordinator})
	env.RegisterWorkflowWithOptions((&batchChildCapture{}).child,
		sdkworkflow.RegisterOptions{Name: "BatchedJob"})
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil)

	// One envelope into a size-2 batch with a long max age: it stays
	// buffered long enough for the delayed query to observe it.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.SignalJob, envelope("job-q", "origin-q", "KindQ"))
	}, time.Second)

	var buffered int
	queryErrCh := make(chan error, 1)
	env.RegisterDelayedCallback(func() {
		val, err := env.QueryWorkflow(workflows.QueryProgress)
		if err != nil {
			queryErrCh <- err
			return
		}
		var prog workflows.BatchProgress
		if err := val.Get(&prog); err != nil {
			queryErrCh <- err
			return
		}
		buffered = prog.Buffered
		queryErrCh <- nil
	}, 5*time.Second)

	env.ExecuteWorkflow(workflows.BatchCoordinator, workflows.BatchCoordinatorInput{
		Queue:       "gpu-large",
		BatchSize:   2,
		BatchMaxAge: time.Hour,
	})

	if err := <-queryErrCh; err != nil {
		t.Fatalf("mid-flight query failed: %v", err)
	}
	if buffered != 1 {
		t.Errorf("mid-flight Buffered = %d, want 1", buffered)
	}
}

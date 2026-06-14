package apiserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-delegator/delegator"
)

func TestIsTerminalTaskStatus(t *testing.T) {
	terminal := []delegator.TaskStatus{
		delegator.StatusCompleted, delegator.StatusFailed,
		delegator.StatusCancelled, delegator.StatusTimedOut,
	}
	for _, s := range terminal {
		if !isTerminalTaskStatus(s) {
			t.Errorf("expected %q to be terminal", s)
		}
	}
	for _, s := range []delegator.TaskStatus{delegator.StatusPending, delegator.StatusRunning, ""} {
		if isTerminalTaskStatus(s) {
			t.Errorf("expected %q to be non-terminal", s)
		}
	}
}

func TestDelegatorTaskStream_NoStore503(t *testing.T) {
	// With no delegator store wired the stream endpoint must 503 cleanly
	// (not panic) before touching any pool.
	s := &APIServer{logger: logrus.New()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/delegator/tasks/task-1/stream", nil)
	s.handleDelegatorTaskStream(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with nil store, got %d", rec.Code)
	}
}

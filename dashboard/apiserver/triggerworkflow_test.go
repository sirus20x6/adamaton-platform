// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
// /thearray/gogents/internal/apiserver/triggerworkflow_test.go
//
// Tests for the four bugs fixed in this audit pass:
//   - Bug 2: triggerWorkflow populates HeadSHA from a Gitea PR fetch.
//   - Bug 3: workflow ID is deterministic (same head SHA -> same ID) so
//     retries dedupe through Temporal's reuse policy.
//   - Bug 4: triggerWorkflow inflight semaphore returns 429 when full.
//   - Bug 5: getRun's UpdateRunStatus error is logged and the response
//     reflects the previously stored status rather than the unpersisted
//     transition.
package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamomaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamomaton-core/pgutil"
	"github.com/sirus20x6/adamomaton-core/types"
	"github.com/sirus20x6/adamomaton-evolve/workflow-builder/workflowstore"
)

// newTriggerTestStore returns a fresh paradedb-backed workflow store
// scoped to the test. Tests that don't have docker available should
// invoke this — it honours GOGENTS_SKIP_DOCKER_TESTS.
func newTriggerTestStore(t *testing.T) *workflowstore.Store {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	store, err := workflowstore.NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// fakeStarter is a hand-rolled stub satisfying temporalStarter. It captures
// the workflow ID the handler passed so tests can assert on it without
// dialing a real Temporal server.
type fakeStarter struct {
	mu        sync.Mutex
	calls     int
	lastID    string
	returnErr error
	// lastCtxErr records ctx.Err() observed at call time. Bug-1 regression
	// test asserts this is nil even when the inbound HTTP request context
	// was cancelled — the handler must NOT plumb r.Context() into the
	// scheduling RPC.
	lastCtxErr error
	// gate, when non-nil, blocks ExecuteWorkflow until released. Used by
	// the 429 test to keep one request "in flight" while a second arrives.
	gate <-chan struct{}
	// entered fires once per call, just before ExecuteWorkflow blocks on
	// gate. Tests use this to know the inflight slot is held.
	entered chan struct{}
}

func (f *fakeStarter) ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	f.mu.Lock()
	f.calls++
	f.lastID = opts.ID
	f.lastCtxErr = ctx.Err()
	gate := f.gate
	entered := f.entered
	err := f.returnErr
	f.mu.Unlock()

	if entered != nil {
		// Non-blocking send so a second concurrent call doesn't deadlock
		// when the channel is unbuffered and the test only reads once.
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	return &fakeWorkflowRun{}, nil
}

// fakeWorkflowRun is the minimal client.WorkflowRun for tests — handlers
// only call GetID/GetRunID for response logging.
type fakeWorkflowRun struct{}

func (f *fakeWorkflowRun) GetID() string                              { return "fake-id" }
func (f *fakeWorkflowRun) GetRunID() string                           { return "fake-run-id" }
func (f *fakeWorkflowRun) Get(_ context.Context, _ interface{}) error { return nil }
func (f *fakeWorkflowRun) GetWithOptions(_ context.Context, _ interface{}, _ client.WorkflowRunGetOptions) error {
	return nil
}

// fakePRFetcher returns a fixed head SHA so the test can assert the workflow
// ID's deterministic suffix without a real Gitea server.
type fakePRFetcher struct {
	headSHA string
	err     error
}

func (f *fakePRFetcher) GetPullRequest(_ context.Context, _, _ string, _ int64) (*gitea.PullRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &gitea.PullRequest{Head: gitea.PRBranchInfo{Sha: f.headSHA}}, nil
}

// newTriggerTestServer wires an APIServer with the provided starter / fetcher
// and an inflight semaphore of the given size. The router is wired with only
// the trigger route so tests don't need a full setupRoutes.
func newTriggerTestServer(t *testing.T, starter temporalStarter, fetcher prFetcher, semSize int) *APIServer {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	cfg := &types.Config{}
	cfg.Temporal.TaskQueue = "test-queue"
	s := &APIServer{
		logger:      logger,
		config:      cfg,
		router:      mux.NewRouter(),
		starter:     starter,
		prFetcher:   fetcher,
		inflightSem: make(chan struct{}, semSize),
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/workflow/trigger", s.triggerWorkflow).Methods("POST")
	return s
}

// TestTriggerWorkflow_DeterministicID confirms the workflow ID embeds the
// 8-char prefix of the fetched head SHA so retries for the same PR+SHA
// produce an identical ID — that's what lets Temporal's
// ALLOW_DUPLICATE_FAILED_ONLY reuse policy dedupe them.
func TestTriggerWorkflow_DeterministicID(t *testing.T) {
	starter := &fakeStarter{}
	fetcher := &fakePRFetcher{headSHA: "deadbeef0123456789abcdef"}
	s := newTriggerTestServer(t, starter, fetcher, 4)

	body := []byte(`{"prNumber":42,"repoOwner":"acme","repoName":"widgets"}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "expected 200, body=%s", rr.Body.String())
	require.Equal(t, "pr-review-acme-widgets-42-deadbeef", starter.lastID)

	// The same request again must produce exactly the same ID — that's the
	// whole point of dropping time.Now() from the suffix.
	starter.mu.Lock()
	starter.lastID = ""
	starter.mu.Unlock()
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
	s.Router().ServeHTTP(rr2, req2)
	require.Equal(t, "pr-review-acme-widgets-42-deadbeef", starter.lastID)
}

// TestTriggerWorkflow_NoGitea_UUIDFallback confirms that when no PR fetcher
// is available (GitHub-only deployments) the workflow ID still gets a
// unique suffix so two POSTs <1s apart don't collide on the
// time.Now()-based suffix the bug-3 fix removed.
func TestTriggerWorkflow_NoGitea_UUIDFallback(t *testing.T) {
	starter := &fakeStarter{}
	s := newTriggerTestServer(t, starter, nil, 4)

	body := []byte(`{"prNumber":7,"repoOwner":"acme","repoName":"widgets"}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	id1 := starter.lastID

	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
	s.Router().ServeHTTP(rr2, req2)
	id2 := starter.lastID

	require.True(t, strings.HasPrefix(id1, "pr-review-acme-widgets-7-"))
	require.True(t, strings.HasPrefix(id2, "pr-review-acme-widgets-7-"))
	require.NotEqual(t, id1, id2, "without head SHA, two calls must produce distinct IDs (UUID fallback)")
}

// TestTriggerWorkflow_StartsDespiteCancelledRequestCtx is the Bug-1
// regression: the handler must NOT plumb r.Context() into ExecuteWorkflow.
// Workflow start is fire-and-forget; if a client hangs up (or Gitea's 5s
// delivery timeout fires) before the scheduling RPC returns, the workflow
// still has to be scheduled. We assert this by sending a request whose
// context is already cancelled and checking that:
//   1. ExecuteWorkflow was still called (calls == 1).
//   2. The context observed inside ExecuteWorkflow was NOT cancelled
//      (lastCtxErr == nil) — proving we used a fresh background context.
//
// Note: the PR fetch DOES use r.Context() — we don't test that here because
// the cancelled request can short-circuit the fetcher path. We use the
// no-Gitea-fetcher branch (nil prFetcher) so the cancelled context never
// reaches the fetch.
func TestTriggerWorkflow_StartsDespiteCancelledRequestCtx(t *testing.T) {
	starter := &fakeStarter{}
	s := newTriggerTestServer(t, starter, nil, 4)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the request arrives "already dead"

	body := []byte(`{"prNumber":42,"repoOwner":"acme","repoName":"widgets"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body)).
		WithContext(cancelledCtx)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "workflow start must succeed even when r.Context() is dead; body=%s", rr.Body.String())
	starter.mu.Lock()
	defer starter.mu.Unlock()
	require.Equal(t, 1, starter.calls, "ExecuteWorkflow must still be invoked")
	require.NoError(t, starter.lastCtxErr, "context observed inside ExecuteWorkflow must be a fresh background — not the cancelled request context")
}

// TestRunDefinition_StartsDespiteCancelledRequestCtx mirrors the above for
// the dynamic-run path in workflow_endpoints.go. Same Bug-1 contract.
func TestRunDefinition_StartsDespiteCancelledRequestCtx(t *testing.T) {
	store := newTriggerTestStore(t)

	def, err := store.CreateDefinition("d", "", `{"nodes":[{"id":"n","type":"activity","activityName":"NoopActivity"}]}`)
	require.NoError(t, err)

	starter := &fakeStarter{}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	cfg := &types.Config{}
	cfg.Temporal.TaskQueue = "test-queue"
	s := &APIServer{
		logger:        logger,
		config:        cfg,
		router:        mux.NewRouter(),
		workflowStore: store,
		starter:       starter,
		inflightSem:   make(chan struct{}, 4),
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.setupWorkflowBuilderRoutes(api)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions/"+def.ID+"/run", nil).
		WithContext(cancelledCtx)
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "dynamic-run must succeed even when r.Context() is dead; body=%s", rr.Body.String())
	starter.mu.Lock()
	defer starter.mu.Unlock()
	require.Equal(t, 1, starter.calls)
	require.NoError(t, starter.lastCtxErr, "ExecuteWorkflow must NOT receive r.Context() — workflow start is fire-and-forget")
}

// TestTriggerWorkflow_GiteaFetchFailure confirms the handler returns 502
// when the PR lookup fails, and does NOT call ExecuteWorkflow — we don't
// want to schedule a workflow with an unknown head SHA when Gitea is
// configured but unreachable.
func TestTriggerWorkflow_GiteaFetchFailure(t *testing.T) {
	starter := &fakeStarter{}
	fetcher := &fakePRFetcher{err: errors.New("connection refused")}
	s := newTriggerTestServer(t, starter, fetcher, 4)

	body := []byte(`{"prNumber":42,"repoOwner":"acme","repoName":"widgets"}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadGateway, rr.Code)
	starter.mu.Lock()
	require.Equal(t, 0, starter.calls, "ExecuteWorkflow must not be called when PR fetch fails")
	starter.mu.Unlock()
}

// TestTriggerWorkflow_SemaphoreFull confirms that when the inflight
// semaphore is saturated, additional requests get 429 and the rejected
// request never reaches ExecuteWorkflow.
//
// We use a gated fakeStarter so request #1 stays "in flight" while we fire
// request #2 — no sleeps, no flakes. The `entered` channel signals when
// request #1 is past the semaphore acquire and inside ExecuteWorkflow.
func TestTriggerWorkflow_SemaphoreFull(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	starter := &fakeStarter{gate: gate, entered: entered}
	fetcher := &fakePRFetcher{headSHA: "abc12345"}
	s := newTriggerTestServer(t, starter, fetcher, 1) // sem size 1

	body := []byte(`{"prNumber":1,"repoOwner":"a","repoName":"b"}`)

	// Drive request #1 in a goroutine; it blocks inside ExecuteWorkflow.
	doneFirst := make(chan struct{})
	go func() {
		defer close(doneFirst)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
		rr := httptest.NewRecorder()
		s.Router().ServeHTTP(rr, req)
	}()

	// Wait until request #1 has acquired the slot AND entered the fake
	// ExecuteWorkflow. Bounded by Goroutine scheduler latency; we cap with
	// a generous timeout to avoid flakes on heavily loaded CI workers.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach ExecuteWorkflow within 5s")
	}

	// Now fire request #2. The semaphore should be full, so the handler
	// returns 429 immediately without touching the body or fetcher.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
	s.Router().ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusTooManyRequests, rr2.Code, "expected 429 when sem full, body=%s", rr2.Body.String())

	starter.mu.Lock()
	require.Equal(t, 1, starter.calls, "rejected request must not reach ExecuteWorkflow")
	starter.mu.Unlock()

	// Release the gate so the first request finishes; test cleans up.
	close(gate)
	<-doneFirst
}

// TestTriggerWorkflow_SemaphoreReleasedAfterRequest confirms that finishing
// a request frees the slot so subsequent requests succeed. Without this
// guarantee the semaphore would slowly leak and eventually wedge the API.
func TestTriggerWorkflow_SemaphoreReleasedAfterRequest(t *testing.T) {
	starter := &fakeStarter{}
	fetcher := &fakePRFetcher{headSHA: "deadbeef"}
	s := newTriggerTestServer(t, starter, fetcher, 1)

	body := []byte(`{"prNumber":1,"repoOwner":"a","repoName":"b"}`)
	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader(body))
		s.Router().ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, "iter %d body=%s", i, rr.Body.String())
	}
	starter.mu.Lock()
	require.Equal(t, 3, starter.calls)
	starter.mu.Unlock()
}

// TestTriggerWorkflow_BadInput covers the input-validation short-circuits.
func TestTriggerWorkflow_BadInput(t *testing.T) {
	starter := &fakeStarter{}
	fetcher := &fakePRFetcher{headSHA: "deadbeef"}
	s := newTriggerTestServer(t, starter, fetcher, 4)

	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "malformed json", body: `not json`, want: http.StatusBadRequest},
		{name: "missing pr", body: `{"repoOwner":"a","repoName":"b"}`, want: http.StatusBadRequest},
		{name: "missing owner", body: `{"prNumber":1,"repoName":"b"}`, want: http.StatusBadRequest},
		{name: "missing repo", body: `{"prNumber":1,"repoOwner":"a"}`, want: http.StatusBadRequest},
		{name: "negative pr", body: `{"prNumber":-1,"repoOwner":"a","repoName":"b"}`, want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/workflow/trigger", bytes.NewReader([]byte(tc.body)))
			s.Router().ServeHTTP(rr, req)
			require.Equal(t, tc.want, rr.Code)
		})
	}
}

// closingDescriber is a temporalDescriber that returns a synthetic terminal
// status AND closes the workflow store as a side effect. That makes the
// ordering deterministic: GetRun (before the call) succeeds, the
// Describe call lands, and UpdateRunStatus (after the call) fails with
// ErrClosed — which is the exact branch bug-5 fixes.
type closingDescriber struct {
	store *workflowstore.Store
}

func (d *closingDescriber) DescribeWorkflowExecution(_ context.Context, _, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	_ = d.store.Close()
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		},
	}, nil
}

// TestGetRun_UpdateStatusFailure confirms bug-5: when UpdateRunStatus
// fails (e.g. the store has been closed under us), the handler logs a
// warning carrying the run_id AND returns the *previously stored* status
// rather than the newly-derived one. Lying to the caller — saying
// "completed" while the DB still says "running" — is worse than a stale
// read; the next poll will re-attempt the transition.
func TestGetRun_UpdateStatusFailure(t *testing.T) {
	store := newTriggerTestStore(t)

	def, err := store.CreateDefinition("d", "", `{"nodes":[{"id":"n","type":"activity","activityName":"NoopActivity"}]}`)
	require.NoError(t, err)
	run, err := store.CreateRun(def.ID, "wf-id", "run-id", "{}")
	require.NoError(t, err)
	require.Equal(t, "running", run.Status)

	logger, hook := newTestLogger()

	cfg := &types.Config{}
	s := &APIServer{
		logger:        logger,
		config:        cfg,
		router:        mux.NewRouter(),
		workflowStore: store,
		describer:     &closingDescriber{store: store},
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.setupWorkflowBuilderRoutes(api)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/runs/"+run.ID, nil)
	s.Router().ServeHTTP(rr, req)

	// 200 OK with run.Status still "running" — bug-5 fix: don't lie when
	// persistence failed.
	require.Equal(t, http.StatusOK, rr.Code, "body=%s", rr.Body.String())
	var resp APIResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Success)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok, "expected map response data, got %T", resp.Data)
	require.Equal(t, "running", data["status"], "status must remain 'running' when UpdateRunStatus fails")

	// The warning log must be present and reference the run_id.
	var found bool
	for _, e := range hook.AllEntries() {
		if e.Level != logrus.WarnLevel {
			continue
		}
		if !strings.Contains(e.Message, "failed to persist run status") {
			continue
		}
		if got, ok := e.Data["run_id"]; ok && got == run.ID {
			found = true
			break
		}
	}
	require.True(t, found, "expected a warn-level log with run_id=%s; entries=%+v", run.ID, hook.AllEntries())
}

// newTestLogger returns a logger with an in-memory hook so tests can
// assert on emitted records.
func newTestLogger() (*logrus.Logger, *test.Hook) {
	l := logrus.New()
	l.SetOutput(io.Discard)
	hook := test.NewLocal(l)
	return l, hook
}
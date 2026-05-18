package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/workflow"

	"github.com/sirus20x6/adamaton-core/types"
)

// fakeScheduler / fakeScheduleHandle / fakeScheduleIterator stub out the
// Temporal SDK surface the handlers reach for. We capture call args so tests
// can assert on them and let each test inject error returns.

type fakeScheduler struct {
	mu sync.Mutex

	// Inputs we capture.
	createCalls   []client.ScheduleOptions
	listCalls     int
	getHandleArgs []string

	// Optional canned returns.
	createErr error
	listErr   error
	entries   []*client.ScheduleListEntry

	// handle is reused across GetHandle calls so a single test can pre-load
	// canned describe / update / delete / etc. behavior.
	handle *fakeScheduleHandle
}

func (f *fakeScheduler) Create(_ context.Context, opts client.ScheduleOptions) (client.ScheduleHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, opts)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.handle == nil {
		f.handle = &fakeScheduleHandle{id: opts.ID}
	} else if f.handle.id == "" {
		f.handle.id = opts.ID
	}
	return f.handle, nil
}

func (f *fakeScheduler) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getHandleArgs = append(f.getHandleArgs, id)
	if f.handle == nil {
		f.handle = &fakeScheduleHandle{id: id}
	} else {
		f.handle.id = id
	}
	return f.handle
}

func (f *fakeScheduler) List(_ context.Context, _ client.ScheduleListOptions) (client.ScheduleListIterator, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &fakeScheduleIterator{entries: append([]*client.ScheduleListEntry{}, f.entries...)}, nil
}

// fakeScheduleHandle implements client.ScheduleHandle. Every mutating method
// records the call and optionally returns a canned error.
type fakeScheduleHandle struct {
	id string

	describe    *client.ScheduleDescription
	describeErr error

	updateCalls int
	updateErr   error

	deleteCalls int
	deleteErr   error

	pauseCalls   int
	pauseErr     error
	unpauseCalls int
	unpauseErr   error
	triggerCalls int
	triggerErr   error
}

func (h *fakeScheduleHandle) GetID() string { return h.id }
func (h *fakeScheduleHandle) Describe(_ context.Context) (*client.ScheduleDescription, error) {
	if h.describeErr != nil {
		return nil, h.describeErr
	}
	return h.describe, nil
}
func (h *fakeScheduleHandle) Backfill(_ context.Context, _ client.ScheduleBackfillOptions) error {
	return nil
}
func (h *fakeScheduleHandle) Update(_ context.Context, opts client.ScheduleUpdateOptions) error {
	h.updateCalls++
	if h.updateErr != nil {
		return h.updateErr
	}
	// Exercise the DoUpdate callback so the handler's mutation logic runs;
	// give it the current describe shape as input.
	if opts.DoUpdate != nil {
		in := client.ScheduleUpdateInput{}
		if h.describe != nil {
			in.Description = *h.describe
		}
		_, _ = opts.DoUpdate(in)
	}
	return nil
}
func (h *fakeScheduleHandle) Delete(_ context.Context) error {
	h.deleteCalls++
	return h.deleteErr
}
func (h *fakeScheduleHandle) Pause(_ context.Context, _ client.SchedulePauseOptions) error {
	h.pauseCalls++
	return h.pauseErr
}
func (h *fakeScheduleHandle) Unpause(_ context.Context, _ client.ScheduleUnpauseOptions) error {
	h.unpauseCalls++
	return h.unpauseErr
}
func (h *fakeScheduleHandle) Trigger(_ context.Context, _ client.ScheduleTriggerOptions) error {
	h.triggerCalls++
	return h.triggerErr
}

type fakeScheduleIterator struct {
	entries []*client.ScheduleListEntry
	i       int
}

func (it *fakeScheduleIterator) HasNext() bool { return it.i < len(it.entries) }
func (it *fakeScheduleIterator) Next() (*client.ScheduleListEntry, error) {
	if it.i >= len(it.entries) {
		return nil, io.EOF
	}
	e := it.entries[it.i]
	it.i++
	return e, nil
}

// newScheduleTestServer wires up an APIServer with just the schedule routes
// registered and the provided scheduler injected. Inflight semaphore size 4
// is enough for every non-concurrency test; tests that exercise the limit
// pass their own.
func newScheduleTestServer(t *testing.T, sched temporalScheduler, semSize int) *APIServer {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	s := &APIServer{
		logger:      logger,
		config:      &types.Config{},
		router:      mux.NewRouter(),
		scheduler:   sched,
		inflightSem: make(chan struct{}, semSize),
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.setupScheduleRoutes(api)
	return s
}

func doReq(t *testing.T, s *APIServer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	s.Router().ServeHTTP(rr, req)
	return rr
}

func decodeResponse(t *testing.T, rr *httptest.ResponseRecorder) APIResponse {
	t.Helper()
	var resp APIResponse
	if rr.Body.Len() == 0 {
		return resp
	}
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&resp))
	return resp
}

// --- list ---

func TestListSchedules_EmptyReturnsArray(t *testing.T) {
	sched := &fakeScheduler{}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodGet, "/api/v1/schedules", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	resp := decodeResponse(t, rr)
	require.True(t, resp.Success)
	data := resp.Data.(map[string]interface{})
	require.IsType(t, []interface{}{}, data["schedules"])
	require.Empty(t, data["schedules"])
}

func TestListSchedules_SummarisesEntries(t *testing.T) {
	t0 := time.Now().Add(time.Minute)
	t1 := time.Now().Add(-time.Minute)
	sched := &fakeScheduler{
		entries: []*client.ScheduleListEntry{
			{
				ID:              "daily-summary",
				Note:            "hello",
				Paused:          false,
				WorkflowType:    workflow.Type{Name: "DelegationWorkflow"},
				Spec:            &client.ScheduleSpec{CronExpressions: []string{"0 9 * * *"}},
				NextActionTimes: []time.Time{t0},
				RecentActions: []client.ScheduleActionResult{
					{ActualTime: t1, StartWorkflowResult: &client.ScheduleWorkflowExecution{WorkflowID: "daily-summary-run-1"}},
				},
			},
		},
	}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodGet, "/api/v1/schedules", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, "\"id\":\"daily-summary\"")
	require.Contains(t, body, "\"kind\":\"delegation\"")
	require.Contains(t, body, "\"cron\":\"0 9 * * *\"")
}

func TestListSchedules_TemporalListErrorBubbles(t *testing.T) {
	sched := &fakeScheduler{listErr: serviceerror.NewUnavailable("temporal down")}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodGet, "/api/v1/schedules", nil)
	require.Equal(t, http.StatusBadGateway, rr.Code)
	resp := decodeResponse(t, rr)
	require.False(t, resp.Success)
	require.Contains(t, resp.Error, "list schedules")
}

// --- get ---

func TestGetSchedule_Describes(t *testing.T) {
	t0 := time.Now()
	desc := &client.ScheduleDescription{
		Schedule: client.Schedule{
			Spec:   &client.ScheduleSpec{CronExpressions: []string{"57 8 * * *"}},
			State:  &client.ScheduleState{Paused: true, Note: "n"},
			Action: &client.ScheduleWorkflowAction{Workflow: "DelegationWorkflow", TaskQueue: "delegator-recurring"},
		},
		Info: client.ScheduleInfo{
			NextActionTimes: []time.Time{t0},
		},
	}
	sched := &fakeScheduler{handle: &fakeScheduleHandle{describe: desc}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodGet, "/api/v1/schedules/my-id", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, "\"id\":\"my-id\"")
	require.Contains(t, body, "\"cron\":\"57 8 * * *\"")
	require.Contains(t, body, "\"paused\":true")
}

func TestGetSchedule_NotFound(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{describeErr: serviceerror.NewNotFound("nope")}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodGet, "/api/v1/schedules/missing", nil)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// --- create ---

func TestCreateSchedule_DelegationHappyPath(t *testing.T) {
	desc := &client.ScheduleDescription{
		Schedule: client.Schedule{
			Spec:   &client.ScheduleSpec{CronExpressions: []string{"0 9 * * *"}},
			State:  &client.ScheduleState{},
			Action: &client.ScheduleWorkflowAction{Workflow: "DelegationWorkflow", TaskQueue: "delegator-recurring"},
		},
	}
	sched := &fakeScheduler{handle: &fakeScheduleHandle{describe: desc}}
	s := newScheduleTestServer(t, sched, 4)

	req := CreateScheduleRequest{
		ID:   "morning-summary",
		Cron: "0 9 * * *",
		Note: "daily summary",
		Kind: ScheduleKindDelegation,
		Delegation: &DelegationSpec{
			Prompt:     "summarise yesterday's PRs",
			Difficulty: "medium",
			Priority:   "normal",
		},
	}
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules", req)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.Len(t, sched.createCalls, 1)

	opts := sched.createCalls[0]
	require.Equal(t, "morning-summary", opts.ID)
	require.Equal(t, []string{"0 9 * * *"}, opts.Spec.CronExpressions)
	act, ok := opts.Action.(*client.ScheduleWorkflowAction)
	require.True(t, ok)
	require.Equal(t, "DelegationWorkflow", act.Workflow)
	require.Equal(t, "delegator-recurring", act.TaskQueue)
	require.Equal(t, "delegation-morning-summary", act.ID)
	require.Len(t, act.Args, 1)
	input := act.Args[0].(map[string]any)
	require.Equal(t, "summarise yesterday's PRs", input["prompt"])
	require.Equal(t, "medium", input["difficulty"])
}

func TestCreateSchedule_GenericHappyPath(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{}}
	s := newScheduleTestServer(t, sched, 4)

	req := CreateScheduleRequest{
		ID:   "weekly-pr",
		Cron: "0 9 * * 1",
		Kind: ScheduleKindGeneric,
		Generic: &GenericSpec{
			Workflow:  "PRReviewWorkflow",
			TaskQueue: "platform",
			ArgsJSON:  `[{"prNumber":42,"repoOwner":"acme"}]`,
		},
	}
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules", req)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	require.Len(t, sched.createCalls, 1)

	act := sched.createCalls[0].Action.(*client.ScheduleWorkflowAction)
	require.Equal(t, "PRReviewWorkflow", act.Workflow)
	require.Equal(t, "platform", act.TaskQueue)
	require.Equal(t, "weekly-pr-run", act.ID)
	require.Len(t, act.Args, 1)
}

func TestCreateSchedule_DuplicateIDConflict(t *testing.T) {
	sched := &fakeScheduler{createErr: serviceerror.NewAlreadyExists("already")}
	s := newScheduleTestServer(t, sched, 4)
	req := CreateScheduleRequest{
		ID:         "x",
		Cron:       "0 9 * * *",
		Kind:       ScheduleKindDelegation,
		Delegation: &DelegationSpec{Prompt: "p"},
	}
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules", req)
	require.Equal(t, http.StatusConflict, rr.Code)
}

func TestCreateSchedule_BadInput(t *testing.T) {
	cases := []struct {
		name string
		body CreateScheduleRequest
	}{
		{"missing id", CreateScheduleRequest{Cron: "0 9 * * *", Kind: ScheduleKindDelegation, Delegation: &DelegationSpec{Prompt: "p"}}},
		{"missing cron", CreateScheduleRequest{ID: "x", Kind: ScheduleKindDelegation, Delegation: &DelegationSpec{Prompt: "p"}}},
		{"missing kind", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *"}},
		{"unknown kind", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: "nope"}},
		{"delegation no payload", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: ScheduleKindDelegation}},
		{"delegation no prompt", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: ScheduleKindDelegation, Delegation: &DelegationSpec{}}},
		{"generic no payload", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: ScheduleKindGeneric}},
		{"generic no workflow", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: ScheduleKindGeneric, Generic: &GenericSpec{TaskQueue: "q"}}},
		{"generic no queue", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: ScheduleKindGeneric, Generic: &GenericSpec{Workflow: "w"}}},
		{"generic bad args", CreateScheduleRequest{ID: "x", Cron: "0 9 * * *", Kind: ScheduleKindGeneric, Generic: &GenericSpec{Workflow: "w", TaskQueue: "q", ArgsJSON: "{not-an-array}"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := &fakeScheduler{}
			s := newScheduleTestServer(t, sched, 4)
			rr := doReq(t, s, http.MethodPost, "/api/v1/schedules", tc.body)
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			require.Empty(t, sched.createCalls, "no create call should reach temporal on bad input")
		})
	}
}

// --- update ---

func TestUpdateSchedule_HappyPath(t *testing.T) {
	desc := &client.ScheduleDescription{
		Schedule: client.Schedule{
			Spec:   &client.ScheduleSpec{CronExpressions: []string{"old"}},
			State:  &client.ScheduleState{},
			Action: &client.ScheduleWorkflowAction{Workflow: "DelegationWorkflow", TaskQueue: "delegator-recurring"},
		},
	}
	sched := &fakeScheduler{handle: &fakeScheduleHandle{describe: desc}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPut, "/api/v1/schedules/my-id", UpdateScheduleRequest{Cron: "0 12 * * *", Note: "noon"})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 1, sched.handle.updateCalls)
}

func TestUpdateSchedule_CronRequired(t *testing.T) {
	sched := &fakeScheduler{}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPut, "/api/v1/schedules/my-id", UpdateScheduleRequest{})
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestUpdateSchedule_NotFound(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{updateErr: serviceerror.NewNotFound("nope")}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPut, "/api/v1/schedules/missing", UpdateScheduleRequest{Cron: "0 9 * * *"})
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// --- delete ---

func TestDeleteSchedule_Success(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodDelete, "/api/v1/schedules/my-id", nil)
	require.Equal(t, http.StatusNoContent, rr.Code)
	require.Equal(t, 1, sched.handle.deleteCalls)
}

func TestDeleteSchedule_IdempotentOnNotFound(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{deleteErr: serviceerror.NewNotFound("nope")}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodDelete, "/api/v1/schedules/missing", nil)
	require.Equal(t, http.StatusNoContent, rr.Code, "delete should be idempotent: NotFound is a success")
}

// --- pause / unpause / trigger ---

func TestPauseSchedule_FlipsPaused(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules/my-id/pause", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 1, sched.handle.pauseCalls)
	require.Contains(t, rr.Body.String(), "\"paused\":true")
}

func TestUnpauseSchedule_FlipsPaused(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules/my-id/unpause", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, 1, sched.handle.unpauseCalls)
	require.Contains(t, rr.Body.String(), "\"paused\":false")
}

func TestTriggerSchedule_Accepted(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules/my-id/trigger", nil)
	require.Equal(t, http.StatusAccepted, rr.Code, rr.Body.String())
	require.Equal(t, 1, sched.handle.triggerCalls)
}

func TestTriggerSchedule_NotFound(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{triggerErr: serviceerror.NewNotFound("nope")}}
	s := newScheduleTestServer(t, sched, 4)
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules/missing/trigger", nil)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// --- inflight limit ---

func TestCreateSchedule_InflightLimit(t *testing.T) {
	sched := &fakeScheduler{handle: &fakeScheduleHandle{}}
	s := newScheduleTestServer(t, sched, 1)
	// Pre-fill the semaphore so the next request gets rejected.
	s.inflightSem <- struct{}{}
	rr := doReq(t, s, http.MethodPost, "/api/v1/schedules", CreateScheduleRequest{
		ID:         "x",
		Cron:       "0 9 * * *",
		Kind:       ScheduleKindDelegation,
		Delegation: &DelegationSpec{Prompt: "p"},
	})
	require.Equal(t, http.StatusTooManyRequests, rr.Code)
	require.Empty(t, sched.createCalls, "no create call should reach temporal when inflight-limited")
}

// --- temporalClient-nil ---

func TestSchedules_NoTemporalClient503(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	s := &APIServer{
		logger:      logger,
		config:      &types.Config{},
		router:      mux.NewRouter(),
		inflightSem: make(chan struct{}, 4),
		// no scheduler, no temporalClient -> pickScheduler() returns nil
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.setupScheduleRoutes(api)

	rr := doReq(t, s, http.MethodGet, "/api/v1/schedules", nil)
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

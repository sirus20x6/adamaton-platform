package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// setupScheduleRoutes registers the /schedules surface. Mirrors the other
// register* helpers; called from setupRoutes.
func (s *APIServer) setupScheduleRoutes(api *mux.Router) {
	api.HandleFunc("/schedules", s.listSchedules).Methods("GET")
	api.HandleFunc("/schedules", s.createSchedule).Methods("POST")
	api.HandleFunc("/schedules/{scheduleID}", s.getSchedule).Methods("GET")
	api.HandleFunc("/schedules/{scheduleID}", s.updateSchedule).Methods("PUT")
	api.HandleFunc("/schedules/{scheduleID}", s.deleteSchedule).Methods("DELETE")
	api.HandleFunc("/schedules/{scheduleID}/pause", s.pauseSchedule).Methods("POST")
	api.HandleFunc("/schedules/{scheduleID}/unpause", s.unpauseSchedule).Methods("POST")
	api.HandleFunc("/schedules/{scheduleID}/trigger", s.triggerSchedule).Methods("POST")
}

// ScheduleKind is the discriminator for how a CreateScheduleRequest payload
// is interpreted. Persisted only as the workflow name on the Temporal side —
// the server derives `kind` back out from the workflow name when listing.
type ScheduleKind string

const (
	ScheduleKindDelegation ScheduleKind = "delegation"
	ScheduleKindGeneric    ScheduleKind = "generic"

	// delegationWorkflowName is the workflow that delegator's recurring
	// schedules execute. Must stay in sync with the delegator package
	// (see github.com/sirus20x6/adamaton-delegator/delegator: NewScheduler
	// default).
	delegationWorkflowName = "DelegationWorkflow"

	// delegationTaskQueue is the task queue the recurring delegator worker
	// listens on. Must stay in sync with the delegator MCP boot.
	delegationTaskQueue = "delegator-recurring"

	// scheduleMutationTimeout caps how long a single Temporal RPC may take
	// before the handler gives up. Matches the workflow trigger handlers'
	// background-context timeout — a slow Temporal frontend should not pin
	// an inflight slot indefinitely.
	scheduleMutationTimeout = 30 * time.Second
)

// DelegationSpec carries the delegator-shaped fields for a create / update
// of `kind: "delegation"`. Field shape mirrors the delegator MCP tool
// arguments (delegator.ScheduleSpec) so the wire shape is the schedule
// shape — UI forms can pass values through unchanged.
type DelegationSpec struct {
	Prompt      string `json:"prompt"`
	Difficulty  string `json:"difficulty,omitempty"`
	Priority    string `json:"priority,omitempty"`
	AgentHint   string `json:"agent_hint,omitempty"`
	WorkingDir  string `json:"working_dir,omitempty"`
	Model       string `json:"model,omitempty"`
	TimeoutSecs int    `json:"timeout_secs,omitempty"`
}

// GenericSpec carries the raw-workflow fields for a create / update of
// `kind: "generic"`. ArgsJSON is the request-side string form; the handler
// JSON-decodes it to []any before passing to Temporal.
type GenericSpec struct {
	Workflow  string `json:"workflow"`
	TaskQueue string `json:"task_queue"`
	ArgsJSON  string `json:"args_json,omitempty"`
}

// CreateScheduleRequest is the POST /schedules body.
type CreateScheduleRequest struct {
	ID         string          `json:"id"`
	Cron       string          `json:"cron"`
	Note       string          `json:"note,omitempty"`
	Paused     bool            `json:"paused,omitempty"`
	Kind       ScheduleKind    `json:"kind"`
	Delegation *DelegationSpec `json:"delegation,omitempty"`
	Generic    *GenericSpec    `json:"generic,omitempty"`
}

// UpdateScheduleRequest is the PUT /schedules/{id} body. Only the cron +
// note are mutable in v1 — the workflow action is immutable, so kind
// changes require delete + recreate. Keeping action immutable avoids the
// ambiguity of "what happens to in-flight runs when the action changes."
type UpdateScheduleRequest struct {
	Cron string `json:"cron"`
	Note string `json:"note,omitempty"`
}

// ScheduleSummary is the per-row response for list / get. RecentRuns is
// populated only on the describe path (get / right-after-create) since the
// list iterator's per-entry payload is bounded for bandwidth.
type ScheduleSummary struct {
	ID         string       `json:"id"`
	Cron       string       `json:"cron"`
	Note       string       `json:"note,omitempty"`
	Paused     bool         `json:"paused"`
	Kind       ScheduleKind `json:"kind"`
	Workflow   string       `json:"workflow"`
	TaskQueue  string       `json:"task_queue,omitempty"`
	NextRun    *time.Time   `json:"next_run,omitempty"`
	LastRun    *RunSummary  `json:"last_run,omitempty"`
	RecentRuns []RunSummary `json:"recent_runs,omitempty"`
}

// RunSummary is the embedded recent-run shape for ScheduleSummary.LastRun.
type RunSummary struct {
	StartedAt  time.Time `json:"started_at"`
	WorkflowID string    `json:"workflow_id,omitempty"`
}

// listSchedules walks the namespace-wide schedule iterator and returns the
// summary view. Empty namespaces return an empty array, never null.
func (s *APIServer) listSchedules(w http.ResponseWriter, r *http.Request) {
	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	iter, err := sched.List(r.Context(), client.ScheduleListOptions{})
	if err != nil {
		s.logger.WithError(err).Error("schedules: list failed")
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: "list schedules: " + err.Error(), Success: false,
		})
		return
	}

	out := []ScheduleSummary{}
	for iter.HasNext() {
		entry, err := iter.Next()
		if err != nil {
			s.logger.WithError(err).Warn("schedules: list iterate failed mid-stream")
			s.sendJSON(w, http.StatusBadGateway, APIResponse{
				Error: "list iterate: " + err.Error(), Success: false,
			})
			return
		}
		out = append(out, summariseListEntry(entry))
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    map[string]interface{}{"schedules": out},
		Success: true,
	})
}

// getSchedule returns the full description of one schedule by ID.
func (s *APIServer) getSchedule(w http.ResponseWriter, r *http.Request) {
	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["scheduleID"]
	handle := sched.GetHandle(r.Context(), id)
	desc, err := handle.Describe(r.Context())
	if err != nil {
		if isNotFoundErr(err) {
			s.sendJSON(w, http.StatusNotFound, APIResponse{
				Error: "schedule not found: " + id, Success: false,
			})
			return
		}
		s.logger.WithError(err).WithField("schedule_id", id).Error("schedules: describe failed")
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: "describe schedule: " + err.Error(), Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    map[string]interface{}{"schedule": summariseDescription(id, desc)},
		Success: true,
	})
}

// createSchedule registers a new schedule. Kind discriminates between the
// delegator-shaped preset and the advanced workflow+args form.
func (s *APIServer) createSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.acquireInflight(w, "createSchedule") {
		return
	}
	defer s.releaseInflight()

	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	var req CreateScheduleRequest
	if err := decodeJSONBody(r, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: err.Error(), Success: false,
		})
		return
	}

	opts, err := buildScheduleOptions(req)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: err.Error(), Success: false,
		})
		return
	}

	// Mutation context is decoupled from r.Context() so a client hangup
	// can't cancel the Temporal RPC mid-flight (matches triggerWorkflow).
	ctx, cancel := context.WithTimeout(context.Background(), scheduleMutationTimeout)
	defer cancel()

	handle, err := sched.Create(ctx, opts)
	if err != nil {
		if isAlreadyExistsErr(err) {
			s.sendJSON(w, http.StatusConflict, APIResponse{
				Error: "schedule already exists: " + req.ID, Success: false,
			})
			return
		}
		s.logger.WithError(err).WithField("schedule_id", req.ID).Error("schedules: create failed")
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: "create schedule: " + err.Error(), Success: false,
		})
		return
	}

	// Describe right after create so the response carries the same shape
	// as get/list (next-run, last-run). If describe fails we still report
	// success since the schedule is registered — the UI can refetch.
	desc, derr := handle.Describe(ctx)
	if derr != nil {
		s.logger.WithError(derr).WithField("schedule_id", req.ID).
			Warn("schedules: describe right after create failed")
		s.sendJSON(w, http.StatusCreated, APIResponse{
			Data:    map[string]interface{}{"schedule": summaryFromCreateRequest(req)},
			Success: true,
		})
		return
	}

	s.sendJSON(w, http.StatusCreated, APIResponse{
		Data:    map[string]interface{}{"schedule": summariseDescription(req.ID, desc)},
		Success: true,
	})
}

// updateSchedule replaces the cron + note. The workflow action is
// immutable — to change workflow type/args, delete and recreate.
func (s *APIServer) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.acquireInflight(w, "updateSchedule") {
		return
	}
	defer s.releaseInflight()

	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["scheduleID"]

	var req UpdateScheduleRequest
	if err := decodeJSONBody(r, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: err.Error(), Success: false,
		})
		return
	}
	if strings.TrimSpace(req.Cron) == "" {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: "cron is required", Success: false,
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), scheduleMutationTimeout)
	defer cancel()

	handle := sched.GetHandle(ctx, id)
	err := handle.Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			updated := in.Description.Schedule
			if updated.Spec == nil {
				updated.Spec = &client.ScheduleSpec{}
			}
			updated.Spec.CronExpressions = []string{req.Cron}
			if updated.State == nil {
				updated.State = &client.ScheduleState{}
			}
			updated.State.Note = req.Note
			return &client.ScheduleUpdate{Schedule: &updated}, nil
		},
	})
	if err != nil {
		if isNotFoundErr(err) {
			s.sendJSON(w, http.StatusNotFound, APIResponse{
				Error: "schedule not found: " + id, Success: false,
			})
			return
		}
		s.logger.WithError(err).WithField("schedule_id", id).Error("schedules: update failed")
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: "update schedule: " + err.Error(), Success: false,
		})
		return
	}

	desc, derr := handle.Describe(ctx)
	if derr != nil {
		s.logger.WithError(derr).WithField("schedule_id", id).
			Warn("schedules: describe right after update failed")
		s.sendJSON(w, http.StatusOK, APIResponse{
			Data:    map[string]interface{}{"schedule": ScheduleSummary{ID: id, Cron: req.Cron, Note: req.Note}},
			Success: true,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    map[string]interface{}{"schedule": summariseDescription(id, desc)},
		Success: true,
	})
}

// deleteSchedule removes a schedule. Idempotent — deleting a non-existent
// schedule is reported as success (matches the delegator MCP's semantics).
func (s *APIServer) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.acquireInflight(w, "deleteSchedule") {
		return
	}
	defer s.releaseInflight()

	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["scheduleID"]

	ctx, cancel := context.WithTimeout(context.Background(), scheduleMutationTimeout)
	defer cancel()

	handle := sched.GetHandle(ctx, id)
	if err := handle.Delete(ctx); err != nil {
		if isNotFoundErr(err) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.logger.WithError(err).WithField("schedule_id", id).Error("schedules: delete failed")
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: "delete schedule: " + err.Error(), Success: false,
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) pauseSchedule(w http.ResponseWriter, r *http.Request) { s.pauseUnpause(w, r, true) }
func (s *APIServer) unpauseSchedule(w http.ResponseWriter, r *http.Request) {
	s.pauseUnpause(w, r, false)
}

// pauseUnpause is the shared body for /pause and /unpause. Both are POST
// no-body endpoints that flip the paused bit.
func (s *APIServer) pauseUnpause(w http.ResponseWriter, r *http.Request, pause bool) {
	if !s.acquireInflight(w, pauseLabel(pause)+"Schedule") {
		return
	}
	defer s.releaseInflight()

	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["scheduleID"]

	ctx, cancel := context.WithTimeout(context.Background(), scheduleMutationTimeout)
	defer cancel()

	handle := sched.GetHandle(ctx, id)
	var err error
	if pause {
		err = handle.Pause(ctx, client.SchedulePauseOptions{Note: "paused via dashboard"})
	} else {
		err = handle.Unpause(ctx, client.ScheduleUnpauseOptions{Note: "resumed via dashboard"})
	}
	if err != nil {
		if isNotFoundErr(err) {
			s.sendJSON(w, http.StatusNotFound, APIResponse{
				Error: "schedule not found: " + id, Success: false,
			})
			return
		}
		s.logger.WithError(err).WithField("schedule_id", id).
			Errorf("schedules: %s failed", pauseLabel(pause))
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: pauseLabel(pause) + " schedule: " + err.Error(), Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    map[string]interface{}{"paused": pause},
		Success: true,
	})
}

// triggerSchedule fires the schedule's action once, immediately, in
// addition to its normal cron firings.
func (s *APIServer) triggerSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.acquireInflight(w, "triggerSchedule") {
		return
	}
	defer s.releaseInflight()

	sched := s.pickScheduler()
	if sched == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "temporal client not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["scheduleID"]

	ctx, cancel := context.WithTimeout(context.Background(), scheduleMutationTimeout)
	defer cancel()

	handle := sched.GetHandle(ctx, id)
	if err := handle.Trigger(ctx, client.ScheduleTriggerOptions{}); err != nil {
		if isNotFoundErr(err) {
			s.sendJSON(w, http.StatusNotFound, APIResponse{
				Error: "schedule not found: " + id, Success: false,
			})
			return
		}
		s.logger.WithError(err).WithField("schedule_id", id).Error("schedules: trigger failed")
		s.sendJSON(w, http.StatusBadGateway, APIResponse{
			Error: "trigger schedule: " + err.Error(), Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusAccepted, APIResponse{
		Data:    map[string]interface{}{"triggered": true},
		Success: true,
	})
}

// --- helpers ---

// buildScheduleOptions translates a request into the Temporal SDK's
// ScheduleOptions, validating along the way.
func buildScheduleOptions(req CreateScheduleRequest) (client.ScheduleOptions, error) {
	if strings.TrimSpace(req.ID) == "" {
		return client.ScheduleOptions{}, errors.New("id is required")
	}
	if strings.TrimSpace(req.Cron) == "" {
		return client.ScheduleOptions{}, errors.New("cron is required")
	}

	var action client.ScheduleAction
	switch req.Kind {
	case ScheduleKindDelegation:
		if req.Delegation == nil {
			return client.ScheduleOptions{}, errors.New("delegation: payload is required when kind=delegation")
		}
		if strings.TrimSpace(req.Delegation.Prompt) == "" {
			return client.ScheduleOptions{}, errors.New("delegation.prompt is required")
		}
		// Input shape must match delegator.scheduleInputForSpec — the
		// DelegationWorkflow's first arg is this map. Keep these keys in
		// sync with /thearray/git/Adamaton/delegator/delegator/scheduler.go.
		input := map[string]any{
			"prompt":          req.Delegation.Prompt,
			"difficulty":      req.Delegation.Difficulty,
			"priority":        req.Delegation.Priority,
			"agent_hint":      req.Delegation.AgentHint,
			"working_dir":     req.Delegation.WorkingDir,
			"model":           req.Delegation.Model,
			"timeout_seconds": req.Delegation.TimeoutSecs,
		}
		action = &client.ScheduleWorkflowAction{
			ID:        "delegation-" + req.ID,
			Workflow:  delegationWorkflowName,
			TaskQueue: delegationTaskQueue,
			Args:      []any{input},
		}
	case ScheduleKindGeneric:
		if req.Generic == nil {
			return client.ScheduleOptions{}, errors.New("generic: payload is required when kind=generic")
		}
		if strings.TrimSpace(req.Generic.Workflow) == "" {
			return client.ScheduleOptions{}, errors.New("generic.workflow is required")
		}
		if strings.TrimSpace(req.Generic.TaskQueue) == "" {
			return client.ScheduleOptions{}, errors.New("generic.task_queue is required")
		}
		args, err := parseGenericArgs(req.Generic.ArgsJSON)
		if err != nil {
			return client.ScheduleOptions{}, fmt.Errorf("generic.args_json: %w", err)
		}
		action = &client.ScheduleWorkflowAction{
			ID:        req.ID + "-run",
			Workflow:  req.Generic.Workflow,
			TaskQueue: req.Generic.TaskQueue,
			Args:      args,
		}
	case "":
		return client.ScheduleOptions{}, errors.New("kind is required (delegation | generic)")
	default:
		return client.ScheduleOptions{}, fmt.Errorf("unknown kind %q (want delegation | generic)", req.Kind)
	}

	return client.ScheduleOptions{
		ID:     req.ID,
		Spec:   client.ScheduleSpec{CronExpressions: []string{req.Cron}},
		Action: action,
		Note:   req.Note,
		Paused: req.Paused,
	}, nil
}

// parseGenericArgs turns the request's args_json string into the []any the
// SDK wants. Empty string is fine — workflows with no args pass [].
func parseGenericArgs(raw string) ([]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []any{}, nil
	}
	var args []any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("must be a JSON array: %w", err)
	}
	return args, nil
}

// summariseListEntry collapses a ScheduleListEntry into the wire shape.
// next_run / last_run come from the entry's NextActionTimes / RecentActions
// arrays, both of which the server bounds to 5 / 10 entries respectively.
func summariseListEntry(e *client.ScheduleListEntry) ScheduleSummary {
	out := ScheduleSummary{
		ID:       e.ID,
		Note:     e.Note,
		Paused:   e.Paused,
		Workflow: e.WorkflowType.Name,
		Kind:     kindFromWorkflowName(e.WorkflowType.Name),
	}
	if e.Spec != nil && len(e.Spec.CronExpressions) > 0 {
		out.Cron = e.Spec.CronExpressions[0]
	}
	if len(e.NextActionTimes) > 0 {
		t := e.NextActionTimes[0]
		out.NextRun = &t
	}
	if len(e.RecentActions) > 0 {
		last := e.RecentActions[len(e.RecentActions)-1]
		out.LastRun = &RunSummary{StartedAt: last.ActualTime}
		if last.StartWorkflowResult != nil {
			out.LastRun.WorkflowID = last.StartWorkflowResult.WorkflowID
		}
	}
	return out
}

// summariseDescription collapses a ScheduleDescription into ScheduleSummary.
// The describe-time shape carries more fields than the list-time shape but
// we expose the same JSON envelope for both — the UI can lazily fetch
// describe when it needs the full picture.
func summariseDescription(id string, desc *client.ScheduleDescription) ScheduleSummary {
	out := ScheduleSummary{ID: id}
	if desc == nil {
		return out
	}
	if desc.Schedule.Spec != nil && len(desc.Schedule.Spec.CronExpressions) > 0 {
		out.Cron = desc.Schedule.Spec.CronExpressions[0]
	}
	if desc.Schedule.State != nil {
		out.Note = desc.Schedule.State.Note
		out.Paused = desc.Schedule.State.Paused
	}
	if act, ok := desc.Schedule.Action.(*client.ScheduleWorkflowAction); ok && act != nil {
		// On Describe the Workflow field is the type name string per the
		// SDK contract (not the function reference passed at create).
		if name, ok := act.Workflow.(string); ok {
			out.Workflow = name
		}
		out.TaskQueue = act.TaskQueue
		out.Kind = kindFromWorkflowName(out.Workflow)
	}
	if len(desc.Info.NextActionTimes) > 0 {
		t := desc.Info.NextActionTimes[0]
		out.NextRun = &t
	}
	if n := len(desc.Info.RecentActions); n > 0 {
		out.RecentRuns = make([]RunSummary, 0, n)
		for _, a := range desc.Info.RecentActions {
			rs := RunSummary{StartedAt: a.ActualTime}
			if a.StartWorkflowResult != nil {
				rs.WorkflowID = a.StartWorkflowResult.WorkflowID
			}
			out.RecentRuns = append(out.RecentRuns, rs)
		}
		// LastRun aliases the most recent entry so list-style consumers
		// (sparkline, status badge) don't have to peek inside RecentRuns.
		last := out.RecentRuns[n-1]
		out.LastRun = &last
	}
	return out
}

// summaryFromCreateRequest is the fallback summary used when Describe fails
// right after Create — we still know the request fields, so we can return
// a populated row even without server-side info like next-run.
func summaryFromCreateRequest(req CreateScheduleRequest) ScheduleSummary {
	out := ScheduleSummary{
		ID:     req.ID,
		Cron:   req.Cron,
		Note:   req.Note,
		Paused: req.Paused,
		Kind:   req.Kind,
	}
	switch req.Kind {
	case ScheduleKindDelegation:
		out.Workflow = delegationWorkflowName
		out.TaskQueue = delegationTaskQueue
	case ScheduleKindGeneric:
		if req.Generic != nil {
			out.Workflow = req.Generic.Workflow
			out.TaskQueue = req.Generic.TaskQueue
		}
	}
	return out
}

// kindFromWorkflowName derives the kind discriminator from the workflow type
// name. Anything other than the delegation workflow is reported as generic.
func kindFromWorkflowName(name string) ScheduleKind {
	if name == delegationWorkflowName {
		return ScheduleKindDelegation
	}
	return ScheduleKindGeneric
}

// pauseLabel maps the boolean to its handler-name fragment.
func pauseLabel(pause bool) string {
	if pause {
		return "pause"
	}
	return "unpause"
}

// decodeJSONBody is a small wrapper that bounds the body, decodes, and
// returns a friendly error for the two common failure modes.
func decodeJSONBody(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

// acquireInflight gates a mutation handler on the shared semaphore. Returns
// true if the slot was taken (caller must call releaseInflight); false if
// the limit was hit (handler has already written the 429 response).
func (s *APIServer) acquireInflight(w http.ResponseWriter, op string) bool {
	if s.inflightSem == nil {
		return true
	}
	select {
	case s.inflightSem <- struct{}{}:
		return true
	default:
		s.logger.WithField("op", op).Warn("schedules: rejected, inflight limit reached")
		s.sendJSON(w, http.StatusTooManyRequests, APIResponse{
			Error: "too many in-flight schedule mutations", Success: false,
		})
		return false
	}
}

// releaseInflight releases a slot taken by acquireInflight. Safe to call
// when inflightSem is nil (no-op).
func (s *APIServer) releaseInflight() {
	if s.inflightSem == nil {
		return
	}
	<-s.inflightSem
}

// isAlreadyExistsErr / isNotFoundErr mirror delegator's helpers. Prefer the
// typed serviceerror values; fall back to substring on rare wrap paths.
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	var ae *serviceerror.AlreadyExists
	if errors.As(err, &ae) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "alreadyexists")
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "notfound")
}

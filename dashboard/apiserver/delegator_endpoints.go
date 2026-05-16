// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/sirus20x6/adamaton-delegator/delegator"
	"github.com/sirus20x6/adamaton-delegator/delegator/quota"
)

// setupDelegatorRoutes registers /api/delegator/quota + /api/delegator/tasks
// on the given subrouter. Both endpoints are read-only — the dashboard
// surfaces tasks and quota state, but new delegations still go through the
// MCP tool. (Phase-3 scope; "delegate from UI" is deferred until the task
// model is Temporal-backed in Phase 2.)
//
// The delegator task store is opened once at apiserver boot and held on
// the server struct; both writer (MCP) and reader (us) point at the
// same postgres instance — the "open + close per request" dance the
// sqlite path used to avoid blocking the writer is no longer needed.
func (s *APIServer) setupDelegatorRoutes(api *mux.Router) {
	api.HandleFunc("/delegator/quota", s.handleDelegatorQuota).Methods("GET")
	api.HandleFunc("/delegator/tasks", s.handleDelegatorTasks).Methods("GET")
}

func (s *APIServer) handleDelegatorQuota(w http.ResponseWriter, r *http.Request) {
	days := 1
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconvAtoiSafe(v); err == nil && n > 0 && n <= 30 {
			days = n
		}
	}
	usage, err := quota.GetAllAgentUsage(r.Context(), days, quota.AggregateConfig{
		Logger:              s.logger,
		SkipGeminiLiveQuota: false,
	})
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: err.Error(), Success: false,
		})
		return
	}
	s.sendJSON(w, http.StatusOK, APIResponse{Data: map[string]any{"agents": usage}, Success: true})
}

func (s *APIServer) handleDelegatorTasks(w http.ResponseWriter, r *http.Request) {
	if s.delegatorStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "delegator task store unavailable (postgres.dsn not configured?)", Success: false,
		})
		return
	}
	status := strings.ToLower(r.URL.Query().Get("status"))
	agent := strings.ToLower(r.URL.Query().Get("agent"))

	tasks := s.delegatorStore.List(delegator.TaskStatus(status), agent)
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, summariseTask(t))
	}
	s.sendJSON(w, http.StatusOK, APIResponse{Data: out, Success: true})
}

// summariseTask flattens the on-disk task record into a UI-friendly shape.
// We deliberately do NOT include the full Output (potentially large); the
// UI fetches detail on click. This response is cheap to render in a list.
func summariseTask(t *delegator.Task) map[string]any {
	created := t.CreatedAt
	end := t.CompletedAt
	if end.IsZero() && !t.StartedAt.IsZero() {
		end = time.Now().UTC()
	}
	elapsedSec := 0
	switch {
	case !t.StartedAt.IsZero() && !end.IsZero():
		elapsedSec = int(end.Sub(t.StartedAt).Seconds())
	case !created.IsZero():
		elapsedSec = int(time.Now().UTC().Sub(created).Seconds())
	}
	return map[string]any{
		"id":              t.ID,
		"agent":           t.Agent,
		"provider":        t.Provider,
		"difficulty":      t.Difficulty,
		"priority":        t.Priority,
		"status":          string(t.Status),
		"prompt_preview":  truncatePrompt(t.Prompt, 200),
		"created_at":      t.CreatedAt,
		"started_at":      nilIfZero(t.StartedAt),
		"completed_at":    nilIfZero(t.CompletedAt),
		"exit_code":       t.ExitCode,
		"elapsed_seconds": elapsedSec,
		"error":           t.Error,
	}
}

func truncatePrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func nilIfZero(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// strconvAtoiSafe is a tiny strconv.Atoi shim that returns (0, error) for
// empty input rather than the awkward strconv error message. Saves a
// strconv import in this file.
func strconvAtoiSafe(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, errEmpty
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

var (
	errEmpty  = httpError("empty")
	errBadInt = httpError("not an integer")
)

type httpError string

func (e httpError) Error() string { return string(e) }
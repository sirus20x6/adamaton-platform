// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirus20x6/adamomaton-evolve/workflow-builder/activityregistry"
	"github.com/sirus20x6/adamomaton-core/metrics"
	"github.com/sirus20x6/adamomaton-evolve/workflow-builder/wfvalidate"
	"github.com/sirus20x6/adamomaton-evolve/workflow-builder/workflowstore"
	"github.com/sirus20x6/adamomaton-evolve/workflow-builder/workflows"
	"go.temporal.io/sdk/client"
)

// shortID returns the first 8 chars of id, or id itself if it's shorter.
// Avoids panics on truncated or empty IDs.
func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// setupWorkflowBuilderRoutes registers workflow builder endpoints.
// Requires workflowStore to be set on the APIServer.
func (s *APIServer) setupWorkflowBuilderRoutes(api *mux.Router) {
	api.HandleFunc("/workflows/activities", s.listActivities).Methods("GET")
	api.HandleFunc("/workflows/activities/extended", s.listExtendedActivities).Methods("GET")
	api.HandleFunc("/workflows/roles", s.listRoles).Methods("GET")
	api.HandleFunc("/workflows/definitions", s.listDefinitions).Methods("GET")
	api.HandleFunc("/workflows/definitions", s.createDefinition).Methods("POST")
	api.HandleFunc("/workflows/definitions/{id}", s.getDefinition).Methods("GET")
	api.HandleFunc("/workflows/definitions/{id}", s.updateDefinition).Methods("PUT")
	api.HandleFunc("/workflows/definitions/{id}", s.deleteDefinition).Methods("DELETE")
	api.HandleFunc("/workflows/definitions/{id}/run", s.runDefinition).Methods("POST")
	api.HandleFunc("/workflows/runs", s.listRuns).Methods("GET")
	api.HandleFunc("/workflows/runs/{id}", s.getRun).Methods("GET")
}

// listActivities returns all available activities for the workflow builder palette.
func (s *APIServer) listActivities(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    activityregistry.Registry(),
		Success: true,
	})
}

// listExtendedActivities returns plugin node definitions with full property schemas.
func (s *APIServer) listExtendedActivities(w http.ResponseWriter, r *http.Request) {
	if s.pluginLoader == nil {
		// Fallback to basic registry
		s.listActivities(w, r)
		return
	}

	nodes := s.pluginLoader.All()
	cats := s.pluginLoader.Categories()

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]interface{}{
			"nodes":      nodes,
			"categories": cats,
		},
		Success: true,
	})
}

// listRoles returns all available roles for workflow routing.
func (s *APIServer) listRoles(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]interface{}{
			"roles":  activityregistry.Roles(),
			"groups": activityregistry.RoleGroups(),
		},
		Success: true,
	})
}

// listDefinitions returns all saved workflow definitions.
func (s *APIServer) listDefinitions(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	defs, err := s.workflowStore.ListDefinitions()
	if err != nil {
		s.logger.WithError(err).Error("Failed to list definitions")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to list definitions", Success: false,
		})
		return
	}
	if defs == nil {
		defs = []workflowstore.WorkflowDefinition{}
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: defs, Success: true})
}

// createDefinition saves a new workflow definition.
func (s *APIServer) createDefinition(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Definition  json.RawMessage `json:"definition"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{Error: "invalid request body", Success: false})
		return
	}

	if req.Name == "" {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{Error: "name is required", Success: false})
		return
	}
	if len(req.Definition) == 0 {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{Error: "definition is required", Success: false})
		return
	}

	// Validate that the definition parses and is structurally sound. The
	// shared wfvalidate package owns the rules so the demo server applies
	// them identically.
	if err := wfvalidate.ValidateDynamicWorkflowDefinition(string(req.Definition)); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error: err.Error(), Success: false,
		})
		return
	}

	def, err := s.workflowStore.CreateDefinition(req.Name, req.Description, string(req.Definition))
	if err != nil {
		s.logger.WithError(err).Error("Failed to create definition")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to create definition", Success: false,
		})
		return
	}

	s.sendJSON(w, http.StatusCreated, APIResponse{Data: def, Success: true})
}

// getDefinition returns a single workflow definition.
func (s *APIServer) getDefinition(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["id"]
	def, err := s.workflowStore.GetDefinition(id)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, APIResponse{Error: "definition not found", Success: false})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: def, Success: true})
}

// updateDefinition updates an existing workflow definition.
func (s *APIServer) updateDefinition(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["id"]

	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Definition  json.RawMessage `json:"definition"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{Error: "invalid request body", Success: false})
		return
	}

	if req.Name == "" {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{Error: "name is required", Success: false})
		return
	}

	// Validate definition if provided. Same shared validator as createDefinition.
	if len(req.Definition) > 0 {
		if err := wfvalidate.ValidateDynamicWorkflowDefinition(string(req.Definition)); err != nil {
			s.sendJSON(w, http.StatusBadRequest, APIResponse{
				Error: err.Error(), Success: false,
			})
			return
		}
	}

	def, err := s.workflowStore.UpdateDefinition(id, req.Name, req.Description, string(req.Definition))
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, APIResponse{Error: "definition not found", Success: false})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: def, Success: true})
}

// deleteDefinition removes a workflow definition.
func (s *APIServer) deleteDefinition(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["id"]
	if err := s.workflowStore.DeleteDefinition(id); err != nil {
		s.sendJSON(w, http.StatusNotFound, APIResponse{Error: "definition not found", Success: false})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]string{"id": id, "status": "deleted"}, Success: true,
	})
}

// runDefinition triggers a workflow execution from a saved definition.
func (s *APIServer) runDefinition(w http.ResponseWriter, r *http.Request) {
	// Bound concurrent mutation requests with the same semaphore the
	// triggerWorkflow handler uses; both fan out into Temporal scheduling
	// RPCs and share the same connection pool budget.
	if s.inflightSem != nil {
		select {
		case s.inflightSem <- struct{}{}:
			defer func() { <-s.inflightSem }()
		default:
			s.logger.Warn("runDefinition rejected: inflight limit reached")
			s.sendJSON(w, http.StatusTooManyRequests, APIResponse{
				Error: "too many in-flight workflow trigger requests", Success: false,
			})
			return
		}
	}

	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["id"]

	// Get the definition
	def, err := s.workflowStore.GetDefinition(id)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, APIResponse{Error: "definition not found", Success: false})
		return
	}

	// Parse optional params from request body. An empty body is OK (no
	// params); malformed JSON is worth a log line so operators can see
	// when the UI sends garbage. We still proceed with an empty params
	// map either way, matching the demo server's behavior.
	var params map[string]interface{}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		if !errors.Is(err, io.EOF) {
			s.logger.WithError(err).WithField("definition", def.ID).
				Warn("runDefinition: failed to decode params, proceeding with empty map")
		}
		params = make(map[string]interface{})
	}
	if params == nil {
		params = make(map[string]interface{})
	}

	// Build workflow args
	args := workflows.DynamicWorkflowArgs{
		DefinitionID: def.ID,
		Definition:   json.RawMessage(def.Definition),
		Params:       params,
	}

	workflowID := fmt.Sprintf("dynamic-%s-%d", shortID(def.ID), time.Now().Unix())

	// Workflow start is fire-and-forget — do NOT plumb r.Context() into
	// the scheduling RPC. A client hangup mid-POST shouldn't cancel the
	// workflow we just decided to schedule; the work is owned by Temporal
	// once accepted. Cap at 30s so a wedged Temporal frontend can't pin
	// our inflight slot. Mirrors triggerWorkflow's pattern.
	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	we, err := s.pickStarter().ExecuteWorkflow(
		execCtx,
		client.StartWorkflowOptions{
			ID:        workflowID,
			TaskQueue: s.config.Temporal.TaskQueue,
		},
		workflows.DynamicWorkflow,
		args,
	)
	if err != nil {
		s.logger.WithError(err).Error("Failed to start dynamic workflow")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to start workflow", Success: false,
		})
		return
	}

	metrics.WorkflowsStarted.WithLabelValues("DynamicWorkflow", "api").Inc()

	// Record the run. The Temporal workflow has already been started — if
	// persistence fails here, return success with an empty run record and a
	// warning rather than reporting a failure (which would falsely imply the
	// workflow did not start). The UI must not assume `data.run` is present;
	// previously this branch returned `run: null` while still reporting
	// Success=true, which crashed callers reading `result.run.id`.
	paramsJSON, _ := json.Marshal(params)
	run, err := s.workflowStore.CreateRun(def.ID, we.GetID(), we.GetRunID(), string(paramsJSON))
	if err != nil {
		s.logger.WithError(err).Error("Failed to record run; workflow already started in Temporal")
		s.sendJSON(w, http.StatusOK, APIResponse{
			Data: map[string]interface{}{
				"workflowID": we.GetID(),
				"runID":      we.GetRunID(),
				"warning":    "workflow started in Temporal but run record could not be persisted",
			},
			Success: true,
		})
		return
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]interface{}{
			"run":        run,
			"workflowID": we.GetID(),
			"runID":      we.GetRunID(),
		},
		Success: true,
	})
}

// listRuns returns recent workflow runs.
func (s *APIServer) listRuns(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	definitionID := r.URL.Query().Get("definition_id")

	// Honor ?limit= when present; clamp to [1, 100] for parity with the demo
	// server. Anything malformed falls back to the store's default.
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			if parsed < 1 {
				parsed = 1
			} else if parsed > 100 {
				parsed = 100
			}
			limit = parsed
		}
	}

	runs, err := s.workflowStore.ListRuns(definitionID, limit)
	if err != nil {
		s.logger.WithError(err).Error("Failed to list runs")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error: "failed to list runs", Success: false,
		})
		return
	}
	if runs == nil {
		runs = []workflowstore.WorkflowRun{}
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: runs, Success: true})
}

// getRun returns a single workflow run with status.
func (s *APIServer) getRun(w http.ResponseWriter, r *http.Request) {
	if s.workflowStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "workflow store not initialized", Success: false,
		})
		return
	}

	id := mux.Vars(r)["id"]
	run, err := s.workflowStore.GetRun(id)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, APIResponse{Error: "run not found", Success: false})
		return
	}

	// If still running, try to get status from Temporal
	if run.Status == "running" {
		desc, err := s.pickDescriber().DescribeWorkflowExecution(r.Context(), run.TemporalID, "")
		if err == nil {
			status := desc.WorkflowExecutionInfo.Status.String()
			if status != "Running" {
				newStatus := "completed"
				if status == "Failed" || status == "TimedOut" || status == "Terminated" {
					newStatus = "failed"
				} else if status == "Canceled" {
					newStatus = "cancelled"
				}
				// Empty output here is intentional and is honored by the
				// store: terminal-status updates with output=="" preserve any
				// previously recorded output payload instead of clobbering it.
				//
				// Choice: when the persistence write fails, log it and return
				// the *original* run.Status (the value still on disk) rather
				// than the newly-derived one. Lying to the caller — saying
				// "completed" while the DB still says "running" — is worse
				// than a stale read; the next poll will re-attempt the
				// transition.
				if updErr := s.workflowStore.UpdateRunStatus(run.ID, newStatus, ""); updErr != nil {
					s.logger.WithError(updErr).WithField("run_id", run.ID).
						Warn("failed to persist run status; returning previously stored status")
				} else {
					run.Status = newStatus
				}
			}
		}
	}

	s.sendJSON(w, http.StatusOK, APIResponse{Data: run, Success: true})
}
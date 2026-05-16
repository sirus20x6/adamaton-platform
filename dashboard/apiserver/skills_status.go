// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

// Workflow status endpoints used by the Skills UI to poll Temporal for
// long-running operations: per-skill R2R sync (skill-sync-{id}), the
// import workflow's live progress (skill-import-{uuid}), and the
// one-off check-source result (skill-check-{skill_id}). All three
// thinly wrap the Temporal client — the dashboard does not maintain a
// shadow status table.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	skillsworkflows "github.com/sirus20x6/adamaton-knowledge/skills/workflows"
	"go.temporal.io/api/serviceerror"
)

// registerSkillsStatusEndpoints mounts the three status endpoints.
// Called alongside registerSkillsEndpoints in setupRoutes.
func (s *APIServer) registerSkillsStatusEndpoints(api *mux.Router) {
	api.HandleFunc("/skills/{id}/sync-status", s.getSkillSyncStatus).Methods("GET")
	api.HandleFunc("/skills/imports/{workflow_id}/progress", s.getImportProgress).Methods("GET")
	api.HandleFunc("/skills/checks/{skill_id}/result", s.getCheckSourceResult).Methods("GET")
}

// skillSyncStatusResponse is the JSON returned by getSkillSyncStatus.
// status uses Temporal's string enum names ("Running" | "Completed" |
// "Failed" | ...) so the UI can match exactly. "NotFound" is the
// dashboard's own sentinel — Temporal returns a typed NotFound error,
// which we tolerate as a non-error so the UI can render "never synced"
// without surfacing a 404 toast.
type skillSyncStatusResponse struct {
	Status       string `json:"status"`
	AttemptCount int64  `json:"attempt_count"`
}

func (s *APIServer) getSkillSyncStatus(w http.ResponseWriter, r *http.Request) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	desc, err := s.pickDescriber().DescribeWorkflowExecution(ctx, "skill-sync-"+id, "")
	if err != nil {
		// Temporal's NotFound is "this workflow never ran", which is a
		// valid steady state for a skill that's never been written
		// since the R2R integration shipped. Surface as JSON, not a
		// transport error.
		var nf *serviceerror.NotFound
		if errors.As(err, &nf) {
			writeEvoJSON(w, skillSyncStatusResponse{Status: "NotFound"})
			return
		}
		s.logger.WithError(err).WithField("skill_id", id).
			Warn("getSkillSyncStatus: describe failed")
		writeEvoErr(w, http.StatusBadGateway, "describe workflow: "+err.Error())
		return
	}
	info := desc.WorkflowExecutionInfo
	writeEvoJSON(w, skillSyncStatusResponse{
		Status:       info.Status.String(),
		AttemptCount: int64(info.HistoryLength), // best-effort proxy; SDK exposes attempts via the next event
	})
}

// importProgressResponse combines a live progress snapshot (queried
// from the running workflow's `progress` handler) with the terminal
// status (Running | Completed | Failed | ...). The frontend uses
// status==Completed as the cue to refetch the skills list.
type importProgressResponse struct {
	Progress *skillsworkflows.ImportProgress `json:"progress"`
	Status   string                          `json:"status"`
}

func (s *APIServer) getImportProgress(w http.ResponseWriter, r *http.Request) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}
	workflowID := mux.Vars(r)["workflow_id"]
	if workflowID == "" {
		writeEvoErr(w, http.StatusBadRequest, "workflow_id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := importProgressResponse{}

	// DescribeWorkflowExecution gives us the terminal status. NotFound
	// from a stale poll (workflow purged from history) is rendered as
	// NotFound so the UI can stop polling without a toast.
	desc, err := s.pickDescriber().DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		var nf *serviceerror.NotFound
		if errors.As(err, &nf) {
			resp.Status = "NotFound"
			writeEvoJSON(w, resp)
			return
		}
		s.logger.WithError(err).WithField("workflow_id", workflowID).
			Warn("getImportProgress: describe failed")
		writeEvoErr(w, http.StatusBadGateway, "describe workflow: "+err.Error())
		return
	}
	resp.Status = desc.WorkflowExecutionInfo.Status.String()

	// Query the workflow for its live progress. The query handler is
	// only registered while the workflow is alive — once Completed /
	// Failed, the query path returns NotFound. We swallow that case so
	// the frontend can show the last terminal status without a second
	// snapshot.
	queryResp, qErr := s.temporalClient.QueryWorkflow(ctx, workflowID, "", skillsworkflows.QueryProgress)
	if qErr == nil && queryResp != nil {
		var progress skillsworkflows.ImportProgress
		if decErr := queryResp.Get(&progress); decErr == nil {
			resp.Progress = &progress
		} else {
			s.logger.WithError(decErr).WithField("workflow_id", workflowID).
				Warn("getImportProgress: failed to decode progress payload")
		}
	} else if qErr != nil {
		// QueryFailed is the expected outcome once the workflow exits —
		// log at debug-ish info, not warn, so a long-completed import
		// doesn't spam the logs as the UI keeps polling.
		var nf *serviceerror.NotFound
		var qf *serviceerror.QueryFailed
		if !errors.As(qErr, &nf) && !errors.As(qErr, &qf) {
			s.logger.WithError(qErr).WithField("workflow_id", workflowID).
				Warn("getImportProgress: query failed")
		}
	}

	writeEvoJSON(w, resp)
}

// checkSourceResultResponse exposes the CheckSkillSourceWorkflow's
// terminal result alongside its status. Result is nil for any
// non-completed status — the UI only renders the diff when status ==
// "Completed".
type checkSourceResultResponse struct {
	Status string                                  `json:"status"`
	Result *skillsworkflows.CheckSkillSourceResult `json:"result"`
}

func (s *APIServer) getCheckSourceResult(w http.ResponseWriter, r *http.Request) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}
	skillID := mux.Vars(r)["skill_id"]
	if skillID == "" {
		writeEvoErr(w, http.StatusBadRequest, "skill_id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	workflowID := "skill-check-" + skillID
	desc, err := s.pickDescriber().DescribeWorkflowExecution(ctx, workflowID, "")
	if err != nil {
		var nf *serviceerror.NotFound
		if errors.As(err, &nf) {
			writeEvoJSON(w, checkSourceResultResponse{Status: "NotFound"})
			return
		}
		s.logger.WithError(err).WithField("skill_id", skillID).
			Warn("getCheckSourceResult: describe failed")
		writeEvoErr(w, http.StatusBadGateway, "describe workflow: "+err.Error())
		return
	}
	status := desc.WorkflowExecutionInfo.Status.String()
	resp := checkSourceResultResponse{Status: status}

	// Only fetch the workflow's result once it's safely terminal. The
	// SDK's GetWorkflow + .Get() will block until completion otherwise,
	// which would defeat the 5s context budget and lock up the UI.
	if status == "Completed" {
		var result skillsworkflows.CheckSkillSourceResult
		run := s.temporalClient.GetWorkflow(ctx, workflowID, "")
		if gerr := run.Get(ctx, &result); gerr != nil {
			s.logger.WithError(gerr).WithField("skill_id", skillID).
				Warn("getCheckSourceResult: failed to fetch result")
		} else {
			resp.Result = &result
		}
	}
	writeEvoJSON(w, resp)
}
package apiserver

// POST /platform/experiments/launch — atomic "create experiment row +
// fire TrainExperimentWorkflow" handler. Decoupled from the regular
// create endpoint so the launch button on the frontend can hit a
// single URL that handles both halves; bare experiment creation (no
// training kick-off) still goes through POST /platform/experiments.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	trainwf "github.com/sirus20x6/adamaton-platform/temporal/workflows"
)

// ExperimentLaunchRequest is the POST body. cmd is the argv vector the
// remote /opt/adamaton/run_train.sh wrapper executes in /tmp/exp-<id>;
// the caller picks the entry point (e.g. ["python", "train.py",
// "--dataset", "$DATASET_VERSION_ID"]).
//
// If cmd is empty, the launch is rejected — we don't ship a default
// command because the right one depends on dataset task_type and isn't
// the apiserver's call to make.
type ExperimentLaunchRequest struct {
	Name             string   `json:"name"`
	Hypothesis       string   `json:"hypothesis"`
	CodeDiff         *string  `json:"code_diff,omitempty"`
	CommitHash       *string  `json:"commit_hash,omitempty"`
	ParentID         *string  `json:"parent_id,omitempty"`
	AgentSessionID   *string  `json:"agent_session_id,omitempty"`
	DatasetVersionID *string  `json:"dataset_version_id,omitempty"`
	Split            *string  `json:"split,omitempty"`
	Cmd              []string `json:"cmd"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
}

// ExperimentLaunchResponse mirrors the dataset import response shape so
// the React side can reuse the same "workflow accepted" toast.
type ExperimentLaunchResponse struct {
	Experiment Experiment `json:"experiment"`
	WorkflowID string     `json:"workflow_id"`
	RunID      string     `json:"run_id"`
}

func (s *APIServer) launchExperiment(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req ExperimentLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Hypothesis = strings.TrimSpace(req.Hypothesis)
	if req.Name == "" {
		writeEvoErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Hypothesis == "" {
		writeEvoErr(w, http.StatusBadRequest, "hypothesis is required")
		return
	}
	if len(req.Cmd) == 0 {
		writeEvoErr(w, http.StatusBadRequest, "cmd is required (no default entry point)")
		return
	}

	// Step 1 — INSERT pending row. Same column set + validation as the
	// regular create handler. tags defaults to [].
	const insertSQL = `
INSERT INTO platform.experiments
    (agent_session_id, name, hypothesis, code_diff, status,
     parent_id, commit_hash, tags, dataset_version_id, split)
VALUES ($1, $2, $3, $4, 'pending', $5, $6, '[]'::jsonb, $7, $8)
RETURNING ` + experimentColumns
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, insertSQL,
		req.AgentSessionID, req.Name, req.Hypothesis, req.CodeDiff,
		req.ParentID, req.CommitHash, req.DatasetVersionID, req.Split,
	)
	exp, err := scanExperiment(row)
	if err != nil {
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusBadRequest, "foreign key violation: "+err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}

	// Step 2 — kick the workflow. Detached context so a client hangup
	// doesn't cancel the workflow start; matches the dataset-import
	// pattern (datasets_endpoints.go:267).
	workflowID := "train-exp-" + exp.ID
	wfCtx, wfCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wfCancel()

	datasetVer := ""
	if req.DatasetVersionID != nil {
		datasetVer = *req.DatasetVersionID
	}
	split := ""
	if req.Split != nil {
		split = *req.Split
	}

	run, wfErr := s.temporalClient.ExecuteWorkflow(wfCtx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: trainwf.TaskQueueTrainExperiment,
	}, trainwf.TrainExperimentWorkflow, trainwf.TrainExperimentInput{
		ExperimentID:     exp.ID,
		DatasetVersionID: datasetVer,
		Split:            split,
		Cmd:              req.Cmd,
		TimeoutSeconds:   req.TimeoutSeconds,
	})
	if wfErr != nil {
		// Mark the row failed so it doesn't sit forever as pending —
		// the operator can see "workflow start failed" in notes.
		_, _ = s.evoPool.Exec(context.Background(),
			`UPDATE platform.experiments SET status = 'failed', notes = $2, finished_at = NOW() WHERE id = $1`,
			exp.ID, "launch failed: "+wfErr.Error())

		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(wfErr, &alreadyStarted) {
			writeEvoErr(w, http.StatusConflict, "training workflow already running for this experiment id")
			return
		}
		s.logger.WithError(wfErr).WithField("experiment_id", exp.ID).
			Error("launchExperiment: failed to start TrainExperimentWorkflow")
		writeEvoErr(w, http.StatusInternalServerError, "execute workflow: "+wfErr.Error())
		return
	}

	// Best-effort stamp workflow_id on the row right away so the UI can
	// link from the experiment to the Temporal run before the workflow
	// has had a chance to call ExperimentMarkRunning itself.
	_, _ = s.evoPool.Exec(context.Background(),
		`UPDATE platform.experiments SET workflow_id = $2 WHERE id = $1`,
		exp.ID, run.GetID())
	wfID := run.GetID()
	exp.WorkflowID = &wfID

	writeEvoJSONStatus(w, http.StatusAccepted, ExperimentLaunchResponse{
		Experiment: exp,
		WorkflowID: run.GetID(),
		RunID:      run.GetRunID(),
	})
}

// Keep uuid import live for the parent_id / dataset_version_id paths the
// create handler already validates; the launch endpoint relies on the
// pgx FK violation 23503 mapping rather than re-parsing here.
var _ = uuid.Nil

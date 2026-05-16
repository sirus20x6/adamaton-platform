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
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"go.temporal.io/sdk/client"

	dispatchworkflows "github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

// vramFloor returns the MinVRAMGB requirement for an evolution run.
// 0 when SkipEval is true (the workflow runs without GPU work), 8 GiB
// otherwise — covers the L1 corpus's working set with headroom and
// excludes nodes with toy/old GPUs.
func vramFloor(skipEval bool) int {
	if skipEval {
		return 0
	}
	return 8
}

// EvoTask is one row in GET /api/v1/evo/tasks — what populates the
// task-picker dropdown on the SPA Evolution page. seed_program is
// intentionally omitted from the list payload (can be several KB per
// row); the POST /runs handler fetches it server-side.
type EvoTask struct {
	ID        string    `json:"id"`
	Domain    string    `json:"domain"`
	Name      string    `json:"name"`
	Level     int       `json:"level"`
	CreatedAt time.Time `json:"created_at"`
}

// EvoRunCreateInput is the body of POST /api/v1/evo/runs. Mirrors
// evo-cli run's flags one-to-one so anyone fluent in the CLI can
// transcribe a command into this payload. Required: TaskID. Everything
// else has a sensible default; SkipEval defaults to true so a UI-initiated
// run never silently kicks off a real GPU evaluation.
type EvoRunCreateInput struct {
	TaskID                 string  `json:"task_id"`
	Iterations             int     `json:"iterations,omitempty"`
	Hardware               string  `json:"hardware,omitempty"`
	NCorrectness           int     `json:"n_correctness,omitempty"`
	NTrial                 int     `json:"n_trial,omitempty"`
	EvalTimeoutSecs        int     `json:"eval_timeout_secs,omitempty"`
	// SkipEval defaults to true — plumbing-test mode, no GPU usage.
	// The form must opt in to real evaluation by passing skip_eval=false
	// AND task_path / seed_path (filesystem paths on the worker).
	SkipEval               *bool   `json:"skip_eval,omitempty"`
	TaskPath               string  `json:"task_path,omitempty"`
	SeedPath               string  `json:"seed_path,omitempty"`
	MutatorModel           string  `json:"mutator_model,omitempty"`
	MutatorDryRun          bool    `json:"mutator_dry_run,omitempty"`
	MutatorSystemMessage   string  `json:"mutator_system_message,omitempty"`
	MemoryEnabled          bool    `json:"memory_enabled,omitempty"`
	MemoryDomain           string  `json:"memory_domain,omitempty"`
	MemoryTopK             int     `json:"memory_top_k,omitempty"`
	DeepResearchCollection string  `json:"deepresearch_collection,omitempty"`
	InsightThreshold       float64 `json:"insight_threshold,omitempty"`
	TaskQueue              string  `json:"task_queue,omitempty"`
}

// evolutionWorkflowInput mirrors evolve/workflows.EvolutionWorkflowInput
// — JSON tags must stay in lockstep. Defined locally to keep dashboard
// from depending on the evolve module just for the wire shape.
type evolutionWorkflowInput struct {
	RunID                  string  `json:"run_id"`
	TaskPath               string  `json:"task_path"`
	SeedPath               string  `json:"seed_path"`
	SeedSource             string  `json:"seed_source"`
	Iterations             int     `json:"iterations"`
	Hardware               string  `json:"hardware,omitempty"`
	NCorrectness           int     `json:"n_correctness,omitempty"`
	NTrial                 int     `json:"n_trial,omitempty"`
	EvalTimeoutSecs        int     `json:"eval_timeout_secs,omitempty"`
	SkipEval               bool    `json:"skip_eval,omitempty"`
	MutatorModel           string  `json:"mutator_model,omitempty"`
	MutatorDryRun          bool    `json:"mutator_dry_run,omitempty"`
	MutatorSystemMessage   string  `json:"mutator_system_message,omitempty"`
	MemoryEnabled          bool    `json:"memory_enabled,omitempty"`
	MemoryDomain           string  `json:"memory_domain,omitempty"`
	MemoryTopK             int     `json:"memory_top_k,omitempty"`
	DeepResearchCollection string  `json:"deepresearch_collection,omitempty"`
	InsightThreshold       float64 `json:"insight_threshold,omitempty"`
}

// EvoRunCreateResponse: 201 body when a run is successfully kicked off.
type EvoRunCreateResponse struct {
	RunID         string `json:"run_id"`
	WorkflowID    string `json:"workflow_id"`
	TemporalRunID string `json:"temporal_run_id"`
}

// registerEvoRunCreate adds the new write endpoints alongside the
// existing /api/v1/evo read endpoints. server.go calls this from
// registerEvoEndpoints once the pool is available.
func (s *APIServer) registerEvoRunCreate(api *mux.Router) {
	api.HandleFunc("/evo/tasks", s.handleEvoTasks).Methods("GET")
	api.HandleFunc("/evo/runs", s.handleCreateEvoRun).Methods("POST")
}

func (s *APIServer) handleEvoTasks(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.evoPool.Query(ctx, `
		SELECT id, domain, name, level, created_at
		FROM evo.tasks
		ORDER BY domain, name, created_at DESC
		LIMIT 200
	`)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]EvoTask, 0)
	for rows.Next() {
		var t EvoTask
		if err := rows.Scan(&t.ID, &t.Domain, &t.Name, &t.Level, &t.CreatedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, t)
	}
	writeEvoJSON(w, out)
}

func (s *APIServer) handleCreateEvoRun(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var in EvoRunCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.TaskID = strings.TrimSpace(in.TaskID)
	if in.TaskID == "" {
		writeEvoErr(w, http.StatusBadRequest, "task_id is required")
		return
	}

	// Resolved defaults — match evo-cli flag defaults in
	// evolve/cmd/evo-cli/main.go:317-330 so a CLI user transcribing a
	// command produces an equivalent run.
	if in.Iterations <= 0 {
		in.Iterations = 5
	}
	if in.NCorrectness <= 0 {
		in.NCorrectness = 3
	}
	if in.NTrial <= 0 {
		in.NTrial = 100
	}
	if in.EvalTimeoutSecs <= 0 {
		in.EvalTimeoutSecs = 300
	}
	if in.InsightThreshold <= 0 {
		in.InsightThreshold = 1.10
	}
	if in.MemoryTopK <= 0 {
		in.MemoryTopK = 5
	}
	// SkipEval defaults TRUE — plumbing-test mode. Caller must explicitly
	// pass false to run the real GPU evaluator.
	skipEval := true
	if in.SkipEval != nil {
		skipEval = *in.SkipEval
	}
	if !skipEval && (in.TaskPath == "" || in.SeedPath == "") {
		writeEvoErr(w, http.StatusBadRequest,
			"skip_eval=false requires task_path and seed_path (filesystem paths on the worker)")
		return
	}
	taskQueue := in.TaskQueue
	if taskQueue == "" {
		taskQueue = "evo"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Pull the seed_program text + verify the task exists. seed_source
	// is shipped into the workflow input so the evaluator activity can
	// resurrect the starting candidate without a filesystem read.
	var seedSource string
	if err := s.evoPool.QueryRow(ctx,
		`SELECT seed_program FROM evo.tasks WHERE id = $1`,
		in.TaskID,
	).Scan(&seedSource); err != nil {
		writeEvoErr(w, http.StatusNotFound, "task not found: "+err.Error())
		return
	}

	// Full UUID rather than the 8-char prefix used previously — 32 bits
	// of entropy hits ~65k-row collision probability fast once the
	// dashboard sees real volume, and the run-id is a primary key in
	// evo.runs. The full UUID gives us 122 bits.
	runID := "evo-run-" + uuid.New().String()
	// dispatchWorkflowID is the ID of the DispatchWorkflow that fronts
	// this run; the actual EvolutionWorkflow is its child and gets ID
	// "child-{jobID}" inside the dispatcher. We store the dispatch ID
	// in evo.runs.temporal_id so the UI can navigate Temporal → dispatch
	// → child without a separate lookup.
	dispatchWorkflowID := "dispatch-" + uuid.New().String()

	cfgBlob, _ := json.Marshal(map[string]interface{}{
		"iterations":              in.Iterations,
		"skip_eval":               skipEval,
		"hardware":                in.Hardware,
		"task_path":               in.TaskPath,
		"seed_path":               in.SeedPath,
		"mutator_model":           in.MutatorModel,
		"mutator_dry_run":         in.MutatorDryRun,
		"memory_enabled":          in.MemoryEnabled,
		"memory_domain":           in.MemoryDomain,
		"memory_top_k":            in.MemoryTopK,
		"deepresearch_collection": in.DeepResearchCollection,
		"insight_threshold":       in.InsightThreshold,
		"n_correctness":           in.NCorrectness,
		"n_trial":                 in.NTrial,
		"eval_timeout_secs":       in.EvalTimeoutSecs,
		"started_by":              "dashboard",
	})

	// Insert the run row first so the UI sees it as soon as the POST
	// returns. Matches the CLI's "insert then ExecuteWorkflow" sequence
	// (evolve/cmd/evo-cli/main.go:374-386).
	if _, err := s.evoPool.Exec(ctx,
		`INSERT INTO evo.runs (id, task_id, config, status, temporal_id)
		 VALUES ($1, $2, $3, 'pending', $4)`,
		runID, in.TaskID, cfgBlob, dispatchWorkflowID,
	); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "insert run: "+err.Error())
		return
	}

	wfIn := evolutionWorkflowInput{
		RunID:                  runID,
		TaskPath:               in.TaskPath,
		SeedPath:               in.SeedPath,
		SeedSource:             seedSource,
		Iterations:             in.Iterations,
		Hardware:               in.Hardware,
		NCorrectness:           in.NCorrectness,
		NTrial:                 in.NTrial,
		EvalTimeoutSecs:        in.EvalTimeoutSecs,
		SkipEval:               skipEval,
		MutatorModel:           in.MutatorModel,
		MutatorDryRun:          in.MutatorDryRun,
		MutatorSystemMessage:   in.MutatorSystemMessage,
		MemoryEnabled:          in.MemoryEnabled,
		MemoryDomain:           in.MemoryDomain,
		MemoryTopK:             in.MemoryTopK,
		DeepResearchCollection: in.DeepResearchCollection,
		InsightThreshold:       in.InsightThreshold,
	}

	// Route through DispatchWorkflow so the run lands on whichever
	// worker has declared the `evo` queue and meets the requirements
	// (GPU when skip_eval=false). The EvolutionWorkflow is launched as
	// a child workflow by the dispatcher; the data flow is unchanged
	// because evolve/activities persist programs keyed on RunID
	// regardless of what workflow is hosting them.
	//
	// Note: workflow-level options the CLI sets (WorkflowExecutionTimeout)
	// don't currently propagate through DispatchWorkflow — JobSpec doesn't
	// have a child-options field yet. If that becomes load-bearing we'll
	// add one. For UI-initiated runs the default per-activity timeouts
	// inside EvolutionWorkflow are sufficient.
	payloadBytes, err := json.Marshal(wfIn)
	if err != nil {
		if _, uerr := s.evoPool.Exec(ctx,
			`UPDATE evo.runs SET status = 'failed', finished_at = NOW() WHERE id = $1`, runID); uerr != nil {
			s.logger.WithError(uerr).WithField("run_id", runID).
				Warn("evo run cleanup UPDATE failed after payload marshal error")
		}
		writeEvoErr(w, http.StatusInternalServerError, "marshal payload: "+err.Error())
		return
	}
	jobSpec := dispatchworkflows.JobSpec{
		Kind:    "EvolutionWorkflow",
		Payload: payloadBytes,
		Requirements: dispatchworkflows.Requirements{
			QueueClass:  taskQueue,
			RequiresGPU: !skipEval,
			// MinVRAMGB conservative for kernel eval — 8 GiB is well
			// under a Blackwell's 96 GB headroom and over the L1
			// corpus's actual VRAM footprint.
			MinVRAMGB: vramFloor(skipEval),
		},
		SubmittedBy: "dashboard",
	}
	// Fresh context for ExecuteWorkflow: a client hangup mid-POST must
	// not cancel the workflow start. Once Temporal has accepted the
	// scheduling RPC, the work is owned by Temporal and dropping it
	// because the dashboard's fetch was cancelled would force the
	// caller into a retry loop against a workflow that may already
	// have started.
	wfCtx, wfCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer wfCancel()
	run, err := s.temporalClient.ExecuteWorkflow(wfCtx, client.StartWorkflowOptions{
		ID:        dispatchWorkflowID,
		TaskQueue: dispatchworkflows.TaskQueue,
	}, dispatchworkflows.WorkflowDispatch, jobSpec)
	if err != nil {
		if _, uerr := s.evoPool.Exec(ctx,
			`UPDATE evo.runs SET status = 'failed', finished_at = NOW() WHERE id = $1`,
			runID); uerr != nil {
			s.logger.WithError(uerr).WithField("run_id", runID).
				Warn("evo run cleanup UPDATE failed after ExecuteWorkflow error")
		}
		writeEvoErr(w, http.StatusInternalServerError, "execute workflow: "+err.Error())
		return
	}
	if _, uerr := s.evoPool.Exec(ctx,
		`UPDATE evo.runs SET status = 'running', temporal_run = $2 WHERE id = $1`,
		runID, run.GetRunID()); uerr != nil {
		s.logger.WithError(uerr).WithField("run_id", runID).
			Warn("evo run status UPDATE to running failed (workflow already started)")
	}

	w.WriteHeader(http.StatusCreated)
	writeEvoJSON(w, EvoRunCreateResponse{
		RunID:         runID,
		WorkflowID:    dispatchWorkflowID,
		TemporalRunID: run.GetRunID(),
	})
}
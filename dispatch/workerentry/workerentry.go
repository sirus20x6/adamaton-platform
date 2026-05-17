// Package workerentry wires the dispatch subsystem onto a Temporal
// worker. Both the standalone dispatch-worker binary and the
// umbrella's planned consolidated adamaton-worker call Register(...)
// to install the dispatch workflows and activities.
//
// Lifted from dispatch/cmd/dispatch-worker/registrations.go so the
// registration logic is reachable from other binaries; the
// dispatch-worker shim in cmd/ now bootstraps a pool + Temporal
// client and hands them here.
package workerentry

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	dispatchactivities "github.com/sirus20x6/adamaton-platform/dispatch/activities"
	dispatchworkflows "github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

// Deps groups the dependencies the dispatch registration helpers
// need. The Temporal client is required because the coordinator
// activity calls client.SignalWithStartWorkflow from the activity
// context — the workflow cannot do that directly without breaking
// determinism.
type Deps struct {
	Pool     *pgxpool.Pool
	Temporal client.Client
	Logger   *logrus.Logger
}

// Register installs the dispatch workflows and activities on w.
// Workflow / activity registration calls do not return errors at the
// SDK level, so this function does not either.
func Register(w worker.Worker, d Deps) {
	registerDispatchWorkflows(w)
	registerDispatchActivities(w, d.Pool, d.Temporal, d.Logger)
}

// registerDispatchWorkflows wires DispatchWorkflow + BatchCoordinator
// into the worker under the names declared in workflows/types.go.
// Names must match exactly — the dashboard's POST /api/v1/jobs/submit
// path references those constants when calling client.ExecuteWorkflow,
// and DispatchWorkflow's batched path references WorkflowBatchCoordinator
// when signalling.
func registerDispatchWorkflows(w worker.Worker) {
	w.RegisterWorkflowWithOptions(dispatchworkflows.DispatchWorkflow,
		workflow.RegisterOptions{Name: dispatchworkflows.WorkflowDispatch})
	w.RegisterWorkflowWithOptions(dispatchworkflows.BatchCoordinator,
		workflow.RegisterOptions{Name: dispatchworkflows.WorkflowBatchCoordinator})
}

// registerDispatchActivities constructs the activity structs with
// their injected dependencies and registers each method by the name
// the workflows reference via string constants.
func registerDispatchActivities(w worker.Worker, pool *pgxpool.Pool, c client.Client, logger *logrus.Logger) {
	sel := &dispatchactivities.SelectActivities{Pool: pool, Logger: logger}
	rec := &dispatchactivities.RecordActivities{Pool: pool, Logger: logger}
	coord := &dispatchactivities.CoordinatorActivities{Client: c, Logger: logger}

	w.RegisterActivityWithOptions(sel.SelectCandidates,
		activity.RegisterOptions{Name: "SelectCandidates"})
	w.RegisterActivityWithOptions(rec.RecordJob,
		activity.RegisterOptions{Name: "RecordJob"})
	w.RegisterActivityWithOptions(rec.AssignWorker,
		activity.RegisterOptions{Name: "AssignWorker"})
	w.RegisterActivityWithOptions(rec.MarkJobStatus,
		activity.RegisterOptions{Name: "MarkJobStatus"})
	w.RegisterActivityWithOptions(coord.EnsureBatchCoordinator,
		activity.RegisterOptions{Name: "EnsureBatchCoordinator"})
}

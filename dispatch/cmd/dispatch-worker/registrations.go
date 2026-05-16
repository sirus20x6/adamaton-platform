// Phase 2B registrations for the dispatch worker. Kept in a separate
// file so the Phase 2 agents can land their pieces without conflicting
// on main.go. main.go's registerWorkflows / registerActivities stubs
// delegate here.
package main

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	dispatchactivities "github.com/sirus20x6/adamomaton-platform/dispatch/activities"
	dispatchworkflows "github.com/sirus20x6/adamomaton-platform/dispatch/workflows"
)

// registerDispatchWorkflows wires DispatchWorkflow + BatchCoordinator
// into the worker under the names declared in workflows/types.go. The
// names must match exactly — the dashboard's POST /api/v1/jobs/submit
// path references those constants when calling client.ExecuteWorkflow,
// and DispatchWorkflow's batched path references WorkflowBatchCoordinator
// when signalling.
func registerDispatchWorkflows(w worker.Worker) {
	w.RegisterWorkflowWithOptions(dispatchworkflows.DispatchWorkflow,
		workflow.RegisterOptions{Name: dispatchworkflows.WorkflowDispatch})
	w.RegisterWorkflowWithOptions(dispatchworkflows.BatchCoordinator,
		workflow.RegisterOptions{Name: dispatchworkflows.WorkflowBatchCoordinator})
}

// registerDispatchActivities constructs the activity structs with their
// injected dependencies and registers each method by the name the
// workflows reference via string constants.
//
// The Temporal client is injected into CoordinatorActivities — that
// activity calls client.SignalWithStartWorkflow from the activity
// context (the workflow cannot call it directly without breaking
// determinism) to atomically start a BatchCoordinator and deliver the
// first envelope.
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

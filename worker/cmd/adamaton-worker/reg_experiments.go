//go:build experiments

package main

// Build-tagged registration for the "experiments-train" queue. Add
// `experiments` to GO_TAGS in the worker Dockerfile (or pass via
// `go build -tags experiments`) to compile this file into the unified
// adamaton-worker binary. Without the tag the queue is absent from
// queueRegistry and selecting it via --queue fails with the standard
// "available: [...]" message.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	"github.com/sirus20x6/adamaton-platform/temporal/activities"
	trainwf "github.com/sirus20x6/adamaton-platform/temporal/workflows"
	"github.com/sirus20x6/adamaton-platform/worker/internal/coreboot"
)

func init() {
	queueRegistry["experiments"] = queueDef{
		Identity:         "experiments-worker",
		DefaultTaskQueue: trainwf.TaskQueueTrainExperiment,
		Setup:            setupExperiments,
	}
}

func setupExperiments(b *bootCtx) (*queueRuntime, error) {
	// The launch queue is useless without the experiments tables, so
	// require an explicit DSN instead of silently falling back to a
	// localhost default that doesn't exist inside the worker
	// containers. s6 restarts the process, so failing fast here turns
	// a misconfigured deployment into a visible crash loop rather than
	// a worker that polls Temporal but errors every activity.
	dsn := coreboot.ResolveDSN([]string{"EVO_POSTGRES_DSN", "POSTGRES_DSN"}, "")
	if dsn == "" {
		return nil, fmt.Errorf("experiments queue requires EVO_POSTGRES_DSN or POSTGRES_DSN to be set")
	}
	pool, err := pgxpool.New(b.Ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open evo postgres pool: %w", err)
	}
	// pgxpool.New is lazy; verify the DSN actually reaches a database
	// before registering with Temporal.
	if err := pool.Ping(b.Ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping experiments postgres: %w", err)
	}

	// Modest concurrency. Dispatch activities are long-lived (training
	// runs are hours) so allowing many in parallel would starve the
	// GPU queue on blackwell; cap at 2 so a single misconfigured
	// queue doesn't pin every GPU slot.
	w := worker.New(b.Temporal, b.TaskQueue, worker.Options{
		Identity:                               "experiments-worker",
		MaxConcurrentActivityExecutionSize:     2,
		MaxConcurrentWorkflowTaskExecutionSize: 8,
	})

	deps := &activities.ExperimentDeps{Pool: pool}
	w.RegisterWorkflow(trainwf.TrainExperimentWorkflow)
	w.RegisterActivity(deps)

	sess, regErr := workerregistry.Register(b.Ctx, pool, workerregistry.RegisterOptions{
		Identity:       "experiments-worker",
		DeclaredQueues: []string{b.TaskQueue},
		Logger:         b.Logger,
	})
	if regErr != nil {
		b.Logger.WithError(regErr).Warn("worker self-registration failed; worker continues without a workers row")
	}

	cleanup := func(ctx context.Context) {
		if sess != nil {
			_ = sess.Close(ctx)
		}
		pool.Close()
	}

	return &queueRuntime{Worker: w, Cleanup: cleanup}, nil
}

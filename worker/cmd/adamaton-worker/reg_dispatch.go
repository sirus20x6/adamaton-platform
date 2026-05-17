//go:build dispatch

package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	dispatchentry "github.com/sirus20x6/adamaton-platform/dispatch/workerentry"
	dispatchworkflows "github.com/sirus20x6/adamaton-platform/dispatch/workflows"
	"github.com/sirus20x6/adamaton-platform/worker/internal/coreboot"
)

func init() {
	queueRegistry["dispatch"] = queueDef{
		Identity:         "dispatch-worker",
		DefaultTaskQueue: dispatchworkflows.TaskQueue,
		Setup:            setupDispatch,
	}
}

func setupDispatch(b *bootCtx) (*queueRuntime, error) {
	dsn := coreboot.ResolveDSN(
		[]string{"EVO_POSTGRES_DSN", "POSTGRES_DSN"},
		"postgres://postgres@localhost:5432/postgres?sslmode=disable",
	)
	pool, err := pgxpool.New(b.Ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open evo postgres pool: %w", err)
	}

	w := worker.New(b.Temporal, b.TaskQueue, worker.Options{
		Identity:                               "dispatch-worker",
		MaxConcurrentActivityExecutionSize:     8,
		MaxConcurrentWorkflowTaskExecutionSize: 16,
	})

	dispatchentry.Register(w, dispatchentry.Deps{
		Pool:     pool,
		Temporal: b.Temporal,
		Logger:   b.Logger,
	})

	sess, regErr := workerregistry.Register(b.Ctx, pool, workerregistry.RegisterOptions{
		Identity:       "dispatch-worker",
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

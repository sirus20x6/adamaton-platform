package main

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamaton-platform/worker/internal/coreboot"
)

// bootCtx is the shared bootstrap context every reg_*.go setup
// function receives. It carries the shutdown context, the logger,
// and the Temporal client (one per process — each queue runs in its
// own OS process under s6, so sharing across queues isn't a concern).
type bootCtx struct {
	Ctx      context.Context
	Logger   *logrus.Logger
	Temporal client.Client

	// TaskQueue is the queue name the worker should poll. Resolved
	// from the queue's per-queue env var or its DefaultTaskQueue
	// constant before bootCtx is handed to Setup.
	TaskQueue string
}

// newBootCtx dials Temporal with retry and returns a ready bootCtx.
// Logger is constructed by the caller (main) so log fields stay
// consistent across the bootstrap log line and per-queue logs.
func newBootCtx(ctx context.Context, logger *logrus.Logger, identity string) (*bootCtx, error) {
	addr := coreboot.EnvOr("TEMPORAL_ADDRESS", "localhost:7233")
	ns := coreboot.EnvOr("TEMPORAL_NAMESPACE", "default")
	c, err := coreboot.DialTemporalWithRetry(ctx, addr, ns, identity, logger)
	if err != nil {
		return nil, err
	}
	return &bootCtx{Ctx: ctx, Logger: logger, Temporal: c}, nil
}

// Close releases the Temporal client. Per-queue resources (pgxpool,
// workerregistry session) are released by queueRuntime.Cleanup.
func (b *bootCtx) Close() {
	if b.Temporal != nil {
		b.Temporal.Close()
	}
}

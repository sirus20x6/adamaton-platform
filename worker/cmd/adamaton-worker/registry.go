package main

import (
	"context"
	"sort"

	"go.temporal.io/sdk/worker"
)

// queueDef describes one Temporal task queue this binary knows how to
// host. Entries are added by init() funcs in reg_*.go files; which
// reg_*.go files compile in depends on the GO_TAGS the binary was
// built with. A queue that is not in queueRegistry at runtime is
// either unknown (typo) or excluded from this build variant.
type queueDef struct {
	// Identity is what we send to Temporal as worker.Options.Identity
	// and as workerregistry.RegisterOptions.Identity. Kept stable across
	// the consolidation so /nodes UI rows for skills-worker /
	// reindex-worker:ingest / etc. don't move.
	Identity string

	// DefaultTaskQueue is the workflow-package constant used when no
	// per-queue env override is set. Per-queue env var is read by the
	// Setup func, not by main, so each queue can use whichever name
	// the standalone worker historically supported.
	DefaultTaskQueue string

	// Setup opens any per-queue resources (pgxpool, HTTP client),
	// constructs the Temporal worker, registers its workflows and
	// activities, and self-registers into core/workerregistry. The
	// returned queueRuntime is run by main and torn down via Cleanup.
	Setup func(b *bootCtx) (*queueRuntime, error)
}

// queueRuntime is what main runs after Setup. Worker.Run blocks until
// SIGTERM; Cleanup closes the pgxpool and the workerregistry session.
type queueRuntime struct {
	Worker  worker.Worker
	Cleanup func(ctx context.Context)
}

// queueRegistry is populated by init() in each build-tagged reg_*.go
// file. A queue absent from this map at the time main runs is either
// unknown (typo) or compiled out of this build variant — main reports
// both cases the same way: a fatal "available: [...]" listing.
var queueRegistry = map[string]queueDef{}

// knownQueues returns the sorted list of queue names compiled into
// this binary. Used in the fatal error message when --queue selects
// something unknown.
func knownQueues() []string {
	out := make([]string, 0, len(queueRegistry))
	for k := range queueRegistry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

//go:build experiments

package main

// Compile-and-run with: go test -tags experiments ./worker/cmd/adamaton-worker
// Without the tag this file (like reg_experiments.go) is excluded, so the
// assertions below verify the build-tag wiring itself: an image built with
// GO_TAGS containing `experiments` (or `evo`, via the Dockerfile expansion)
// gets the experiments-train launch queue in its registry.

import (
	"context"
	"strings"
	"testing"

	trainwf "github.com/sirus20x6/adamaton-platform/temporal/workflows"
)

// TestExperimentsQueueRegistered asserts the experiments build tag wires
// the launch queue into queueRegistry with the canonical task queue name.
func TestExperimentsQueueRegistered(t *testing.T) {
	def, ok := queueRegistry["experiments"]
	if !ok {
		t.Fatalf("queueRegistry missing %q; known queues: %v", "experiments", knownQueues())
	}
	if def.DefaultTaskQueue != trainwf.TaskQueueTrainExperiment {
		t.Errorf("DefaultTaskQueue = %q, want %q", def.DefaultTaskQueue, trainwf.TaskQueueTrainExperiment)
	}
	if def.DefaultTaskQueue != "experiments-train" {
		t.Errorf("DefaultTaskQueue = %q, want the literal %q", def.DefaultTaskQueue, "experiments-train")
	}
	if def.Identity != "experiments-worker" {
		t.Errorf("Identity = %q, want %q", def.Identity, "experiments-worker")
	}
	if def.Setup == nil {
		t.Error("Setup func is nil")
	}
}

// TestExperimentsSetupRequiresDSN asserts setup fails fast (rather than
// falling back to a nonexistent localhost default) when no Postgres DSN
// is configured — the deps-present gate for registering the queue.
func TestExperimentsSetupRequiresDSN(t *testing.T) {
	t.Setenv("EVO_POSTGRES_DSN", "")
	t.Setenv("POSTGRES_DSN", "")
	b := &bootCtx{Ctx: context.Background()}
	rt, err := setupExperiments(b)
	if err == nil {
		if rt != nil && rt.Cleanup != nil {
			rt.Cleanup(context.Background())
		}
		t.Fatal("setupExperiments succeeded without a DSN; want fail-fast error")
	}
	if !strings.Contains(err.Error(), "EVO_POSTGRES_DSN") {
		t.Errorf("error %q does not mention the required env vars", err)
	}
}

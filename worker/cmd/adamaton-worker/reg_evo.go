//go:build evo

package main

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/octen"
	"github.com/sirus20x6/adamaton-core/workerregistry"
	"github.com/sirus20x6/adamaton-evolve/evolve/store"
	evoentry "github.com/sirus20x6/adamaton-evolve/evolve/workerentry"
	"github.com/sirus20x6/adamaton-platform/worker/internal/coreboot"
)

const (
	defaultEvoEvaluatorDir = "/thearray/git/evo/evolve/evaluator/kernel"
	defaultEvoPython       = "python3"
	defaultEvoOpenCode     = "/home/sirus/.opencode/bin/opencode"
	defaultEvoModel        = "anthropic/claude-opus-4"
	defaultEvoTaskQueue    = "evo"
)

func init() {
	queueRegistry["evo"] = queueDef{
		Identity:         "evo-worker",
		DefaultTaskQueue: defaultEvoTaskQueue,
		Setup:            setupEvo,
	}
}

func setupEvo(b *bootCtx) (*queueRuntime, error) {
	dsn := coreboot.ResolveDSN(
		[]string{"POSTGRES_DSN"},
		"postgres://postgres@localhost:5432/postgres?sslmode=disable",
	)
	st, err := store.Open(b.Ctx, dsn, b.Logger)
	if err != nil {
		return nil, fmt.Errorf("open evo store: %w", err)
	}

	evalDir := coreboot.EnvOr("EVO_EVALUATOR_DIR", defaultEvoEvaluatorDir)
	pythonBin := coreboot.EnvOr("EVO_PYTHON_BIN", defaultEvoPython)
	// Dual-target switch — when set, the kernel evaluator subprocess
	// runs over SSH on the named host instead of locally. A Pi-side
	// worker sets this to the workstation's hostname; blackwell runs
	// it empty (local).
	kernelRemote := coreboot.EnvOr("EVO_KERNEL_REMOTE_HOST", "")
	opencodeBin := coreboot.EnvOr("EVO_OPENCODE_BIN", defaultEvoOpenCode)
	mutateModel := coreboot.EnvOr("EVO_MUTATE_MODEL", defaultEvoModel)

	octenURL := coreboot.EnvOr("OCTEN_URL", "")
	octenModel := coreboot.EnvOr("OCTEN_MODEL", "octen-embedding-0.6b")
	octenClient := octen.NewClient(octenURL, octenModel, b.Logger)
	drURL := coreboot.EnvOr("DEEPRESEARCH_URL", "")

	w := worker.New(b.Temporal, b.TaskQueue, worker.Options{Identity: "evo-worker"})

	evoentry.Register(w, evoentry.Deps{
		Store:        st,
		Octen:        octenClient,
		Logger:       b.Logger,
		EvaluatorDir: evalDir,
		PythonBin:    pythonBin,
		KernelRemote: kernelRemote,
		OpenCodeBin:  opencodeBin,
		MutateModel:  mutateModel,
		DRBaseURL:    drURL,
	})

	sess, regErr := workerregistry.Register(b.Ctx, st.Pool, workerregistry.RegisterOptions{
		Identity:       "evo-worker",
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
		st.Close()
	}

	return &queueRuntime{Worker: w, Cleanup: cleanup}, nil
}

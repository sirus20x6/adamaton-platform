//go:build skills

package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	skillsentry "github.com/sirus20x6/adamaton-knowledge/skills/workerentry"
	skillsworkflows "github.com/sirus20x6/adamaton-knowledge/skills/workflows"
	"github.com/sirus20x6/adamaton-platform/worker/internal/coreboot"
)

func init() {
	queueRegistry["skills"] = queueDef{
		Identity:         "skills-worker",
		DefaultTaskQueue: skillsworkflows.TaskQueue,
		Setup:            setupSkills,
	}
}

func setupSkills(b *bootCtx) (*queueRuntime, error) {
	dsn := coreboot.ResolveDSN(
		[]string{"EVO_POSTGRES_DSN", "POSTGRES_DSN"},
		"postgres://postgres@localhost:5432/postgres?sslmode=disable",
	)
	pool, err := pgxpool.New(b.Ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open evo postgres pool: %w", err)
	}

	// HTTP client tuned for the longest call (R2R ingest at 90s).
	// MaxIdleConnsPerHost=10 caps the keep-alive pool that activities
	// fan out across api.github.com, raw.githubusercontent.com, and
	// the R2R host concurrently.
	httpClient := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	w := worker.New(b.Temporal, b.TaskQueue, worker.Options{
		Identity:                               "skills-worker",
		MaxConcurrentActivityExecutionSize:     8,
		MaxConcurrentWorkflowTaskExecutionSize: 8,
	})

	skillsentry.Register(b.Ctx, w, skillsentry.Deps{
		Pool:       pool,
		HTTPClient: httpClient,
		Logger:     b.Logger,
		Temporal:   b.Temporal,
		TaskQueue:  b.TaskQueue,
	})

	sess, regErr := workerregistry.Register(b.Ctx, pool, workerregistry.RegisterOptions{
		Identity:       "skills-worker",
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

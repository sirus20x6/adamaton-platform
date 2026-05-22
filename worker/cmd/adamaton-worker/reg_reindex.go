//go:build reindex

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	reindexentry "github.com/sirus20x6/adamaton-knowledge/reindex/workerentry"
	reindexworkflows "github.com/sirus20x6/adamaton-knowledge/reindex/workflows"
	"github.com/sirus20x6/adamaton-platform/worker/internal/coreboot"
)

// reindex is split into TWO queueRegistry entries — one per Temporal
// task queue — so under s6 each runs in its own OS process. The old
// standalone reindex-worker hosted both in one process and shared a
// pgxpool / HTTP client across them; here each process opens its own.
// The per-process cost is small (a handful of postgres conns and one
// http.Transport) and worth it for crash isolation.
func init() {
	queueRegistry["reindex-ingest"] = queueDef{
		Identity:         "reindex-worker:ingest",
		DefaultTaskQueue: reindexworkflows.TaskQueueIngest,
		Setup:            setupReindexIngest,
	}
	queueRegistry["reindex-extract"] = queueDef{
		Identity:         "reindex-worker:extract",
		DefaultTaskQueue: reindexworkflows.TaskQueueExtract,
		Setup:            setupReindexExtract,
	}
}

func setupReindexIngest(b *bootCtx) (*queueRuntime, error) {
	return setupReindexQueue(b, "ingest")
}

func setupReindexExtract(b *bootCtx) (*queueRuntime, error) {
	return setupReindexQueue(b, "extract")
}

func setupReindexQueue(b *bootCtx, side string) (*queueRuntime, error) {
	dsn := coreboot.ResolveDSN(
		[]string{"REINDEX_POSTGRES_DSN", "DEEPRESEARCH_POSTGRES_DSN"},
		"postgres://postgres@localhost:5433/deepresearch?sslmode=disable",
	)
	pool, err := pgxpool.New(b.Ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open deepresearch postgres pool: %w", err)
	}

	platformURL := coreboot.EnvOr("PLATFORM_BASE_URL", "https://deepresearch.local")
	r2gURL := os.Getenv("R2G_URL")
	arxivSkipURL := os.Getenv("ARXIV_SKIP_URL")
	// Embedding sidecar for the ingest-side ChunkEmbedPersist activity.
	// Prefer the native llama-server endpoint (BGE_EMBED_URL); fall back to
	// the legacy Python sidecar (BGE_SIDECAR_URL) during the migration.
	embedURL := coreboot.EnvOr("BGE_EMBED_URL", os.Getenv("BGE_SIDECAR_URL"))
	embedModel := coreboot.EnvOr("REINDEX_EMBED_MODEL", "bge-m3")

	concurrencyEnv := "REINDEX_INGEST_CONCURRENCY"
	identity := "reindex-worker:ingest"
	if side == "extract" {
		concurrencyEnv = "REINDEX_EXTRACT_CONCURRENCY"
		identity = "reindex-worker:extract"
	}
	concurrency := coreboot.EnvInt(concurrencyEnv, 4)

	// 20-minute ceiling sized for ExtractEntities (LLM-driven graph
	// extraction under R2R's "simple" orchestration). Shorter
	// activities cap themselves via Temporal's StartToCloseTimeout.
	httpClient := &http.Client{
		Timeout: 20 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	w := worker.New(b.Temporal, b.TaskQueue, worker.Options{
		Identity:                               identity,
		MaxConcurrentActivityExecutionSize:     concurrency,
		MaxConcurrentWorkflowTaskExecutionSize: concurrency,
	})

	deps := reindexentry.Deps{
		Pool:         pool,
		HTTPClient:   httpClient,
		Logger:       b.Logger,
		PlatformURL:  platformURL,
		R2GURL:       r2gURL,
		ArxivSkipURL: arxivSkipURL,
		EmbedURL:     embedURL,
		EmbedModel:   embedModel,
	}
	if side == "ingest" {
		reindexentry.RegisterIngest(w, deps)
	} else {
		reindexentry.RegisterExtract(w, deps)
	}

	sess, regErr := workerregistry.Register(b.Ctx, pool, workerregistry.RegisterOptions{
		Identity:       identity,
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

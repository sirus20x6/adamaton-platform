// dispatch-worker is the Temporal worker for the dispatch subsystem.
// Polls the "dispatch" task queue (workflows.TaskQueue) and runs
// DispatchWorkflow + BatchCoordinator. Every other worker in the fleet
// (skills, reindex, evo, pr-review) registers itself into evo.workers
// at startup; this worker reads that registry to route incoming jobs.
//
// Deployed on the Pi via docker-compose alongside the other workers.
// Reads Postgres + Temporal addresses from env; defaults match the
// Pi-side service hostnames.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/sirus20x6/adamaton-core/workerregistry"
	"github.com/sirus20x6/adamaton-platform/dispatch/workerentry"
	dispatchworkflows "github.com/sirus20x6/adamaton-platform/dispatch/workflows"
)

const (
	defaultDSN       = "postgres://postgres@localhost:5432/postgres?sslmode=disable"
	defaultTemporal  = "localhost:7233"
	defaultNamespace = "default"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	dsn := envOr("EVO_POSTGRES_DSN", envOr("POSTGRES_DSN", defaultDSN))
	temporalAddr := envOr("TEMPORAL_ADDRESS", defaultTemporal)
	namespace := envOr("TEMPORAL_NAMESPACE", defaultNamespace)
	taskQueue := envOr("DISPATCH_TASK_QUEUE", dispatchworkflows.TaskQueue)

	logger.WithFields(logrus.Fields{
		"temporal":  temporalAddr,
		"namespace": namespace,
		"queue":     taskQueue,
		"dsn":       redactDSN(dsn),
	}).Info("dispatch-worker starting")

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("open evo postgres pool: %v", err)
	}
	defer pool.Close()

	c, err := dialTemporalWithRetry(ctx, temporalAddr, namespace, logger)
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	w := worker.New(c, taskQueue, worker.Options{
		Identity:                               "dispatch-worker",
		MaxConcurrentActivityExecutionSize:     8,
		MaxConcurrentWorkflowTaskExecutionSize: 16,
	})

	// Workflow + activity registration lives in dispatch/workerentry
	// so the umbrella's consolidated adamaton-worker can install the
	// same set without duplicating the registration code.
	workerentry.Register(w, workerentry.Deps{
		Pool:     pool,
		Temporal: c,
		Logger:   logger,
	})

	// Self-registration into evo.workers so the dispatcher shows up on
	// the /nodes page.
	if sess, err := registerSelf(ctx, pool, taskQueue, logger); err != nil {
		logger.WithError(err).Warn("self-registration failed; dispatch-worker continues without a workers row")
	} else if sess != nil {
		defer sess.Close(context.Background())
	}

	logger.Info("dispatch-worker ready")
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker.Run: %v", err)
	}
	logger.Info("dispatch-worker stopped")
}

// registerSelf inserts the dispatch-worker's row into evo.workers and
// starts the heartbeat goroutine via core/workerregistry. The returned
// Session marks the worker offline when Close is called.
func registerSelf(ctx context.Context, pool *pgxpool.Pool, taskQueue string, logger *logrus.Logger) (interface {
	Close(context.Context) error
}, error) {
	sess, err := workerregistry.Register(ctx, pool, workerregistry.RegisterOptions{
		Identity:       "dispatch-worker",
		DeclaredQueues: []string{taskQueue},
		Logger:         logger,
	})
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func redactDSN(dsn string) string {
	at := -1
	colon := -1
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			at = i
			break
		}
		if dsn[i] == ':' && i > 11 {
			colon = i
		}
	}
	if at < 0 || colon < 0 || colon >= at {
		return dsn
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}

func dialTemporalWithRetry(ctx context.Context, addr, ns string, logger *logrus.Logger) (client.Client, error) {
	const (
		base    = 5 * time.Second
		maxStep = 6
	)
	var attempts atomic.Int32
	for {
		c, err := client.Dial(client.Options{
			HostPort:  addr,
			Namespace: ns,
			Identity:  "dispatch-worker",
		})
		if err == nil {
			if a := attempts.Load(); a > 0 {
				logger.WithField("attempts", a).Info("temporal connection established after retries")
			}
			return c, nil
		}
		a := attempts.Add(1)
		step := int32(a)
		if step > maxStep {
			step = maxStep
		}
		wait := base * time.Duration(step)
		logger.WithError(err).Warnf("temporal dial failed; retrying in %s", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

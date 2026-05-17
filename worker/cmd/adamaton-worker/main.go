// adamaton-worker is the umbrella's unified Temporal worker. One
// binary, one container, N processes — each process is the same
// binary invoked with --queue=<name>, supervised by s6-overlay
// inside the container so a crash in one queue restarts only that
// queue.
//
// Which queues are available is determined at BUILD TIME by Go build
// tags (skills, reindex, dispatch, evo). Which subset of compiled-in
// queues actually run is determined at RUNTIME by the WORKER_QUEUES
// env var (consumed by the s6 init script, which spawns one
// adamaton-worker --queue=<q> process per entry).
//
// Variants published from this source tree:
//
//	light  -tags="skills reindex dispatch"           — Pi5 default
//	full   -tags="skills reindex dispatch evo"       — Pi5 + evo
//	gpu    -tags="evo"                               — blackwell
//
// Each invocation runs exactly one queue. Cross-queue isolation is
// the s6 supervisor's job, not Go's.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	queueFlag := flag.String("queue", "", "task queue to host (e.g. skills, reindex-ingest, reindex-extract, dispatch, evo). Falls back to $WORKER_QUEUE.")
	flag.Parse()

	queue := strings.TrimSpace(*queueFlag)
	if queue == "" {
		queue = strings.TrimSpace(os.Getenv("WORKER_QUEUE"))
	}
	if queue == "" {
		fmt.Fprintf(os.Stderr, "adamaton-worker: --queue or $WORKER_QUEUE required; compiled-in queues: %s\n", strings.Join(knownQueues(), ", "))
		os.Exit(2)
	}

	def, ok := queueRegistry[queue]
	if !ok {
		fmt.Fprintf(os.Stderr, "adamaton-worker: queue %q is not compiled into this variant; available: %s\n", queue, strings.Join(knownQueues(), ", "))
		os.Exit(2)
	}

	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	b, err := newBootCtx(ctx, logger, def.Identity)
	if err != nil {
		log.Fatalf("adamaton-worker: temporal dial: %v", err)
	}
	defer b.Close()

	b.TaskQueue = resolveTaskQueue(queue, def)

	rt, err := def.Setup(b)
	if err != nil {
		log.Fatalf("adamaton-worker: setup %s: %v", queue, err)
	}
	defer rt.Cleanup(context.Background())

	logger.WithFields(logrus.Fields{
		"queue":      queue,
		"task_queue": b.TaskQueue,
		"identity":   def.Identity,
	}).Info("adamaton-worker ready")

	if err := rt.Worker.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("adamaton-worker: worker.Run: %v", err)
	}
	logger.WithField("queue", queue).Info("adamaton-worker stopped")
}

// resolveTaskQueue lets operators override the per-queue Temporal
// task-queue name via env. Names: SKILLS_TASK_QUEUE,
// REINDEX_INGEST_TASK_QUEUE, REINDEX_EXTRACT_TASK_QUEUE,
// DISPATCH_TASK_QUEUE, EVO_TASK_QUEUE. Falls back to the
// workflow-package constant.
func resolveTaskQueue(queue string, def queueDef) string {
	envVar := strings.ToUpper(strings.ReplaceAll(queue, "-", "_")) + "_TASK_QUEUE"
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return def.DefaultTaskQueue
}

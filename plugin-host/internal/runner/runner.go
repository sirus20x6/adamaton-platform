// Package runner closes the loop between an HTTP request marking a
// run 'pending' and the plugin subprocess actually doing the work.
//
// The HTTP routes (compat_zotero, /platform/plugins/{id}/run, the future
// importer-tab UI) all just write a platform.plugin_runs row and return.
// This package polls that table, atomically claims pending rows
// (SELECT ... FOR UPDATE SKIP LOCKED), calls the supervisor to ensure
// the plugin is running, and drains the Plugin.Sync server-streaming
// gRPC for SyncEvent messages.
//
// Per event:
//   - Item:     persist into platform.plugin_items (UPSERT on the
//               (plugin_id, external_id) unique index).
//   - Progress: log.
//   - Error:    append to run.error; on fatal, stop the stream early.
//   - Summary:  capture final totals.
//
// When the stream closes the run is moved to 'succeeded' or 'failed'
// with the totals + error fields populated. Worker count and poll
// interval are configurable; defaults are tuned for the Pi (one
// concurrent run, 5s poll).
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/persist"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/supervisor"
)

// Options is the wide constructor.
type Options struct {
	Logger       *logrus.Logger
	Persist      *persist.Store
	Supervisor   *supervisor.Supervisor
	PollInterval time.Duration // default 5s
	// SyncTimeout caps a single Plugin.Sync stream. Large library syncs
	// (Zotero with 10k items) can take a while; the timeout is mostly
	// to keep a wedged plugin from holding the worker forever.
	SyncTimeout time.Duration // default 30m
}

// Runner is the polling worker. One instance per process is enough for
// the Pi; the SKIP LOCKED claim semantics make it safe to scale.
type Runner struct {
	opts Options
}

func New(opts Options) *Runner {
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.SyncTimeout == 0 {
		opts.SyncTimeout = 30 * time.Minute
	}
	return &Runner{opts: opts}
}

// Start blocks until ctx is canceled.
func (r *Runner) Start(ctx context.Context) error {
	t := time.NewTicker(r.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := r.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.opts.Logger.WithError(err).Warn("runner tick")
			}
		}
	}
}

// tick claims at most one pending run per pass. Keeping it serial means
// one plugin process per Pi at a time, which fits the 4 GB RAM budget --
// raising parallelism would need a per-plugin RAM allowance and a
// scheduler that respects it.
func (r *Runner) tick(ctx context.Context) error {
	runID, pluginID, args, err := r.opts.Persist.PickPendingRun(ctx)
	if err != nil {
		return fmt.Errorf("pick pending: %w", err)
	}
	if runID == "" {
		return nil // queue empty
	}
	r.opts.Logger.WithFields(logrus.Fields{
		"run_id":    runID,
		"plugin_id": pluginID,
	}).Info("runner: claimed pending run")
	r.execute(ctx, runID, pluginID, args)
	return nil
}

// execute drives one run end-to-end. Errors are recorded on the run row;
// the function never returns them because the caller already moved the
// row out of 'pending' and just needs to know to keep polling.
func (r *Runner) execute(parentCtx context.Context, runID, pluginID string, args map[string]any) {
	ctx, cancel := context.WithTimeout(parentCtx, r.opts.SyncTimeout)
	defer cancel()
	log := r.opts.Logger.WithFields(logrus.Fields{
		"run_id":    runID,
		"plugin_id": pluginID,
	})

	client, _, err := r.opts.Supervisor.EnsureRunning(ctx, pluginID)
	if err != nil {
		r.finish(parentCtx, runID, "failed", nil, fmt.Sprintf("spawn plugin: %v", err))
		log.WithError(err).Warn("runner: spawn failed")
		return
	}

	syncReq := &pluginv1.SyncRequest{
		RunId:        runID,
		CollectionId: strFrom(args, "collection_id"),
		Since:        strFrom(args, "since"),
		CorpusId:     strFrom(args, "corpus_id"),
		Options:      optionsToStruct(args),
	}
	stream, err := client.Sync(ctx, syncReq)
	if err != nil {
		r.finish(parentCtx, runID, "failed", nil, fmt.Sprintf("open sync stream: %v", err))
		log.WithError(err).Warn("runner: open sync stream failed")
		return
	}

	totals := map[string]int64{}
	var firstErr string
	for {
		evt, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Stream-level RPC failure -- record + bail.
			if firstErr == "" {
				firstErr = fmt.Sprintf("stream recv: %v", err)
			}
			break
		}
		switch e := evt.Event.(type) {
		case *pluginv1.SyncEvent_Item:
			totals["new_items"]++
			if err := r.opts.Persist.InsertPluginItem(ctx, runID, e.Item); err != nil {
				log.WithError(err).WithField("external_id", e.Item.GetExternalId()).
					Warn("runner: persist item failed")
				totals["errored"]++
			}
		case *pluginv1.SyncEvent_Progress:
			log.WithFields(logrus.Fields{
				"message": e.Progress.GetMessage(),
				"seen":    e.Progress.GetSeen(),
			}).Debug("runner: progress")
		case *pluginv1.SyncEvent_Error:
			if firstErr == "" {
				firstErr = fmt.Sprintf("[%s] %s", e.Error.GetCode(), e.Error.GetMessage())
			}
			if e.Error.GetFatal() {
				log.WithField("code", e.Error.GetCode()).Warn("runner: fatal error from plugin")
				goto closeStream
			}
		case *pluginv1.SyncEvent_Summary:
			s := e.Summary
			// Plugin-reported totals override our per-event tally only
			// for the fields the plugin actually populated -- it's
			// authoritative on its own bookkeeping. We keep our own
			// new_items count as the floor (the plugin's may be lower
			// if it errored before its final summary).
			if v := s.GetSeen(); v > 0 {
				totals["seen"] = v
			}
			if v := s.GetNewItems(); v > totals["new_items"] {
				totals["new_items"] = v
			}
			if v := s.GetFetched(); v > 0 {
				totals["fetched"] = v
			}
			if v := s.GetDeduped(); v > 0 {
				totals["deduped"] = v
			}
			if v := s.GetQueued(); v > 0 {
				totals["queued"] = v
			}
			if v := s.GetErrored(); v > 0 {
				totals["errored"] = v
			}
			if v := s.GetSkipped(); v > 0 {
				totals["skipped"] = v
			}
		}
	}
closeStream:

	finalStatus := "succeeded"
	if firstErr != "" {
		finalStatus = "failed"
	}
	r.finish(parentCtx, runID, finalStatus, totals, firstErr)
	log.WithFields(logrus.Fields{
		"status": finalStatus,
		"totals": totals,
	}).Info("runner: run finished")
}

// finish closes the run with whatever final state we got. Uses
// parentCtx (not the per-call ctx that may have expired) so the
// timestamp lands even after a sync timeout.
func (r *Runner) finish(parentCtx context.Context, runID, status string, totals map[string]int64, errMsg string) {
	finCtx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancel()
	if err := r.opts.Persist.UpdateRunFinished(finCtx, runID, status, totals, errMsg); err != nil {
		r.opts.Logger.WithError(err).WithField("run_id", runID).
			Warn("runner: UpdateRunFinished failed")
	}
}

// strFrom is the args[k] -> string helper that defaults to "" without
// breaking on type mismatches.
func strFrom(args map[string]any, k string) string {
	v, ok := args[k]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// optionsToStruct lifts the args bag into a protobuf Struct that the
// plugin receives as SyncRequest.Options. We pass the FULL args bag --
// plugins decide which keys they care about (zotero reads source +
// sqlite_path + ..., search plugins read q etc.).
func optionsToStruct(args map[string]any) *structpb.Struct {
	if len(args) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(args)
	if err != nil {
		return nil
	}
	return s
}

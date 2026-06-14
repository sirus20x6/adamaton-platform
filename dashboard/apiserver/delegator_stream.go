package apiserver

// Live delegation output streaming (SSE).
//
// This is the consumer half of the delegator live-stream feature: the
// delegator orchestrator publishes a running delegation's stdout/stderr as
// chunks over Postgres NOTIFY on `delegator_task_<id>` (see
// delegator.PgTaskEvents); this handler holds a dedicated pool connection
// in LISTEN mode and forwards each chunk to the browser as an SSE event,
// then auto-closes when the task reaches a terminal status.
//
// Why it lives in the (otherwise deprecated) dashboard apiserver: LISTEN
// only receives NOTIFYs from the SAME database, and the apiserver is the
// only service that already holds the delegator task-store pool
// (s.delegatorStore). The intended future home — a deepresearch platform
// backend — does not exist as a Go service yet, and would in any case need
// its own pool on the delegator database. New, self-contained file (the
// deprecated delegator_endpoints.go is left untouched); registration is in
// server.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/sirus20x6/adamaton-delegator/delegator"
)

// setupDelegatorStreamRoutes registers the SSE tail endpoint:
//
//	GET /api/v1/delegator/tasks/{id}/stream
func (s *APIServer) setupDelegatorStreamRoutes(api *mux.Router) {
	api.HandleFunc("/delegator/tasks/{id}/stream", s.handleDelegatorTaskStream).Methods(http.MethodGet)
}

func (s *APIServer) handleDelegatorTaskStream(w http.ResponseWriter, r *http.Request) {
	if s.delegatorStore == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error: "delegator task store unavailable (postgres.dsn not configured?)", Success: false,
		})
		return
	}
	taskID := mux.Vars(r)["id"]
	if taskID == "" {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{Error: "missing task id", Success: false})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{Error: "streaming unsupported", Success: false})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat proxy buffering (Caddy/nginx)
	w.WriteHeader(http.StatusOK)

	// Bound the stream so a forgotten tab can't pin a pool connection in
	// LISTEN mode forever. A delegation is capped at 30m wall-clock, so 35m
	// outlives any real task.
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Minute)
	defer cancel()

	// All writes to the HTTP response happen on this goroutine via `out`;
	// SSE is single-writer per connection.
	type sseMsg struct{ event, data string }
	out := make(chan sseMsg, 64)
	send := func(event, data string) {
		select {
		case out <- sseMsg{event, data}:
		case <-ctx.Done():
		}
	}
	send("hello", fmt.Sprintf(`{"task_id":%q}`, taskID))

	// LISTEN consumer on a dedicated pooled connection, held for the life
	// of the stream so the registration persists.
	channel := delegator.TaskChannel(taskID)
	notifyCh := make(chan string, 64)
	go func() {
		conn, err := s.delegatorStore.Pool().Acquire(ctx)
		if err != nil {
			s.logger.WithError(err).Debug("delegator stream: acquire failed; poll-only")
			return
		}
		defer conn.Release()
		// channel is sanitized to [a-z0-9_] by delegator.TaskChannel, so
		// it's a safe unquoted identifier — no injection surface.
		if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
			s.logger.WithError(err).WithField("channel", channel).
				Debug("delegator stream: LISTEN failed; poll-only")
			return
		}
		pgxConn := conn.Conn()
		for {
			n, err := pgxConn.WaitForNotification(ctx)
			if err != nil {
				return // ctx cancelled or conn died; poll loop handles termination
			}
			if n == nil {
				continue
			}
			select {
			case notifyCh <- n.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Terminal-status poller. The producer only NOTIFYs chunks, not
	// completion, so termination is detected here (also catches a task that
	// is already terminal when the client connects, and out-of-band status
	// changes). Reuses PgStore.Get rather than raw SQL.
	type pollResult struct {
		status delegator.TaskStatus
		found  bool
	}
	pollCh := make(chan pollResult, 4)
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				t, ok := s.delegatorStore.Get(taskID)
				st := delegator.TaskStatus("")
				if ok {
					st = t.Status
				}
				select {
				case pollCh <- pollResult{status: st, found: ok}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	done := false
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-out:
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.event, msg.data)
				flusher.Flush()
				if done && msg.event == "done" {
					return
				}
			}
		}
	}()

	misses := 0
	for {
		select {
		case <-ctx.Done():
			<-writerDone
			return
		case <-writerDone:
			return
		case <-heartbeat.C:
			send("heartbeat", fmt.Sprintf(`{"at":%q}`, time.Now().UTC().Format(time.RFC3339)))
		case payload := <-notifyCh:
			// payload is the producer's {stream,data} envelope JSON; forward
			// verbatim under the "chunk" event. Validate it's JSON so a
			// malformed notification can't corrupt the SSE framing.
			if json.Valid([]byte(payload)) {
				send("chunk", payload)
			}
		case p := <-pollCh:
			if !p.found {
				// Task not visible yet (just created) or evicted. Tolerate a
				// short window, then end the stream so we don't hold a conn
				// for a task that will never appear.
				misses++
				if misses >= 5 {
					done = true
					send("done", `{"status":"unknown"}`)
					cancel()
				}
				continue
			}
			misses = 0
			if isTerminalTaskStatus(p.status) && !done {
				done = true
				send("done", fmt.Sprintf(`{"status":%q}`, string(p.status)))
				cancel()
			}
		}
	}
}

// isTerminalTaskStatus mirrors the delegator's own terminal set (its
// isTerminalStatus is unexported). Kept in lock-step with
// delegator.TaskStatus constants.
func isTerminalTaskStatus(s delegator.TaskStatus) bool {
	switch s {
	case delegator.StatusCompleted, delegator.StatusFailed,
		delegator.StatusCancelled, delegator.StatusTimedOut:
		return true
	}
	return false
}

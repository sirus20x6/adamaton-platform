package apiserver

// Tests for the workflow-failure gauge (card-8bb8ab29):
//   - the /api/v1/health/workflows endpoint surface, and the gauge folded
//     into /api/v1/health/fleet;
//   - the production PgWorkflowFailureSource SQL against the real
//     workflow.runs table (skips when the DB is unavailable).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/sirus20x6/adamaton-platform/dashboard/apiserver/health"
)

// TestFleetWorkflows_endpoint asserts the /health/workflows surface:
//   - 503 when no topology is loaded (fleetHealth nil);
//   - 200 with workflows == null when a topology is loaded but no
//     workflow source is wired (the newTestAPIServerWithHealth helper
//     wires none), so the SPA can tell "health down" from "no telemetry".
func TestFleetWorkflows_endpoint(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)

	req := httptest.NewRequest("GET", "/api/v1/health/workflows", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Data struct {
			Workflows *health.WorkflowHealth `json:"workflows"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if !body.Success {
		t.Fatal("success=false")
	}
	if body.Data.Workflows != nil {
		t.Fatalf("expected null workflows without a source, got %+v", body.Data.Workflows)
	}
}

// TestFleetWorkflows_noTopology503 asserts the 503 branch.
func TestFleetWorkflows_noTopology503(t *testing.T) {
	s := newPoollessServer(t) // fleetHealth + fleetTopology both nil
	rr := serveVia(s, s.registerHealthEndpoints, http.MethodGet, "/api/v1/health/workflows", "")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// TestPgWorkflowFailureSource_windowedCounts drives the production SQL
// against a real workflow.runs table. It seeds a definition and a spread
// of runs across statuses + windows, then asserts the source tallies the
// last-hour failures/completions, the running count, and the 24h failure
// count. Skips when the evo DB is unreachable.
func TestPgWorkflowFailureSource_windowedCounts(t *testing.T) {
	pool := sharedTestPool(t)
	ctx := context.Background()

	defID := "wf5-def-" + uuid.NewString()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO workflow.definitions (id, name, definition, created_at, updated_at)
		VALUES ($1, $1, '{}', NOW(), NOW())`, defID)
	if err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		// CASCADE on the FK removes the runs with the definition.
		_, _ = pool.Exec(c, `DELETE FROM workflow.definitions WHERE id = $1`, defID)
	})

	// seedRun inserts one run with an explicit status + finished_at offset.
	// finishedSQL is a SQL expression for finished_at (NULL for running).
	seedRun := func(status, finishedSQL string) {
		t.Helper()
		id := "wf5-run-" + uuid.NewString()
		_, e := pool.Exec(ctx, `
			INSERT INTO workflow.runs
				(id, definition_id, temporal_id, temporal_run, status, input, started_at, finished_at)
			VALUES ($1, $2, $1, $1, $3, '{}', NOW() - INTERVAL '2 hours', `+finishedSQL+`)`,
			id, defID, status)
		if e != nil {
			t.Fatalf("seed run (%s): %v", status, e)
		}
	}

	// In-window (last hour):
	seedRun("failed", "NOW() - INTERVAL '10 minutes'")
	seedRun("failed", "NOW() - INTERVAL '30 minutes'")
	seedRun("completed", "NOW() - INTERVAL '15 minutes'")
	seedRun("cancelled", "NOW() - INTERVAL '20 minutes'") // counted by neither
	// Out-of-window for 1h but in 24h:
	seedRun("failed", "NOW() - INTERVAL '5 hours'")
	// Out-of-window entirely:
	seedRun("failed", "NOW() - INTERVAL '30 hours'")
	// Running (no finished_at):
	seedRun("running", "NULL")
	seedRun("running", "NULL")

	src := &health.PgWorkflowFailureSource{Pool: pool}
	counts, err := src.WorkflowFailureCounts(ctx)
	if err != nil {
		t.Fatalf("WorkflowFailureCounts: %v", err)
	}

	// These assertions are deltas off a possibly-non-empty table: other
	// rows in workflow.runs would inflate the absolute counts. To stay
	// robust we re-derive the expected minimums from our own seeds rather
	// than asserting exact totals — except RunningNow which we can't
	// isolate, so we only assert it grew by at least our 2 seeds.
	if counts.FailedLastHour < 2 {
		t.Fatalf("failed_1h = %d, want >= 2 (our seeds)", counts.FailedLastHour)
	}
	if counts.CompletedLastHour < 1 {
		t.Fatalf("completed_1h = %d, want >= 1", counts.CompletedLastHour)
	}
	if counts.FailedLast24h < 3 {
		t.Fatalf("failed_24h = %d, want >= 3 (2 in-hour + 1 at 5h)", counts.FailedLast24h)
	}
	if counts.RunningNow < 2 {
		t.Fatalf("running_now = %d, want >= 2", counts.RunningNow)
	}
	// 24h failures must include the 1h failures.
	if counts.FailedLast24h < counts.FailedLastHour {
		t.Fatalf("failed_24h (%d) < failed_1h (%d)", counts.FailedLast24h, counts.FailedLastHour)
	}
}

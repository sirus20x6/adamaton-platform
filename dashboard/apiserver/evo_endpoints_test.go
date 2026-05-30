package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseLimitOffset exercises the clamp logic without a DB: defaults when
// params are absent, upper-bound clamping, the negative/zero rejection paths
// (which fall back to the default for limit and 0 for offset), and offset
// overflow clamping to maxOffset.
func TestParseLimitOffset(t *testing.T) {
	const defLimit, maxLimit, maxOffset = 50, 500, 100000
	cases := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{name: "absent -> defaults", query: "", wantLimit: defLimit, wantOffset: 0},
		{name: "in-range", query: "?limit=10&offset=20", wantLimit: 10, wantOffset: 20},
		{name: "limit over max clamps", query: "?limit=99999", wantLimit: maxLimit, wantOffset: 0},
		{name: "offset over max clamps", query: "?offset=99999999", wantLimit: defLimit, wantOffset: maxOffset},
		// limit<=0 is rejected (n>0 guard) -> default retained.
		{name: "zero limit -> default", query: "?limit=0", wantLimit: defLimit, wantOffset: 0},
		{name: "negative limit -> default", query: "?limit=-5", wantLimit: defLimit, wantOffset: 0},
		// offset<0 is rejected (n>=0 guard) -> 0 retained.
		{name: "negative offset -> 0", query: "?offset=-5", wantLimit: defLimit, wantOffset: 0},
		// non-numeric is ignored (Atoi error) -> defaults.
		{name: "garbage limit -> default", query: "?limit=abc", wantLimit: defLimit, wantOffset: 0},
		{name: "garbage offset -> 0", query: "?offset=xyz", wantLimit: defLimit, wantOffset: 0},
		// int64 overflow string: Atoi errors -> default retained (no panic).
		{name: "overflow limit string -> default", query: "?limit=99999999999999999999", wantLimit: defLimit, wantOffset: 0},
		{name: "overflow offset string -> 0", query: "?offset=99999999999999999999", wantLimit: defLimit, wantOffset: 0},
		// exact boundary values are accepted unchanged.
		{name: "limit exactly max", query: "?limit=500", wantLimit: maxLimit, wantOffset: 0},
		{name: "offset exactly max", query: "?offset=100000", wantLimit: defLimit, wantOffset: maxOffset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/evo/runs"+tc.query, nil)
			limit, offset := parseLimitOffset(req, defLimit, maxLimit, maxOffset)
			require.Equal(t, tc.wantLimit, limit, "limit")
			require.Equal(t, tc.wantOffset, offset, "offset")
		})
	}
}

// TestEvoEndpoints_NoPool confirms every evo handler 503s cleanly when the pool
// is nil (no panic, JSON error body).
func TestEvoEndpoints_NoPool(t *testing.T) {
	s := newPoollessServer(t)
	for _, target := range []string{
		"/api/v1/evo/runs",
		"/api/v1/evo/runs/abc/programs",
		"/api/v1/evo/insights",
	} {
		rr := serveVia(s, s.registerEvoEndpoints, http.MethodGet, target, "")
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, target)
		require.Contains(t, rr.Body.String(), "evo pool not configured", target)
	}
}

// TestEvoRunPrograms_MissingID asserts the run-id required guard. The {id}
// path var is non-empty for any real route hit, so we drive the handler with an
// empty mux var directly.
func TestEvoRunPrograms_MissingID(t *testing.T) {
	s := newDBTestServer(t)
	// A request whose {id} resolves to empty isn't reachable through the
	// router (mux won't match), so call the handler with no vars set.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/evo/runs//programs", nil)
	rr := httptest.NewRecorder()
	s.handleEvoRunPrograms(rr, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "run id required")
}

// TestEvoRuns_Smoke drives the live handler against the migrated DB and asserts
// the response decodes as a JSON array of EvoRun (empty is fine).
func TestEvoRuns_Smoke(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerEvoEndpoints, http.MethodGet, "/api/v1/evo/runs?limit=5", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out []EvoRun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
}

// TestEvoRuns_GroupByMatchesCount is the perf-rewrite assertion for
// card-6145d587: the num_programs returned by the GROUP BY rewrite must equal a
// straight COUNT(*) over evo.programs for that run. We seed a run + a few
// programs, then compare the handler's count against the table count.
func TestEvoRuns_GroupByMatchesCount(t *testing.T) {
	s := newDBTestServer(t)
	pool := s.evoPool
	ctx := context.Background()

	const runID = "test-evo-groupby-run"
	const taskID = "test-evo-groupby-task"
	// Clean any prior leftovers, then seed deterministically. runs.task_id has
	// a FK to evo.tasks, so a task row must exist first.
	_, _ = pool.Exec(ctx, `DELETE FROM evo.programs WHERE run_id = $1`, runID)
	_, _ = pool.Exec(ctx, `DELETE FROM evo.runs WHERE id = $1`, runID)
	_, _ = pool.Exec(ctx, `DELETE FROM evo.tasks WHERE id = $1`, taskID)
	_, err := pool.Exec(ctx, `INSERT INTO evo.tasks (id, domain, name, seed_program)
		VALUES ($1, 'test', 'groupby-task', '')`, taskID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO evo.runs (id, task_id, status, started_at)
		VALUES ($1, $2, 'running', NOW())`, runID, taskID)
	require.NoError(t, err)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM evo.programs WHERE run_id = $1`, runID)
		_, _ = pool.Exec(c, `DELETE FROM evo.runs WHERE id = $1`, runID)
		_, _ = pool.Exec(c, `DELETE FROM evo.tasks WHERE id = $1`, taskID)
	})
	const nPrograms = 3
	for i := 0; i < nPrograms; i++ {
		_, err := pool.Exec(ctx, `INSERT INTO evo.programs
			(run_id, island, generation, source, backend, compiled, correct, speedup)
			VALUES ($1, 0, $2, 'x', 'cpu', true, true, $3)`, runID, i, float64(i+1))
		require.NoError(t, err)
	}

	rr := serveVia(s, s.registerEvoEndpoints, http.MethodGet, "/api/v1/evo/runs?limit=500", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out []EvoRun
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))

	var found *EvoRun
	for i := range out {
		if out[i].ID == runID {
			found = &out[i]
			break
		}
	}
	require.NotNil(t, found, "seeded run not present in listing")
	require.Equal(t, nPrograms, found.NumPrograms, "GROUP BY count must match seeded program count")

	// Cross-check against a direct COUNT(*) so the assertion can't drift if
	// the handler query changes.
	var direct int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM evo.programs WHERE run_id = $1`, runID).Scan(&direct))
	require.Equal(t, direct, found.NumPrograms)
	require.NotNil(t, found.BestSpeedup)
	require.InDelta(t, float64(nPrograms), *found.BestSpeedup, 0.001, "max(speedup) preserved by rewrite")
}

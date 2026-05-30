package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func TestWorkersEndpoints_NoPool(t *testing.T) {
	s := newPoollessServer(t)
	for _, target := range []string{
		"/api/v1/workers",
		"/api/v1/workers/" + uuid.NewString(),
	} {
		rr := serveVia(s, s.registerWorkerEndpoints, http.MethodGet, target, "")
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, target)
		require.Contains(t, rr.Body.String(), "evo pool not configured", target)
	}
}

func TestListWorkers_Smoke(t *testing.T) {
	s := newDBTestServer(t)
	for _, q := range []string{"", "?limit=5", "?limit=0", "?offset=99999999"} {
		rr := serveVia(s, s.registerWorkerEndpoints, http.MethodGet, "/api/v1/workers"+q, "")
		require.Equal(t, http.StatusOK, rr.Code, "q=%q body=%s", q, rr.Body.String())
		var out []Worker
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "q=%q", q)
		// Array fields must never serialise as null.
		for _, wk := range out {
			require.NotNil(t, wk.DeclaredQueues)
			require.NotNil(t, wk.CPUFeatures)
			require.NotNil(t, wk.Permissions)
		}
	}
}

func TestGetWorker_NotFound(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerWorkerEndpoints, http.MethodGet, "/api/v1/workers/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "worker not found")
}

// TestWorkers_JobCountsGroupBy asserts the wave-1 rewrite: jobs_assigned /
// jobs_completed come from a single COUNT(*) FILTER ... GROUP BY assigned_worker
// pass, and must equal direct filtered counts. We seed one worker + a handful of
// jobs in assorted statuses and read the worker back through getWorker.
func TestWorkers_JobCountsGroupBy(t *testing.T) {
	s := newDBTestServer(t)
	pool := s.evoPool
	ctx := context.Background()

	workerID := "wf3-worker-" + uuid.NewString()[:8]
	// Seed an active worker with a fresh heartbeat so the derived status is
	// 'active' (not stale/offline) and getWorker returns it.
	_, err := pool.Exec(ctx, `
		INSERT INTO evo.workers (id, identity, hostname, status, last_heartbeat, first_seen, last_seen)
		VALUES ($1, $1, 'wf3-host', 'active', NOW(), NOW(), NOW())`, workerID)
	require.NoError(t, err)
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM evo.jobs WHERE assigned_worker = $1`, workerID)
		_, _ = pool.Exec(c, `DELETE FROM evo.workers WHERE id = $1`, workerID)
	})

	// jobs_assigned counts status IN ('assigned','running'); jobs_completed
	// counts status='succeeded'. Seed 2 assigned-ish, 3 succeeded, 1 failed
	// (counted by neither).
	statuses := []string{"assigned", "running", "succeeded", "succeeded", "succeeded", "failed"}
	for _, st := range statuses {
		_, err := pool.Exec(ctx, `
			INSERT INTO evo.jobs (id, kind, spec, requirements, status, assigned_worker)
			VALUES ($1, 'evolve', '{}'::jsonb, '{}'::jsonb, $2, $3)`, uuid.NewString(), st, workerID)
		require.NoError(t, err)
	}

	rr := serveVia(s, s.registerWorkerEndpoints, http.MethodGet, "/api/v1/workers/"+workerID, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var wk Worker
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &wk))
	require.Equal(t, 2, wk.JobsAssigned, "assigned+running counted")
	require.Equal(t, 3, wk.JobsCompleted, "succeeded counted")

	// Cross-check against direct filtered counts so the assertion tracks the
	// query's intent rather than a fixed literal.
	var asg, done int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE status IN ('assigned','running')),
		       COUNT(*) FILTER (WHERE status = 'succeeded')
		FROM evo.jobs WHERE assigned_worker = $1`, workerID).Scan(&asg, &done))
	require.Equal(t, asg, wk.JobsAssigned)
	require.Equal(t, done, wk.JobsCompleted)
}

// ---- llm_endpoints ----

// TestVLLMCompatibilityInfo is the one LLM handler that touches only config —
// vllmClient stays nil, so this is safe to drive directly.
func TestVLLMCompatibilityInfo(t *testing.T) {
	s := newPoollessServer(t)
	rr := serveVia(s, s.registerLLMForTest, http.MethodGet, "/api/v1/llm/vllm-info", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp APIResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Success)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, true, data["openai_compatible"])
}

// TestLLMGeneration_BadJSON exercises the request-validation branch of
// testLLMGeneration, which returns 400 BEFORE dereferencing the nil vllmClient.
func TestLLMGeneration_BadJSON(t *testing.T) {
	s := newPoollessServer(t)
	rr := serveVia(s, s.registerLLMForTest, http.MethodPost, "/api/v1/llm/test", `{not json`)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	var resp APIResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.False(t, resp.Success)
	require.Contains(t, resp.Error, "Invalid request format")
}

// registerLLMForTest mounts the LLM handlers on the subrouter at the same paths
// server.go uses. The package has no standalone registerLLMEndpoints helper
// (they're wired inline in setupRoutes), so the test declares one to drive them
// through the router the same way.
func (s *APIServer) registerLLMForTest(api *mux.Router) {
	api.HandleFunc("/llm/status", s.getLLMStatus).Methods("GET")
	api.HandleFunc("/llm/test", s.testLLMGeneration).Methods("POST")
	api.HandleFunc("/llm/vllm-info", s.getVLLMCompatibilityInfo).Methods("GET")
}

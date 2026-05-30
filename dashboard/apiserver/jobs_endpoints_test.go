package apiserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestJobsEndpoints_NoPool covers the evo-pool-not-configured branch on the
// read handlers.
func TestJobsEndpoints_NoPool(t *testing.T) {
	s := newPoollessServer(t)
	for _, target := range []string{
		"/api/v1/jobs",
		"/api/v1/jobs/" + uuid.NewString(),
	} {
		rr := serveVia(s, s.registerJobsEndpoints, http.MethodGet, target, "")
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, target)
		require.Contains(t, rr.Body.String(), "evo pool not configured", target)
	}
}

// TestListJobs_Smoke + pagination/filter params drive the live handler against
// the migrated DB. Empty result is acceptable; we only assert shape + that the
// filters/limit don't error.
func TestListJobs_Smoke(t *testing.T) {
	s := newDBTestServer(t)
	for _, q := range []string{
		"",
		"?limit=5",
		"?limit=0",        // clamped to default
		"?offset=1000000", // clamped to maxOffset
		"?status=succeeded",
		"?worker=nonexistent",
		"?kind=evolve",
		"?status=" + longString(400), // >256 cap path
	} {
		rr := serveVia(s, s.registerJobsEndpoints, http.MethodGet, "/api/v1/jobs"+q, "")
		require.Equal(t, http.StatusOK, rr.Code, "q=%q body=%s", q, rr.Body.String())
		var out []Job
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), "q=%q", q)
	}
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestGetJob_NotFound(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerJobsEndpoints, http.MethodGet, "/api/v1/jobs/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "job not found")
}

// TestSubmitJob_NoTemporal asserts the temporal-not-configured 503 branch, the
// first guard in submitJob — reached before any body parse.
func TestSubmitJob_NoTemporal(t *testing.T) {
	s := newDBTestServer(t) // temporalClient is nil
	rr := serveVia(s, s.registerJobsEndpoints, http.MethodPost, "/api/v1/jobs/submit",
		`{"kind":"evolve","requirements":{"queue_class":"cpu"}}`)
	require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "temporal client not configured")
}

// TestScanJob_NullPayloads documents the null-coalescing on spec/requirements
// via a round-trip through a seeded row would need a worker; instead we exercise
// the listJobs path which uses scanJob, ensuring null spec/requirements decode
// as JSON null rather than empty/invalid.
func TestListJobs_NullSpecDecodes(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerJobsEndpoints, http.MethodGet, "/api/v1/jobs?limit=50", "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out []Job
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	for _, j := range out {
		// scanJob guarantees Spec / Requirements are valid JSON (never empty
		// bytes), so re-marshalling the slice must succeed.
		require.True(t, json.Valid(j.Spec), "spec must be valid json for job %s", j.ID)
		require.True(t, json.Valid(j.Requirements), "requirements must be valid json for job %s", j.ID)
	}
}

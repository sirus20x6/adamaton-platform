package apiserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDatasetsEndpoints_NoPool(t *testing.T) {
	s := newPoollessServer(t)
	cases := []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/datasets", ""},
		{http.MethodGet, "/api/v1/datasets/abc", ""},
		{http.MethodGet, "/api/v1/datasets/abc/quality", ""},
		{http.MethodGet, "/api/v1/datasets/versions/" + uuid.NewString(), ""},
		{http.MethodPost, "/api/v1/datasets", `{"id":"x","task_type":"sft"}`},
		{http.MethodPost, "/api/v1/datasets/abc/archive", ""},
		{http.MethodPost, "/api/v1/datasets/abc/tags", `{"tag":"t"}`},
		{http.MethodDelete, "/api/v1/datasets/abc/tags/t", ""},
	}
	for _, tc := range cases {
		rr := serveVia(s, s.registerDatasetsEndpoints, tc.method, tc.target, tc.body)
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, tc.target)
		require.Contains(t, rr.Body.String(), "evo pool not configured", tc.target)
	}
}

// TestCreateDataset_Validation exercises the validation branches that run before
// the INSERT (so they don't need the evo_datasets schema, which is absent in the
// unit DB).
func TestCreateDataset_Validation(t *testing.T) {
	s := newDBTestServer(t)
	cases := []struct {
		name, body, wantSub string
		wantCode            int
	}{
		{name: "bad json", body: `{`, wantCode: http.StatusBadRequest, wantSub: "invalid json"},
		{name: "missing id", body: `{"task_type":"sft"}`, wantCode: http.StatusBadRequest, wantSub: "id is required"},
		{name: "bad task_type", body: `{"id":"d1","task_type":"nope"}`, wantCode: http.StatusBadRequest, wantSub: "task_type must be one of"},
		{name: "empty task_type", body: `{"id":"d1"}`, wantCode: http.StatusBadRequest, wantSub: "task_type must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets", tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			require.Contains(t, rr.Body.String(), tc.wantSub)
		})
	}
}

// TestImportDataset_NoTemporal: importDataset checks temporalClient first, so a
// nil client yields 503 before any body validation.
func TestImportDataset_NoTemporal(t *testing.T) {
	s := newDBTestServer(t) // temporalClient nil
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/import",
		`{"dataset_id":"d1","source_kind":"local","source_ref":"/tmp/x"}`)
	require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "temporal client not configured")
}

// TestTransformDataset_Validation: transformDataset parses + validates the body
// BEFORE calling startWorkflow, so the 400 branches are reachable with a nil
// temporal client. The valid-body case then hits the temporal 503 in
// startWorkflow.
func TestTransformDataset_Validation(t *testing.T) {
	s := newDBTestServer(t)
	cases := []struct {
		name, body string
		wantCode   int
		wantSub    string
	}{
		{name: "bad json", body: `{`, wantCode: http.StatusBadRequest, wantSub: "invalid json"},
		{name: "missing dataset_id", body: `{"target_format":"jsonl","source_version_id":"` + uuid.NewString() + `"}`, wantCode: http.StatusBadRequest, wantSub: "dataset_id is required"},
		{name: "bad source uuid", body: `{"dataset_id":"d1","source_version_id":"not-uuid","target_format":"jsonl"}`, wantCode: http.StatusBadRequest, wantSub: "source_version_id must be UUID"},
		{name: "bad target format", body: `{"dataset_id":"d1","source_version_id":"` + uuid.NewString() + `","target_format":"csv"}`, wantCode: http.StatusBadRequest, wantSub: "target_format must be one of"},
		{name: "valid -> temporal 503", body: `{"dataset_id":"d1","source_version_id":"` + uuid.NewString() + `","target_format":"jsonl"}`, wantCode: http.StatusServiceUnavailable, wantSub: "temporal client not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/transform", tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			require.Contains(t, rr.Body.String(), tc.wantSub)
		})
	}
}

// TestMakeSplits_Validation covers the ratio/length checks (all before
// startWorkflow).
func TestMakeSplits_Validation(t *testing.T) {
	s := newDBTestServer(t)
	vid := uuid.NewString()
	cases := []struct {
		name, body string
		wantCode   int
		wantSub    string
	}{
		{name: "bad version uuid", body: `{"version_id":"x","name":"s","ratios":[0.5,0.5],"split_names":["a","b"]}`, wantCode: http.StatusBadRequest, wantSub: "version_id must be UUID"},
		{name: "missing name", body: `{"version_id":"` + vid + `","ratios":[1.0],"split_names":["a"]}`, wantCode: http.StatusBadRequest, wantSub: "name is required"},
		{name: "len mismatch", body: `{"version_id":"` + vid + `","name":"s","ratios":[0.5],"split_names":["a","b"]}`, wantCode: http.StatusBadRequest, wantSub: "same length"},
		{name: "non-positive ratio", body: `{"version_id":"` + vid + `","name":"s","ratios":[0,1],"split_names":["a","b"]}`, wantCode: http.StatusBadRequest, wantSub: "each ratio must be"},
		{name: "ratios dont sum", body: `{"version_id":"` + vid + `","name":"s","ratios":[0.2,0.2],"split_names":["a","b"]}`, wantCode: http.StatusBadRequest, wantSub: "sum to ~1.0"},
		{name: "valid -> temporal 503", body: `{"version_id":"` + vid + `","name":"s","ratios":[0.7,0.3],"split_names":["a","b"]}`, wantCode: http.StatusServiceUnavailable, wantSub: "temporal client not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/splits", tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			require.Contains(t, rr.Body.String(), tc.wantSub)
		})
	}
}

func TestMakeKFoldSplits_Validation(t *testing.T) {
	s := newDBTestServer(t)
	vid := uuid.NewString()
	cases := []struct {
		name, body string
		wantCode   int
		wantSub    string
	}{
		{name: "bad uuid", body: `{"version_id":"x","k":5}`, wantCode: http.StatusBadRequest, wantSub: "version_id must be UUID"},
		{name: "k too small", body: `{"version_id":"` + vid + `","k":1}`, wantCode: http.StatusBadRequest, wantSub: "k must be"},
		{name: "valid -> temporal 503", body: `{"version_id":"` + vid + `","k":5}`, wantCode: http.StatusServiceUnavailable, wantSub: "temporal client not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/splits/kfold", tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			require.Contains(t, rr.Body.String(), tc.wantSub)
		})
	}
}

func TestRecordQualityObservation_Validation(t *testing.T) {
	s := newDBTestServer(t)
	vid := uuid.NewString()
	rid := uuid.NewString()
	cases := []struct {
		name, body string
		wantCode   int
		wantSub    string
	}{
		{name: "bad version uuid", body: `{"version_id":"x","distill_run_id":"` + rid + `"}`, wantCode: http.StatusBadRequest, wantSub: "version_id must be UUID"},
		{name: "bad run uuid", body: `{"version_id":"` + vid + `","distill_run_id":"y"}`, wantCode: http.StatusBadRequest, wantSub: "distill_run_id must be UUID"},
		{name: "nothing set", body: `{"version_id":"` + vid + `","distill_run_id":"` + rid + `"}`, wantCode: http.StatusBadRequest, wantSub: "at least one of"},
		{name: "valid -> temporal 503", body: `{"version_id":"` + vid + `","distill_run_id":"` + rid + `","won":true}`, wantCode: http.StatusServiceUnavailable, wantSub: "temporal client not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/quality", tc.body)
			require.Equal(t, tc.wantCode, rr.Code, rr.Body.String())
			require.Contains(t, rr.Body.String(), tc.wantSub)
		})
	}
}

// TestGetDatasetVersion_BadUUID asserts the UUID guard (runs before the query,
// so it works without the evo_datasets schema).
func TestGetDatasetVersion_BadUUID(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodGet, "/api/v1/datasets/versions/not-a-uuid", "")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid version_id")
}

// TestAddDatasetTag_Validation: tag-required guard runs before the INSERT.
func TestAddDatasetTag_Validation(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerDatasetsEndpoints, http.MethodPost, "/api/v1/datasets/d1/tags", `{"tag":"  "}`)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "tag is required")
}

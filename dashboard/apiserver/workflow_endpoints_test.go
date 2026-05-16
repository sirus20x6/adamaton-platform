// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
// /thearray/gogents/internal/apiserver/workflow_endpoints_test.go
//
// Tests for the workflow-builder endpoints. Each test spins up an
// isolated paradedb container via pgutil.TestDSN — set
// GOGENTS_SKIP_DOCKER_TESTS to skip when docker is not available.
// Endpoints that also call Temporal (runDefinition's happy path) are
// tested indirectly through the not-found short-circuit; the full happy
// path is exercised by tests/integration/api_test.go where a real
// Temporal server is available.
package apiserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/pgutil"
	"github.com/sirus20x6/adamaton-core/types"
	"github.com/sirus20x6/adamaton-evolve/workflow-builder/workflowstore"
)

// newTestServerWithStore returns an APIServer wired to a fresh Postgres
// workflow store. setupRoutes is wired to a fresh router so the test
// exercises the real registration paths.
func newTestServerWithStore(t *testing.T) (*APIServer, *workflowstore.Store) {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	store, err := workflowstore.NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cfg := &types.Config{}
	cfg.Temporal.TaskQueue = "test-queue"

	s := &APIServer{
		logger:        logger,
		config:        cfg,
		router:        mux.NewRouter(),
		workflowStore: store,
	}
	// Register only the workflow-builder routes under /api/v1 so we can drive
	// them through the router. setupRoutes() would also need the metrics
	// handler and a temporal client; we don't want either for these unit
	// tests.
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.setupWorkflowBuilderRoutes(api)
	return s, store
}

// validDefinitionJSON returns a structurally valid workflow definition that
// the wfvalidate package will accept. It's deliberately minimal (one activity
// node with a known-allowed activity) so the validator doesn't reject it for
// reasons unrelated to the test under it.
func validDefinitionJSON(t *testing.T) string {
	t.Helper()
	graph := workflowstore.GraphDef{
		Nodes: []workflowstore.NodeDef{
			{
				ID:           "n1",
				Type:         "activity",
				ActivityName: "FetchDiffActivity",
				Position:     workflowstore.Position{X: 0, Y: 0},
			},
		},
		Edges:      []workflowstore.EdgeDef{},
		Parameters: []workflowstore.ParameterDef{},
	}
	b, err := json.Marshal(graph)
	require.NoError(t, err)
	return string(b)
}

// invalidDefinitionJSON returns a definition with a duplicate node ID. The
// validator must reject it with an error mentioning the offending ID.
func invalidDefinitionJSON(t *testing.T) string {
	t.Helper()
	graph := workflowstore.GraphDef{
		Nodes: []workflowstore.NodeDef{
			{ID: "dup", Type: "activity", ActivityName: "FetchDiffActivity"},
			{ID: "dup", Type: "activity", ActivityName: "FetchDiffActivity"},
		},
	}
	b, err := json.Marshal(graph)
	require.NoError(t, err)
	return string(b)
}

func decodeAPIResp(t *testing.T, rr *httptest.ResponseRecorder) APIResponse {
	t.Helper()
	var r APIResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &r))
	return r
}

// --- createDefinition ----------------------------------------------------

func TestCreateDefinition_EmptyBody(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions", bytes.NewReader(nil))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.False(t, resp.Success)
	require.NotEmpty(t, resp.Error)
}

func TestCreateDefinition_BodyTooLarge(t *testing.T) {
	s, _ := newTestServerWithStore(t)

	// 2MB body (over 1MB MaxBytesReader limit). The handler must respond
	// with 400 and not OOM.
	huge := make([]byte, 2*1024*1024)
	for i := range huge {
		huge[i] = 'a'
	}
	body := []byte(`{"name":"x","definition":"`)
	body = append(body, huge...)
	body = append(body, []byte(`"}`)...)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestCreateDefinition_MissingName(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	def := validDefinitionJSON(t)
	body := []byte(`{"name":"","description":"d","definition":` + def + `}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.Contains(t, strings.ToLower(resp.Error), "name")
}

func TestCreateDefinition_MissingDefinition(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	body := []byte(`{"name":"My WF","description":"d"}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.Contains(t, strings.ToLower(resp.Error), "definition")
}

func TestCreateDefinition_InvalidGraph(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	def := invalidDefinitionJSON(t)
	body := []byte(`{"name":"WF","definition":` + def + `}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code, "duplicate node IDs should be rejected by validator")
	resp := decodeAPIResp(t, rr)
	require.False(t, resp.Success)
	require.Contains(t, strings.ToLower(resp.Error), "dup")
}

func TestCreateDefinition_HappyPath(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	def := validDefinitionJSON(t)
	body := []byte(`{"name":"WF","description":"the workflow","definition":` + def + `}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions", bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusCreated, rr.Code, "valid definitions should be accepted")
	resp := decodeAPIResp(t, rr)
	require.True(t, resp.Success)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok, "expected data to be the WorkflowDefinition object")
	require.NotEmpty(t, data["id"], "created definition should have a non-empty id")
}

// --- runDefinition (not-found path only — real run requires Temporal) -------

func TestRunDefinition_NotFound(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/definitions/no-such-id/run", nil)
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusNotFound, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.False(t, resp.Success)
	require.Contains(t, strings.ToLower(resp.Error), "not found")
}

// --- listDefinitions / getDefinition / updateDefinition / deleteDefinition --

func TestListDefinitions_EmptyReturnsEmptyArray(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/definitions", nil)
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.True(t, resp.Success)
	// On empty result, the handler returns an empty array (not null) so the
	// UI doesn't have to special-case nil.
	defs, ok := resp.Data.([]interface{})
	require.True(t, ok, "expected data to be an array; got %T", resp.Data)
	require.Empty(t, defs)
}

func TestGetDefinition_NotFound(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/definitions/no-such", nil)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestUpdateDefinition_RequiresName(t *testing.T) {
	s, store := newTestServerWithStore(t)
	def, err := store.CreateDefinition("orig", "", validDefinitionJSON(t))
	require.NoError(t, err)

	body := []byte(`{"name":""}`)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workflows/definitions/"+def.ID, bytes.NewReader(body))
	s.Router().ServeHTTP(rr, req)

	require.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestDeleteDefinition_NotFoundIsNotFatal(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/workflows/definitions/missing", nil)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// --- listRuns / getRun ----------------------------------------------------

func TestListRuns_BadLimitFallsBackToDefault(t *testing.T) {
	s, _ := newTestServerWithStore(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/runs?limit=not-a-number", nil)
	s.Router().ServeHTTP(rr, req)

	// Bad limit is silently ignored — the handler still returns 200 with an
	// empty list. We just want to confirm we don't 500 on a parse error.
	require.Equal(t, http.StatusOK, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.True(t, resp.Success)
}

func TestGetRun_NotFound(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/runs/no-such", nil)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// --- shortID helper ------------------------------------------------------

func TestShortID(t *testing.T) {
	require.Equal(t, "12345678", shortID("123456789012"))
	require.Equal(t, "abc", shortID("abc"))
	require.Equal(t, "", shortID(""))
}

// --- listActivities / listRoles ------------------------------------------

func TestListActivities(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/activities", nil)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
}

func TestListRoles(t *testing.T) {
	s, _ := newTestServerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/roles", nil)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	resp := decodeAPIResp(t, rr)
	require.True(t, resp.Success)
}
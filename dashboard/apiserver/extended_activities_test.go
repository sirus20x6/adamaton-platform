package apiserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-evolve/workflow-builder/pluginloader"
)

// TestListExtendedActivities_ShapeContract locks in the wire shape the
// Connectors page consumes. Builds an APIServer with a real pluginLoader
// over a fake YAML tree (no DB, no Temporal) so the contract drift is
// caught as a unit test even when the integration stack is offline.
//
// The handler must return APIResponse{Data:{nodes,categories}} and each
// node must carry its source tag ("builtin"|"community"|"n8n") so the
// frontend can tab by origin.
func TestListExtendedActivities_ShapeContract(t *testing.T) {
	tmp := t.TempDir()
	// Single builtin and single n8n node — enough to assert source tagging
	// without dragging in the full 122-YAML builtin catalog.
	builtin := filepath.Join(tmp, "builtin")
	n8n := filepath.Join(tmp, "n8n")
	require.NoError(t, os.MkdirAll(builtin, 0o755))
	require.NoError(t, os.MkdirAll(n8n, 0o755))

	writeYAML(t, filepath.Join(builtin, "compile.yaml"), `
name: CompileCheck
displayName: Compile
description: verify compile
category: build
version: 1
executor: { type: agent_prompt, agent: codex }
prompt: "go"
properties:
  - { name: diff, type: string, required: true }
outputType: CheckResult
`)
	writeYAML(t, filepath.Join(n8n, "slack.yaml"), `
name: SlackPost
displayName: Slack
description: post a slack message
category: messaging
version: 1
executor: { type: n8n_bridge, package: n8n-nodes-base, nodeName: slack }
properties:
  - { name: channel, type: string, required: true }
outputType: Message
`)

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	pl := pluginloader.NewLoader([]string{builtin, n8n}, logger)
	require.NoError(t, pl.LoadAll())
	require.Equal(t, 2, pl.Count(), "fake catalog should load both nodes")

	s := &APIServer{
		logger:       logger,
		router:       mux.NewRouter(),
		pluginLoader: pl,
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.setupWorkflowBuilderRoutes(api)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflows/activities/extended", nil)
	s.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Nodes []struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
				Source      string `json:"source"`
				OutputType  string `json:"output_type"`
				Category    string `json:"category"`
			} `json:"nodes"`
			Categories []map[string]interface{} `json:"categories"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.True(t, resp.Success)
	require.Len(t, resp.Data.Nodes, 2)

	bySource := map[string]string{}
	for _, n := range resp.Data.Nodes {
		bySource[n.Source] = n.Name
	}
	require.Equal(t, "CompileCheck", bySource["builtin"], "compile YAML must be tagged builtin from its containing dir")
	require.Equal(t, "SlackPost", bySource["n8n"], "slack YAML must be tagged n8n from its containing dir")
}

func writeYAML(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

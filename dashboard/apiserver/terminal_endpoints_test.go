package apiserver

// DB+tmux-backed terminal lifecycle test (card-1cb6e76c). The guarantee:
//   create -> a tmux session actually exists -> delete -> the tmux session
//   is gone and the row is flipped to 'dead'.
//
// This test drives the REAL projectfs/tmux path (no mocks), so it requires
// tmux on PATH and PTY_BACKEND != "none". When either is missing it
// t.Skips, per the lane's "gate the terminal-lifecycle test on tmux/
// PTY_BACKEND availability" instruction. It also needs the evo DB and the
// 0018 host column (createTerminal selects evo.projects.host).

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/projectfs"
)

// tmuxSessionExists shells out to `tmux has-session -t <name>` and reports
// whether the session is live right now. Used to assert the create/delete
// effects on the actual tmux server, independent of the DB row.
func tmuxSessionExists(name string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", name)
	return cmd.Run() == nil
}

func TestTerminalLifecycle_createThenDelete(t *testing.T) {
	if !projectfs.Enabled() {
		t.Skip("PTY_BACKEND=none: terminal backend disabled")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH: skipping terminal-lifecycle test")
	}
	// A tmux server must be reachable (e.g. a usable $HOME/socket dir). Probe
	// with a harmless command; skip rather than fail in sandboxes without one.
	if out, err := exec.Command("tmux", "list-sessions").CombinedOutput(); err != nil &&
		!strings.Contains(string(out), "no server running") {
		t.Skipf("tmux server unusable in this environment: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	s := newDBTestServer(t)
	ensureProjectsHostColumn(t, s)
	ctx := context.Background()

	// Register a local project rooted at a temp dir so the tmux session has
	// a valid working directory.
	root := t.TempDir()
	projectID := "wf5-termproj-" + uuid.NewString()[:8]
	_, err := s.evoPool.Exec(ctx, `
		INSERT INTO evo.projects (id, path, display_name, type, host)
		VALUES ($1, $2, $1, 'folder', '')`, projectID, root)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = s.evoPool.Exec(context.Background(), `DELETE FROM evo.projects WHERE id = $1`, projectID)
	})

	// CREATE: POST /projects/{id}/terminals.
	rr := serveVia(s, s.registerTerminalEndpoints, http.MethodPost,
		"/api/v1/projects/"+projectID+"/terminals", `{"title":"wf5","command":"bash"}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())

	var created TerminalSession
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "live", created.Status)
	require.True(t, strings.HasPrefix(created.ID, "adam-"), "session id should be adam-prefixed: %q", created.ID)

	// Safety-net cleanup: kill the tmux session + delete the row even if an
	// assertion below fails mid-test.
	t.Cleanup(func() {
		_ = projectfs.KillSession(created.ID)
		_, _ = s.evoPool.Exec(context.Background(),
			`DELETE FROM evo.terminal_sessions WHERE id = $1`, created.ID)
	})

	// The tmux session must actually exist now.
	require.True(t, tmuxSessionExists(created.ID),
		"tmux session %q should exist after create", created.ID)

	// The DB row must exist and be live.
	var dbStatus string
	require.NoError(t, s.evoPool.QueryRow(ctx,
		`SELECT status FROM evo.terminal_sessions WHERE id = $1`, created.ID).Scan(&dbStatus))
	require.Equal(t, "live", dbStatus)

	// DELETE: DELETE /terminals/{sid}.
	rr = serveVia(s, s.registerTerminalEndpoints, http.MethodDelete,
		"/api/v1/terminals/"+created.ID, "")
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

	// The tmux session must be gone.
	require.False(t, tmuxSessionExists(created.ID),
		"tmux session %q should be killed after delete", created.ID)

	// The DB row must be flipped to 'dead' with ended_at stamped.
	var endedStatus string
	var endedAtNonNull bool
	require.NoError(t, s.evoPool.QueryRow(ctx,
		`SELECT status, ended_at IS NOT NULL FROM evo.terminal_sessions WHERE id = $1`,
		created.ID).Scan(&endedStatus, &endedAtNonNull))
	require.Equal(t, "dead", endedStatus)
	require.True(t, endedAtNonNull, "ended_at should be set on delete")
}

// TestTerminalEndpoints_disabled503 asserts the PTY_BACKEND=none gate: when
// the backend is disabled every handler 503s. We can only exercise this
// deterministically when the env actually says none; otherwise skip so the
// suite stays green wherever it runs.
func TestTerminalEndpoints_disabled503(t *testing.T) {
	if projectfs.Enabled() {
		t.Skip("PTY_BACKEND != none in this env: disabled-path not exercised here")
	}
	s := newPoollessServer(t)
	rr := serveVia(s, s.registerTerminalEndpoints, http.MethodGet,
		"/api/v1/projects/whatever/terminals", "")
	require.Equal(t, http.StatusServiceUnavailable, rr.Code)
	require.Contains(t, rr.Body.String(), "PTY_BACKEND=none")
}

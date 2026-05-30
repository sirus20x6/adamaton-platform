package apiserver

// DB-backed project file-tree traversal tests (card-1cb6e76c). The
// guarantee under test: getProjectTree can never be coaxed into reading
// outside the project root via `..` path segments — they are neutralized
// (collapsed back to the root), and an escaping symlink is rejected
// outright. A nonexistent project id is a clean 404.
//
// These run locally (host=""), so projectfs.BuildTree executes in-process
// against a real temp dir; no remote agent or tmux is involved.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/projectfs"
)

// fileNode mirrors projectfs.FileNode for decoding the tree response
// without importing the exact struct (its JSON tags are the contract).
type fileNode struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// ensureProjectsHostColumn applies the additive part of migration
// 0018_projects_host to the test DB when the baseline is still at v17.
// projects_endpoints.go's projectLocSQL selects evo.projects.host, so the
// column must exist for getProjectTree to run. The migration is
// `ADD COLUMN host TEXT NOT NULL DEFAULT ”` — purely additive and
// back-compat (existing rows default to ” = local host), so applying it
// via IF NOT EXISTS is idempotent and safe to run from a test.
func ensureProjectsHostColumn(t *testing.T, s *APIServer) {
	t.Helper()
	_, err := s.evoPool.Exec(context.Background(),
		`ALTER TABLE evo.projects ADD COLUMN IF NOT EXISTS host TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		t.Skipf("cannot ensure evo.projects.host column (need migration 0018): %v", err)
	}
}

// seedLocalProject creates a temp directory tree and registers it as a
// local (host="") project, returning the project id and its root path.
// The temp dir holds a marker file so we can assert the tree lists the
// ROOT (not a parent) when `..` is supplied.
func seedLocalProject(t *testing.T, s *APIServer) (projectID, root string) {
	t.Helper()
	ensureProjectsHostColumn(t, s)
	root = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "inside-marker.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(root, "subdir"), 0o755))

	projectID = "wf5-fsproj-" + uuid.NewString()[:8]
	_, err := s.evoPool.Exec(context.Background(), `
		INSERT INTO evo.projects (id, path, display_name, type)
		VALUES ($1, $2, $1, 'folder')`, projectID, root)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = s.evoPool.Exec(context.Background(), `DELETE FROM evo.projects WHERE id = $1`, projectID)
	})
	return projectID, root
}

func getTree(t *testing.T, s *APIServer, projectID, relPath string) *responseCapture {
	t.Helper()
	target := "/api/v1/projects/" + projectID + "/tree"
	if relPath != "" {
		target += "?path=" + relPath
	}
	rr := serveVia(s, s.registerProjectsEndpoints, http.MethodGet, target, "")
	return &responseCapture{code: rr.Code, body: rr.Body.Bytes()}
}

type responseCapture struct {
	code int
	body []byte
}

// TestProjectTree_dotDotTraversalNeutralized is the security guarantee:
// `..` segments never read outside the project root. The handler resolves
// `../../../..` (and friends) back to the root and lists the ROOT's
// contents — it must never return the parent directory's entries.
func TestProjectTree_dotDotTraversalNeutralized(t *testing.T) {
	s := newDBTestServer(t)
	projectID, _ := seedLocalProject(t, s)

	// A baseline: the root listing contains our marker.
	base := getTree(t, s, projectID, "")
	require.Equal(t, http.StatusOK, base.code, string(base.body))

	// Two families of evil input. The first collapse straight back to the
	// project root (so they 200 with the root listing); the second collapse
	// to an in-root path that doesn't exist (so they 404). BOTH are secure
	// outcomes — the traversal is neutralized either way and never reaches
	// the real filesystem parent. None may ever leak /etc or a parent dir.
	collapseToRoot := []string{
		"..",
		"../..",
		"%2e%2e%2f%2e%2e%2f%2e%2e", // ../../.. url-encoded -> root
	}
	collapseToMissing := []string{
		"..%2F..%2F..%2Fetc",           // ../../../etc -> root/etc (absent) -> 404
		"subdir/../../../../../../etc", // climb out from a subdir -> root/etc -> 404
	}

	for _, evil := range collapseToRoot {
		rc := getTree(t, s, projectID, evil)
		require.Equal(t, http.StatusOK, rc.code, "evil=%q body=%s", evil, string(rc.body))

		var nodes []fileNode
		require.NoError(t, json.Unmarshal(rc.body, &nodes), "evil=%q", evil)

		names := map[string]bool{}
		for _, n := range nodes {
			require.False(t, filepath.IsAbs(n.Path), "evil=%q leaked absolute path %q", evil, n.Path)
			require.NotContains(t, n.Path, "..", "evil=%q leaked .. in path %q", evil, n.Path)
			names[n.Name] = true
		}
		// Must be OUR root (marker present), never the filesystem root.
		require.True(t, names["inside-marker.txt"], "evil=%q did not collapse to project root", evil)
		require.False(t, names["etc"] && names["usr"], "evil=%q escaped to filesystem root", evil)
	}

	for _, evil := range collapseToMissing {
		rc := getTree(t, s, projectID, evil)
		// Neutralized to an in-root path that doesn't exist -> a clean 404,
		// the card's stated outcome. The crucial property is that it did
		// NOT list /etc: a 200 listing here would be the escape bug.
		require.Equal(t, http.StatusNotFound, rc.code, "evil=%q body=%s", evil, string(rc.body))
		require.NotContains(t, string(rc.body), "passwd", "evil=%q leaked /etc contents", evil)
	}
}

// TestProjectTree_escapingSymlinkRejected proves the second layer of the
// guard: a symlink INSIDE the project that points OUT of it can't be used
// to read the target — projectfs returns ErrEscape, mapped to HTTP 400.
func TestProjectTree_escapingSymlinkRejected(t *testing.T) {
	s := newDBTestServer(t)
	projectID, root := seedLocalProject(t, s)

	// Create a symlink inside the project that points at the project's
	// PARENT (outside the root). Reading through it must be refused.
	outside := filepath.Dir(root)
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rc := getTree(t, s, projectID, "escape-link")
	// projectfs.BuildTree EvalSymlinks the resolved target and finds it
	// outside root -> ErrEscape -> 400. (If the platform resolved it back
	// under root somehow, a 200 with no parent leak is also acceptable;
	// but on Linux this is a hard 400.)
	require.Equal(t, http.StatusBadRequest, rc.code, "escaping symlink should 400, body=%s", string(rc.body))
	require.Contains(t, string(rc.body), projectfs.ErrEscape.Error())
}

// TestProjectTree_unknownProject404 keeps the not-found contract honest.
func TestProjectTree_unknownProject404(t *testing.T) {
	s := newDBTestServer(t)
	ensureProjectsHostColumn(t, s)
	rc := getTree(t, s, "wf5-nonexistent-"+uuid.NewString()[:8], "")
	require.Equal(t, http.StatusNotFound, rc.code, string(rc.body))
}

package apiserver

// Project endpoints. CRUD over evo.projects — the registry behind the
// dashboard "Projects" sidebar. A project is a host folder (git repo, nested
// worktree, submodule, or plain folder). This file is Phase 1: registry only.
// The file-tree (Phase 2), persistent terminals (Phase 3), and per-project
// Kanban board (Phase 4) land in sibling endpoint groups that key off
// projects.id. See docs/PROJECTS_KANBAN.md.
//
// Reads/writes go straight to evo.projects via s.evoPool; every handler 503s
// when the pool is nil, matching datasets_endpoints.go.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
)

// Project is the wire shape for /api/v1/projects. Mirrors evo.projects.
type Project struct {
	ID             string     `json:"id"`
	Path           string     `json:"path"`
	DisplayName    string     `json:"display_name"`
	Type           string     `json:"type"` // git-repo|worktree|submodule|folder
	GitRemote      *string    `json:"git_remote,omitempty"`
	ParentID       *string    `json:"parent_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
}

// ProjectCreateRequest is the body for POST /api/v1/projects. Only Path is
// required; DisplayName defaults to the folder basename and Type/GitRemote are
// auto-detected from the filesystem. ParentID is optional (nesting).
type ProjectCreateRequest struct {
	Path        string `json:"path"`
	DisplayName string `json:"display_name"`
	ParentID    string `json:"parent_id"`
}

func (s *APIServer) registerProjectsEndpoints(api *mux.Router) {
	api.HandleFunc("/projects", s.listProjects).Methods("GET")
	api.HandleFunc("/projects", s.createProject).Methods("POST")
	api.HandleFunc("/projects/{id}", s.getProject).Methods("GET")
	api.HandleFunc("/projects/{id}", s.deleteProject).Methods("DELETE")
	api.HandleFunc("/projects/{id}/tree", s.getProjectTree).Methods("GET")
	api.HandleFunc("/projects/{id}/file", s.getProjectFile).Methods("GET")
}

// FileNode is the wire shape for /api/v1/projects/{id}/tree. Path is relative
// to the project root ("" means the root itself); the tree endpoint returns
// the direct children of the requested path only (lazy, one level deep).
type FileNode struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

const projectsListSQL = `
SELECT id, path, display_name, type, git_remote, parent_id, created_at, last_accessed_at
FROM evo.projects
ORDER BY created_at DESC
LIMIT 500`

func (s *APIServer) listProjects(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.evoPool.Query(ctx, projectsListSQL)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Path, &p.DisplayName, &p.Type,
			&p.GitRemote, &p.ParentID, &p.CreatedAt, &p.LastAccessedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

const projectGetSQL = `
SELECT id, path, display_name, type, git_remote, parent_id, created_at, last_accessed_at
FROM evo.projects
WHERE id = $1`

func (s *APIServer) getProject(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var p Project
	if err := s.evoPool.QueryRow(ctx, projectGetSQL, id).Scan(
		&p.ID, &p.Path, &p.DisplayName, &p.Type,
		&p.GitRemote, &p.ParentID, &p.CreatedAt, &p.LastAccessedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "project not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	writeEvoJSON(w, p)
}

// maxFileBytes caps the file endpoint: refuse to serve anything bigger so we
// never buffer a huge blob into the response.
const maxFileBytes = 1 << 20 // 1 MiB

// projectRootSQL fetches just the on-disk root for a project; nil pool and
// missing rows are handled by the caller.
const projectRootSQL = `SELECT path FROM evo.projects WHERE id = $1`

// resolveInProject canonicalizes a caller-supplied relative path against a
// project root and guarantees the result stays inside that root. It returns
// the absolute, symlink-resolved path. The guard is layered: reject ".."
// segments up front, join+Clean against the root, then EvalSymlinks and
// re-check the resolved path is still under the (also symlink-resolved) root —
// so a symlink inside the tree can't be used to escape.
func resolveInProject(root, rel string) (string, error) {
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	rel = strings.TrimSpace(rel)
	// Treat a leading slash as relative-to-root, not absolute.
	rel = strings.TrimPrefix(rel, "/")
	clean := filepath.Clean("/" + rel) // collapses ".." that would escape "/"
	clean = strings.TrimPrefix(clean, "/")

	abs := filepath.Join(root, clean)
	abs = filepath.Clean(abs)

	// Re-resolve symlinks on the target when it exists; a non-existent path
	// (EvalSymlinks errors) is fine to keep as-is for the under-root check —
	// os.ReadDir/os.Open will then surface the not-found.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", errors.New("path escapes project root")
	}
	return abs, nil
}

// relFromRoot renders an absolute path back as a forward-slash relative path
// from the project root ("" for the root itself), for the FileNode wire shape.
func relFromRoot(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// projectRoot loads and symlink-resolves a project's on-disk path, returning
// false (after writing the response) when the pool is nil or the project is
// missing.
func (s *APIServer) projectRoot(w http.ResponseWriter, r *http.Request, id string) (string, bool) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var root string
	if err := s.evoPool.QueryRow(ctx, projectRootSQL, id).Scan(&root); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "project not found")
			return "", false
		}
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return "", false
	}
	root = filepath.Clean(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root, true
}

// getProjectTree lists the direct children of ?path (relative to the project
// root, "" = root) one level deep, dirs first then files, each sorted by name.
func (s *APIServer) getProjectTree(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	root, ok := s.projectRoot(w, r, id)
	if !ok {
		return
	}

	rel := r.URL.Query().Get("path")
	abs, err := resolveInProject(root, rel)
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeEvoErr(w, http.StatusNotFound, "path not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "readdir: "+err.Error())
		return
	}

	out := make([]FileNode, 0, len(entries))
	for _, e := range entries {
		child := filepath.Join(abs, e.Name())
		var size int64
		if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
		}
		out = append(out, FileNode{
			Name:  e.Name(),
			Path:  relFromRoot(root, child),
			IsDir: e.IsDir(),
			Size:  size,
		})
	}
	// Directories first, then files; each group alphabetical by name.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeEvoJSON(w, out)
}

// getProjectFile returns the contents of ?path within the project. Files over
// maxFileBytes get a 413; binary content (per http.DetectContentType) is
// base64-encoded, otherwise it is returned as utf8.
func (s *APIServer) getProjectFile(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	root, ok := s.projectRoot(w, r, id)
	if !ok {
		return
	}

	rel := r.URL.Query().Get("path")
	abs, err := resolveInProject(root, rel)
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeEvoErr(w, http.StatusNotFound, "file not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "stat: "+err.Error())
		return
	}
	if info.IsDir() {
		writeEvoErr(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if info.Size() > maxFileBytes {
		writeEvoErr(w, http.StatusRequestEntityTooLarge, "file exceeds 1MB limit")
		return
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "read: "+err.Error())
		return
	}

	// http.DetectContentType sniffs the first 512 bytes; anything that isn't
	// a text/* type (or that contains a NUL) is treated as binary.
	ctype := http.DetectContentType(data)
	isText := strings.HasPrefix(ctype, "text/") || strings.HasPrefix(ctype, "application/json") || strings.HasPrefix(ctype, "application/xml")

	resp := map[string]interface{}{
		"path":      relFromRoot(root, abs),
		"size":      info.Size(),
		"truncated": false,
	}
	if isText {
		resp["contents"] = string(data)
		resp["encoding"] = "utf8"
	} else {
		resp["contents"] = base64.StdEncoding.EncodeToString(data)
		resp["encoding"] = "base64"
	}
	writeEvoJSON(w, resp)
}

func (s *APIServer) createProject(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req ProjectCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.ParentID = strings.TrimSpace(req.ParentID)
	if req.Path == "" {
		writeEvoErr(w, http.StatusBadRequest, "path is required")
		return
	}

	// Canonicalize the path and verify it's an existing directory on the
	// host. We register absolute, symlink-resolved paths so the UNIQUE
	// constraint dedupes the same folder reached by different spellings.
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "bad path: "+err.Error())
		return
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeEvoErr(w, http.StatusBadRequest, "path does not exist: "+abs)
			return
		}
		writeEvoErr(w, http.StatusBadRequest, "cannot stat path: "+err.Error())
		return
	}
	if !info.IsDir() {
		writeEvoErr(w, http.StatusBadRequest, "path is not a directory: "+abs)
		return
	}

	if req.DisplayName == "" {
		req.DisplayName = filepath.Base(abs)
	}
	projType := detectProjectType(abs)
	var gitRemote *string
	if projType != "folder" {
		if remote := gitRemoteURL(abs); remote != "" {
			gitRemote = &remote
		}
	}

	// Validate parent_id (when supplied) exists, so we never insert a
	// dangling self-FK that the ON DELETE SET NULL can't help with.
	var parentID *string
	if req.ParentID != "" {
		var exists bool
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err := s.evoPool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM evo.projects WHERE id = $1)", req.ParentID).Scan(&exists)
		cancel()
		if err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "parent lookup: "+err.Error())
			return
		}
		if !exists {
			writeEvoErr(w, http.StatusBadRequest, "parent_id does not exist")
			return
		}
		parentID = &req.ParentID
	}

	slug := slugify(req.DisplayName)
	if slug == "" {
		slug = "project"
	}
	id := slug + "-" + uuid.NewString()[:8]

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const insertSQL = `
INSERT INTO evo.projects (id, path, display_name, type, git_remote, parent_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, path, display_name, type, git_remote, parent_id, created_at, last_accessed_at`
	var p Project
	if err := s.evoPool.QueryRow(ctx, insertSQL,
		id, abs, req.DisplayName, projType, gitRemote, parentID,
	).Scan(&p.ID, &p.Path, &p.DisplayName, &p.Type,
		&p.GitRemote, &p.ParentID, &p.CreatedAt, &p.LastAccessedAt); err != nil {
		// 23505 = unique_violation on path: the folder is already registered.
		if strings.Contains(err.Error(), "23505") {
			writeEvoErr(w, http.StatusConflict, "path already registered: "+abs)
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, p)
}

func (s *APIServer) deleteProject(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Unregister only — we never touch the files on disk.
	tag, err := s.evoPool.Exec(ctx, "DELETE FROM evo.projects WHERE id = $1", id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeEvoErr(w, http.StatusNotFound, "project not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// detectProjectType classifies a folder by how it relates to git:
//
//	folder     — no .git entry
//	git-repo   — .git is a directory (a normal clone / main worktree)
//	worktree   — .git is a file pointing into .../.git/worktrees/...
//	submodule  — .git is a file pointing into .../.git/modules/...
//
// A linked worktree and a submodule both use a `.git` *file* (a gitdir
// pointer) rather than a directory; we read it to tell them apart.
func detectProjectType(dir string) string {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "folder"
	}
	if info.IsDir() {
		return "git-repo"
	}
	// .git is a file: "gitdir: <path>".
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "git-repo"
	}
	pointer := strings.TrimSpace(string(data))
	switch {
	case strings.Contains(pointer, "/modules/"):
		return "submodule"
	case strings.Contains(pointer, "/worktrees/"):
		return "worktree"
	default:
		return "git-repo"
	}
}

// gitRemoteURL returns the origin remote URL for a repo dir, or "" if there
// is none / git is unavailable. Best-effort: a missing remote is normal for
// a fresh repo and not an error.
func gitRemoteURL(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "config", "--get", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

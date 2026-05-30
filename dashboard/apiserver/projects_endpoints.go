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
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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


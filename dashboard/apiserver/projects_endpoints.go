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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/sirus20x6/adamaton-core/projectfs"
)

// Project is the wire shape for /api/v1/projects. Mirrors evo.projects. Host
// names the machine the folder lives on ("" = the local host, for back-compat
// with pre-migration-0018 rows).
type Project struct {
	ID             string     `json:"id"`
	Path           string     `json:"path"`
	DisplayName    string     `json:"display_name"`
	Type           string     `json:"type"` // git-repo|worktree|submodule|folder
	Host           string     `json:"host"`
	GitRemote      *string    `json:"git_remote,omitempty"`
	ParentID       *string    `json:"parent_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
}

// ProjectCreateRequest is the body for POST /api/v1/projects. Only Path is
// required; DisplayName defaults to the folder basename and Type/GitRemote are
// auto-detected from the filesystem. ParentID is optional (nesting). Host
// selects the machine the folder lives on; empty means the local host.
type ProjectCreateRequest struct {
	Path        string `json:"path"`
	DisplayName string `json:"display_name"`
	ParentID    string `json:"parent_id"`
	Host        string `json:"host"`
}

func (s *APIServer) registerProjectsEndpoints(api *mux.Router) {
	api.HandleFunc("/projects", s.listProjects).Methods("GET")
	api.HandleFunc("/projects", s.createProject).Methods("POST")
	api.HandleFunc("/projects/hosts", s.listProjectHosts).Methods("GET")
	api.HandleFunc("/projects/{id}", s.getProject).Methods("GET")
	api.HandleFunc("/projects/{id}", s.deleteProject).Methods("DELETE")
	api.HandleFunc("/projects/{id}/tree", s.getProjectTree).Methods("GET")
	api.HandleFunc("/projects/{id}/file", s.getProjectFile).Methods("GET")
}

// listProjectHosts returns the hosts a project can be registered on: the local
// host plus every host with a configured deploy-agent URL (deduped, local
// first). The Projects UI uses this to populate the host picker on create.
func (s *APIServer) listProjectHosts(w http.ResponseWriter, r *http.Request) {
	writeEvoJSON(w, registerableHosts())
}

const projectsListSQL = `
SELECT id, path, display_name, type, host, git_remote, parent_id, created_at, last_accessed_at
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
		if err := rows.Scan(&p.ID, &p.Path, &p.DisplayName, &p.Type, &p.Host,
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
SELECT id, path, display_name, type, host, git_remote, parent_id, created_at, last_accessed_at
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
		&p.ID, &p.Path, &p.DisplayName, &p.Type, &p.Host,
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

// projectLoc fetches the on-disk root and host for a project; nil pool and
// missing rows are handled by the caller via projectPathHost.
const projectLocSQL = `SELECT path, host FROM evo.projects WHERE id = $1`

// projectPathHost loads a project's on-disk path and host, returning false
// (after writing the response) when the pool is nil or the project is missing.
// The path is returned as stored (already canonicalised at create time);
// projectfs re-resolves symlinks for the local case, and the agent does the
// same for the remote case.
func (s *APIServer) projectPathHost(w http.ResponseWriter, r *http.Request, id string) (path, host string, ok bool) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.evoPool.QueryRow(ctx, projectLocSQL, id).Scan(&path, &host); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "project not found")
			return "", "", false
		}
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return "", "", false
	}
	return path, host, true
}

// getProjectTree lists the direct children of ?path (relative to the project
// root, "" = root) one level deep, dirs first then files, each sorted by name.
// When the project lives on a remote host the request is reverse-proxied to
// that host's deploy-agent /project/tree; locally it runs projectfs.BuildTree.
func (s *APIServer) getProjectTree(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	root, host, ok := s.projectPathHost(w, r, id)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")

	if !isLocalHost(host) {
		s.proxyAgentGET(w, r, host, "/project/tree",
			"root="+urlQueryEscape(root)+"&path="+urlQueryEscape(rel))
		return
	}

	out, err := projectfs.BuildTree(root, rel)
	if err != nil {
		writeProjectfsErr(w, err)
		return
	}
	writeEvoJSON(w, out)
}

// getProjectFile returns the contents of ?path within the project. Files over
// projectfs.MaxFileBytes get a 413; binary content (per http.DetectContentType)
// is base64-encoded, otherwise utf8. Remote projects are reverse-proxied to the
// host's deploy-agent /project/file; locally it runs projectfs.ReadFileContents.
func (s *APIServer) getProjectFile(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	root, host, ok := s.projectPathHost(w, r, id)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")

	if !isLocalHost(host) {
		s.proxyAgentGET(w, r, host, "/project/file",
			"root="+urlQueryEscape(root)+"&path="+urlQueryEscape(rel))
		return
	}

	fc, err := projectfs.ReadFileContents(root, rel)
	if err != nil {
		writeProjectfsErr(w, err)
		return
	}
	writeEvoJSON(w, fc)
}

// writeProjectfsErr maps a projectfs error to the right HTTP status: ErrEscape
// -> 400, ErrTooLarge -> 413, ErrNotDir -> 400, os not-found -> 404, else 500.
func writeProjectfsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, projectfs.ErrEscape):
		writeEvoErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, projectfs.ErrTooLarge):
		writeEvoErr(w, http.StatusRequestEntityTooLarge, "file exceeds 1MB limit")
	case errors.Is(err, projectfs.ErrNotDir):
		writeEvoErr(w, http.StatusBadRequest, err.Error())
	case os.IsNotExist(err):
		writeEvoErr(w, http.StatusNotFound, "path not found")
	default:
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
	}
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

	// Resolve the target host. An empty host (or one that names this machine)
	// is local: we validate the directory in-process via projectfs. A remote
	// host is validated by its deploy-agent, which runs the same projectfs
	// primitives on its own filesystem. host is stored "" for local so
	// pre-0018 rows and freshly-local rows share a representation.
	targetHost := strings.TrimSpace(req.Host)
	var (
		abs       string
		projType  string
		gitRemote *string
		storeHost string
	)
	if isLocalHost(targetHost) {
		canon, ptype, remote, verr := projectfs.ValidateDir(req.Path)
		if verr != nil {
			if os.IsNotExist(verr) {
				writeEvoErr(w, http.StatusBadRequest, "path does not exist: "+req.Path)
				return
			}
			writeEvoErr(w, http.StatusBadRequest, "invalid path: "+verr.Error())
			return
		}
		abs, projType = canon, ptype
		if remote != "" {
			gitRemote = &remote
		}
		storeHost = "" // local is always stored as the empty host.
	} else {
		vctx, vcancel := context.WithTimeout(r.Context(), 12*time.Second)
		res, verr := validateOnAgent(vctx, targetHost, req.Path)
		vcancel()
		if verr != nil {
			writeEvoErr(w, http.StatusBadRequest,
				"validate on host "+targetHost+": "+verr.Error())
			return
		}
		abs, projType = res.Abs, res.Type
		if res.GitRemote != "" {
			remote := res.GitRemote
			gitRemote = &remote
		}
		storeHost = targetHost
	}

	if req.DisplayName == "" {
		req.DisplayName = filepathBase(abs)
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
INSERT INTO evo.projects (id, path, display_name, type, host, git_remote, parent_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, path, display_name, type, host, git_remote, parent_id, created_at, last_accessed_at`
	var p Project
	if err := s.evoPool.QueryRow(ctx, insertSQL,
		id, abs, req.DisplayName, projType, storeHost, gitRemote, parentID,
	).Scan(&p.ID, &p.Path, &p.DisplayName, &p.Type, &p.Host,
		&p.GitRemote, &p.ParentID, &p.CreatedAt, &p.LastAccessedAt); err != nil {
		// 23505 = unique_violation on (host, path): the folder is already
		// registered on that host.
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

// filepathBase returns the last element of a slash- or OS-separated path. Both
// local (projectfs) and remote (deploy-agent) hosts are unix, so a forward
// slash is the canonical separator; filepath.Base handles the local case too.
func filepathBase(p string) string { return filepath.Base(p) }

// urlQueryEscape escapes a value for use in a query string when proxying tree
// and file requests to a remote host's deploy-agent.
func urlQueryEscape(v string) string { return url.QueryEscape(v) }

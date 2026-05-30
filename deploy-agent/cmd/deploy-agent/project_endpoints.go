package main

// Project-serving endpoints layered on top of core/projectfs. Unlike the
// /restart, /scale, etc. handlers — which drive docker compose against this
// host's stack — these expose the host's *filesystem* and persistent tmux
// terminals so the dashboard apiserver can browse projects and attach shells
// on whichever host the project actually lives on.
//
// The agent has NO database: every request carries the absolute project root
// (and, for tree/file reads, a relative path under it) as query params or JSON
// body, and we hand them straight to projectfs. projectfs.ResolveInRoot is the
// escape guard, so a caller can't walk out of the supplied root via "..".
//
// Auth is the same shared bearer token as the deploy endpoints (requireAuth);
// the token IS the security boundary. The websocket attach is the one route
// that can't go through requireAuth's plain wrapper unchanged — see
// handleTerminalWS for how the bearer token is checked before the upgrade.

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/sirus20x6/adamaton-core/projectfs"
)

// terminalWSUpgrader upgrades /project/terminals/{id}/ws. The agent sits behind
// Caddy on a fixed in-network hostname and the bearer token is checked before
// the upgrade, so we don't lean on Origin for the security boundary; accept any
// origin to keep the dashboard's cross-host attach simple.
var terminalWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// registerProjectEndpoints wires the /project/* routes onto mux. Called from
// run() next to the deploy-endpoint registrations. The terminal routes are
// registered under the shared "/project/terminals/" prefix and demuxed by
// method + path suffix in handleTerminalsPrefix, because http.ServeMux has no
// path-param routing for the {id} segment.
func (s *server) registerProjectEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/project/validate", s.requireAuth(s.handleProjectValidate))
	mux.HandleFunc("/project/tree", s.requireAuth(s.handleProjectTree))
	mux.HandleFunc("/project/file", s.requireAuth(s.handleProjectFile))
	mux.HandleFunc("/project/terminals", s.requireAuth(s.handleTerminalsCreate))
	// The {id}-scoped terminal routes (ws upgrade, resize, delete) share this
	// prefix. handleTerminalsPrefix authenticates and dispatches; the websocket
	// route checks the bearer token itself (it can't 401 after an upgrade).
	mux.HandleFunc("/project/terminals/", s.handleTerminalsPrefix)
}

// handleProjectValidate canonicalises an absolute path and classifies it.
//
//	GET /project/validate?path=<abs> -> {abs, type, git_remote} | 400
func (s *server) handleProjectValidate(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	abs, ptype, gitRemote, err := projectfs.ValidateDir(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"abs":        abs,
		"type":       ptype,
		"git_remote": gitRemote,
	})
}

// handleProjectTree lists the direct children of root/path (one level).
//
//	GET /project/tree?root=<abs>&path=<rel> -> []FileNode
func (s *server) handleProjectTree(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	rel := r.URL.Query().Get("path")
	if root == "" {
		http.Error(w, "root is required", http.StatusBadRequest)
		return
	}
	nodes, err := projectfs.BuildTree(root, rel)
	if err != nil {
		writeProjectFSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

// handleProjectFile reads root/path, 413 when it exceeds projectfs.MaxFileBytes.
//
//	GET /project/file?root=<abs>&path=<rel> -> FileContents | 413
func (s *server) handleProjectFile(w http.ResponseWriter, r *http.Request) {
	root := r.URL.Query().Get("root")
	rel := r.URL.Query().Get("path")
	if root == "" {
		http.Error(w, "root is required", http.StatusBadRequest)
		return
	}
	fc, err := projectfs.ReadFileContents(root, rel)
	if err != nil {
		writeProjectFSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fc)
}

// terminalCreateRequest is the POST /project/terminals body.
type terminalCreateRequest struct {
	Root    string `json:"root"`
	Command string `json:"command"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// terminalResizeRequest is the POST /project/terminals/{id}/resize body.
type terminalResizeRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// handleTerminalsCreate spawns a persistent tmux session rooted at root.
//
//	POST /project/terminals {root, command, cols, rows} -> {id}
//
// id is "adam-"+uuid so the dashboard can recognise agent-created sessions and
// the name can't collide with an operator's manual tmux session.
func (s *server) handleTerminalsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if !projectfs.Enabled() {
		http.Error(w, "terminal backend disabled (PTY_BACKEND=none)", http.StatusServiceUnavailable)
		return
	}
	var req terminalCreateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Root == "" {
		http.Error(w, "root is required", http.StatusBadRequest)
		return
	}
	// Canonicalise + verify the cwd exists before handing it to tmux; a bad
	// -c path makes tmux fail with a less actionable error.
	abs, _, _, err := projectfs.ValidateDir(req.Root)
	if err != nil {
		http.Error(w, "root: "+err.Error(), http.StatusBadRequest)
		return
	}
	command := req.Command
	if command == "" {
		command = defaultShell()
	}
	cols := req.Cols
	if cols <= 0 {
		cols = 80
	}
	rows := req.Rows
	if rows <= 0 {
		rows = 24
	}

	id := "adam-" + uuid.NewString()
	if err := projectfs.CreateSession(id, abs, command, cols, rows); err != nil {
		log.Printf("terminal create failed root=%s err=%v", abs, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("terminal create: id=%s root=%s cols=%d rows=%d", id, abs, cols, rows)
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

// handleTerminalsPrefix demultiplexes the {id}-scoped terminal routes that
// share the "/project/terminals/" prefix:
//
//	GET    ws /project/terminals/{id}/ws      -> upgrade + BridgeWebsocket
//	POST      /project/terminals/{id}/resize  -> ResizeSession -> 200
//	DELETE    /project/terminals/{id}         -> KillSession   -> 204
//
// The websocket route authenticates inside handleTerminalWS (it can't 401 after
// the upgrade); the other two require the bearer token up front here.
func (s *server) handleTerminalsPrefix(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/project/terminals/")
	if rest == "" {
		http.Error(w, "terminal id required", http.StatusBadRequest)
		return
	}

	// /project/terminals/{id}/ws — auth happens in the handler so a failed
	// token returns 401 instead of a half-open upgrade.
	if id, ok := strings.CutSuffix(rest, "/ws"); ok {
		s.handleTerminalWS(w, r, id)
		return
	}

	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// /project/terminals/{id}/resize
	if id, ok := strings.CutSuffix(rest, "/resize"); ok {
		s.handleTerminalResize(w, r, id)
		return
	}

	// /project/terminals/{id} — DELETE only. Reject ids containing a slash
	// (an unknown sub-route) rather than treating the whole tail as an id.
	if strings.Contains(rest, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.handleTerminalDelete(w, r, rest)
}

// handleTerminalWS upgrades the connection and bridges it to the tmux session.
// Auth is checked before the upgrade: once we've upgraded we can't send an HTTP
// status, so an unauthenticated request must be rejected here.
func (s *server) handleTerminalWS(w http.ResponseWriter, r *http.Request, id string) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if id == "" {
		http.Error(w, "terminal id required", http.StatusBadRequest)
		return
	}
	if !projectfs.SessionExists(id) {
		http.Error(w, "terminal session not found", http.StatusNotFound)
		return
	}
	conn, err := terminalWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response on failure.
		log.Printf("terminal ws upgrade failed id=%s err=%v", id, err)
		return
	}
	defer conn.Close()
	if err := projectfs.BridgeWebsocket(conn, id); err != nil {
		log.Printf("terminal ws bridge ended id=%s err=%v", id, err)
	}
}

// handleTerminalResize resizes an existing session so a fresh attach picks up
// the new geometry.
//
//	POST /project/terminals/{id}/resize {cols, rows} -> 200
func (s *server) handleTerminalResize(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if id == "" {
		http.Error(w, "terminal id required", http.StatusBadRequest)
		return
	}
	var req terminalResizeRequest
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		http.Error(w, "cols and rows must be positive", http.StatusBadRequest)
		return
	}
	if err := projectfs.ResizeSession(id, req.Cols, req.Rows); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleTerminalDelete kills a session.
//
//	DELETE /project/terminals/{id} -> 204
func (s *server) handleTerminalDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	if id == "" {
		http.Error(w, "terminal id required", http.StatusBadRequest)
		return
	}
	if err := projectfs.KillSession(id); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("terminal delete: id=%s", id)
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSONBody decodes the request body into v, capping the read at 64 KiB
// (terminal create/resize bodies are tiny) and rejecting unknown fields so a
// typo'd key surfaces as a 400 rather than being silently ignored.
func decodeJSONBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

// defaultShell picks the session's program when the caller omits one: $SHELL if
// set, else bash.
func defaultShell() string {
	if sh := strings.TrimSpace(os.Getenv("SHELL")); sh != "" {
		return sh
	}
	return "bash"
}

// writeProjectFSError maps projectfs sentinel errors to HTTP statuses: ErrEscape
// and ErrNotDir are caller mistakes (400), ErrTooLarge is 413, and anything else
// (os not-found, permission) is 400 with the raw message — the agent trusts its
// authenticated caller, so leaking the os error text is acceptable here.
func writeProjectFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, projectfs.ErrTooLarge):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
	case errors.Is(err, projectfs.ErrEscape), errors.Is(err, projectfs.ErrNotDir):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

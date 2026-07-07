package apiserver

// Terminal endpoints. Persistent, tmux-backed shells for the dashboard
// "Projects" feature. Each terminal is a long-lived `tmux` session whose shell
// survives a page reload, a network drop, and an apiserver restart/crash. The
// rows live in evo.terminal_sessions (migration 0016); this file owns reads +
// writes:
//
//   - POST   /projects/{id}/terminals  -> tmux new-session + insert row
//   - GET    /projects/{id}/terminals  -> list rows for a project
//   - GET    /terminals/{sid}/ws       -> websocket bridge to `tmux attach`
//   - POST   /terminals/{sid}/resize   -> pty.Setsize + tmux resize-window
//   - DELETE /terminals/{sid}          -> tmux kill-session + flip status='dead'
//
// On boot ReconcileTerminals reconciles the table against `tmux ls` (vanished
// -> 'dead'); a ~60s ReapOrphanTerminals kills tmux sessions whose project row
// is gone. The whole feature is gated behind env PTY_BACKEND (tmux|none;
// default tmux) — when 'none' every endpoint 503s.
//
// The id of a session is also its tmux session name ("adam-"+uuid), so the row
// and the tmux session share a key. Reads/writes go through s.evoPool; every
// handler 503s when the pool is nil, matching the sibling endpoint groups.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/sirus20x6/adamaton-core/projectfs"
	"github.com/sirus20x6/adamaton-core/tracectx"
)

// terminalsEnabled reports whether the tmux backend is active (PTY_BACKEND !=
// "none"). When false, the endpoints respond 503 and the boot reconciler/reaper
// are no-ops. Delegates to projectfs.Enabled so the gate lives in one place.
func terminalsEnabled() bool { return projectfs.Enabled() }

// TerminalSession is the wire shape for /api/v1/projects/{id}/terminals.
// Mirrors evo.terminal_sessions (minus tmux_session, an internal detail that
// equals id today).
type TerminalSession struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Title     string     `json:"title"`
	Command   string     `json:"command"`
	Cwd       string     `json:"cwd"`
	Status    string     `json:"status"`
	Cols      int        `json:"cols"`
	Rows      int        `json:"rows"`
	CreatedAt time.Time  `json:"created_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// TerminalCreateRequest is the body for POST /projects/{id}/terminals. All
// fields are optional: Command defaults to "bash", Cols to 120, Rows to 40,
// Title to "shell".
type TerminalCreateRequest struct {
	Title   string `json:"title"`
	Command string `json:"command"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// TerminalResizeRequest is the body for POST /terminals/{sid}/resize.
type TerminalResizeRequest struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func (s *APIServer) registerTerminalEndpoints(api *mux.Router) {
	api.HandleFunc("/projects/{id}/terminals", s.listTerminals).Methods("GET")
	api.HandleFunc("/projects/{id}/terminals", s.createTerminal).Methods("POST")
	api.HandleFunc("/terminals/{sid}/ws", s.terminalWS).Methods("GET")
	api.HandleFunc("/terminals/{sid}/ticket", s.issueTerminalTicket).Methods("POST")
	api.HandleFunc("/terminals/{sid}/resize", s.resizeTerminal).Methods("POST")
	api.HandleFunc("/terminals/{sid}", s.deleteTerminal).Methods("DELETE")
}

const terminalSelectCols = `id, project_id, title, command, COALESCE(cwd, ''), status, cols, rows, created_at, ended_at`

func scanTerminal(row pgx.Row, t *TerminalSession) error {
	return row.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Command, &t.Cwd,
		&t.Status, &t.Cols, &t.Rows, &t.CreatedAt, &t.EndedAt)
}

const terminalsListSQL = `
SELECT ` + terminalSelectCols + `
FROM evo.terminal_sessions
WHERE project_id = $1
ORDER BY created_at DESC
LIMIT 500`

func (s *APIServer) listTerminals(w http.ResponseWriter, r *http.Request) {
	if !terminalsEnabled() {
		writeEvoErr(w, http.StatusServiceUnavailable, "terminals disabled (PTY_BACKEND=none)")
		return
	}
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	projectID := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.evoPool.Query(ctx, terminalsListSQL, projectID)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]TerminalSession, 0)
	for rows.Next() {
		var t TerminalSession
		if err := scanTerminal(rows, &t); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

func (s *APIServer) createTerminal(w http.ResponseWriter, r *http.Request) {
	if !terminalsEnabled() {
		writeEvoErr(w, http.StatusServiceUnavailable, "terminals disabled (PTY_BACKEND=none)")
		return
	}
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	projectID := mux.Vars(r)["id"]

	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req TerminalCreateRequest
	// An empty body is valid (all fields default); only a malformed body is
	// an error.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Command = strings.TrimSpace(req.Command)
	if req.Title == "" {
		req.Title = "shell"
	}
	if req.Command == "" {
		req.Command = "bash"
	}
	if req.Cols <= 0 {
		req.Cols = 120
	}
	if req.Rows <= 0 {
		req.Rows = 40
	}
	// Clamp to sane terminal bounds so a hostile body can't push tmux into a
	// pathological geometry.
	if req.Cols > 1000 {
		req.Cols = 1000
	}
	if req.Rows > 1000 {
		req.Rows = 1000
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	// Look up the project to resolve its on-disk path (the tmux working dir)
	// and the host the folder lives on.
	var projectPath, projectHost string
	if err := s.evoPool.QueryRow(ctx,
		"SELECT path, host FROM evo.projects WHERE id = $1", projectID,
	).Scan(&projectPath, &projectHost); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "project not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "project lookup: "+err.Error())
		return
	}

	local := isLocalHost(projectHost)

	// Create the tmux session: locally via projectfs, or on the remote host's
	// deploy-agent. In both cases the session id is the key for the row and the
	// tmux session name; remotely the agent mints its own "adam-"+uuid id, so
	// we adopt that as our row id to keep the apiserver and agent in lock-step.
	var sid string
	if local {
		sid = "adam-" + uuid.NewString()
		if err := projectfs.CreateSession(sid, projectPath, req.Command, req.Cols, req.Rows); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		id, err := s.createRemoteTerminal(ctx, projectHost, projectPath, req)
		if err != nil {
			writeEvoErr(w, http.StatusBadGateway, "create terminal on host "+projectHost+": "+err.Error())
			return
		}
		sid = id
	}

	const insertSQL = `
INSERT INTO evo.terminal_sessions
    (id, project_id, tmux_session, title, command, cwd, status, cols, rows)
VALUES ($1, $2, $1, $3, $4, $5, 'live', $6, $7)
RETURNING ` + terminalSelectCols
	var t TerminalSession
	if err := scanTerminal(s.evoPool.QueryRow(ctx, insertSQL,
		sid, projectID, req.Title, req.Command, projectPath, req.Cols, req.Rows,
	), &t); err != nil {
		// The row insert failed after we already spawned the session — kill the
		// orphan (locally or on the agent) so we don't leak it.
		if local {
			_ = projectfs.KillSession(sid)
		} else {
			s.deleteRemoteTerminal(context.Background(), projectHost, sid)
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, t)
}

// terminalUpgrader upgrades the /terminals/{sid}/ws GET into a websocket.
// Origin is validated against the security allowlist (security.go): the
// dashboard + vite dev server are same-origin through the /evo-api proxy and
// pass the same-host check; other trusted fronts (deepresearch.local,
// adamaton.local, loopback dev ports) are allowlisted; anything else is
// refused before the upgrade so a hostile page can't ride a user's browser
// into a shell.
var terminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     requestOriginAllowed,
}

func (s *APIServer) terminalWS(w http.ResponseWriter, r *http.Request) {
	if !terminalsEnabled() {
		writeEvoErr(w, http.StatusServiceUnavailable, "terminals disabled (PTY_BACKEND=none)")
		return
	}
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	// Reject cross-origin browsers before doing any work (the upgrader
	// would also refuse, but this returns a clean 403 instead of a failed
	// upgrade).
	if !requestOriginAllowed(r) {
		writeEvoErr(w, http.StatusForbidden, "origin not allowed")
		return
	}

	sid := mux.Vars(r)["sid"]

	// Credential check. Preferred: a short-lived ticket (minted via POST
	// /terminals/{sid}/ticket) in ?ticket= or the adam.ticket.* subprotocol,
	// or the API token via headers / the adam.token.* subprotocol. The
	// legacy ?token=<api-token> query parameter still works but is
	// deprecated (it lands in proxy logs). See terminal_ticket.go.
	if !s.checkTerminalToken(r, sid) {
		writeEvoErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Confirm the session exists and is live before upgrading, so a bogus sid
	// gets a clean 404 instead of a websocket that immediately dies. We also
	// pull the project host to decide local-attach vs remote reverse-proxy.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var status, host string
	err := s.evoPool.QueryRow(ctx,
		`SELECT ts.status, p.host
		 FROM evo.terminal_sessions ts
		 JOIN evo.projects p ON p.id = ts.project_id
		 WHERE ts.id = $1`, sid,
	).Scan(&status, &host)
	cancel()
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "terminal not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "lookup: "+err.Error())
		return
	}
	if status != "live" {
		writeEvoErr(w, http.StatusGone, "terminal is dead")
		return
	}

	conn, err := terminalUpgrader.Upgrade(w, r, wsResponseHeader(r))
	if err != nil {
		// Upgrade already wrote an error response on failure.
		s.logger.WithError(err).Warn("terminalWS: upgrade failed")
		return
	}

	if isLocalHost(host) {
		// projectfs.BridgeWebsocket owns the local PTY<->ws bridge: it attaches
		// `tmux attach-session -t <sid>`, pipes both directions, and tears down
		// only the attach client (the tmux session persists for the next
		// attach). It closes conn on return.
		if err := projectfs.BridgeWebsocket(conn, sid); err != nil {
			s.logger.WithError(err).WithField("sid", sid).Warn("terminalWS: local bridge failed")
		}
		return
	}

	// Remote: dial the host's deploy-agent ws and pipe frames both directions.
	dctx, dcancel := context.WithTimeout(context.Background(), 12*time.Second)
	agentConn, derr := dialAgentTerminalWS(dctx, host, sid)
	dcancel()
	if derr != nil {
		s.logger.WithError(derr).WithField("sid", sid).Warn("terminalWS: remote dial failed")
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("\r\n[adam: failed to attach remote terminal: "+derr.Error()+"]\r\n"))
		_ = conn.Close()
		return
	}
	bridgeWSConns(conn, agentConn)
}

func (s *APIServer) resizeTerminal(w http.ResponseWriter, r *http.Request) {
	if !terminalsEnabled() {
		writeEvoErr(w, http.StatusServiceUnavailable, "terminals disabled (PTY_BACKEND=none)")
		return
	}
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	sid := mux.Vars(r)["sid"]

	r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
	var req TerminalResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		writeEvoErr(w, http.StatusBadRequest, "cols and rows must be > 0")
		return
	}
	if req.Cols > 1000 {
		req.Cols = 1000
	}
	if req.Rows > 1000 {
		req.Rows = 1000
	}

	host, ok := s.terminalHost(w, r, sid)
	if !ok {
		return
	}

	// Resize the tmux window so a fresh attach picks up the new geometry —
	// locally via projectfs.ResizeSession, remotely by proxying to the host's
	// deploy-agent. Best-effort: a missing session is not fatal to the persist.
	if isLocalHost(host) {
		_ = projectfs.ResizeSession(sid, req.Cols, req.Rows)
	} else {
		body, _ := json.Marshal(map[string]int{"cols": req.Cols, "rows": req.Rows})
		s.resizeRemoteTerminal(r.Context(), host, sid, body)
	}

	// Persist the latest geometry so a reattach / list reflects it.
	uctx, ucancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer ucancel()
	if _, err := s.evoPool.Exec(uctx,
		"UPDATE evo.terminal_sessions SET cols = $2, rows = $3 WHERE id = $1",
		sid, req.Cols, req.Rows,
	); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusOK, map[string]any{"id": sid, "cols": req.Cols, "rows": req.Rows})
}

func (s *APIServer) deleteTerminal(w http.ResponseWriter, r *http.Request) {
	if !terminalsEnabled() {
		writeEvoErr(w, http.StatusServiceUnavailable, "terminals disabled (PTY_BACKEND=none)")
		return
	}
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	sid := mux.Vars(r)["sid"]

	host, ok := s.terminalHost(w, r, sid)
	if !ok {
		return
	}

	// Kill the tmux session (best-effort: it may already be gone), then flip
	// the row to 'dead'. We mark the row even if the kill found nothing so the
	// UI stops offering an attach. Local kills go through projectfs; remote
	// kills proxy to the host's deploy-agent.
	if isLocalHost(host) {
		_ = projectfs.KillSession(sid)
	} else {
		s.deleteRemoteTerminal(r.Context(), host, sid)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tag, err := s.evoPool.Exec(ctx,
		"UPDATE evo.terminal_sessions SET status = 'dead', ended_at = NOW() WHERE id = $1 AND status = 'live'",
		sid)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		// Either the id is unknown or it was already dead. Disambiguate so the
		// caller gets a clean 404 for a truly unknown session.
		var exists bool
		ectx, ecancel := context.WithTimeout(r.Context(), 5*time.Second)
		_ = s.evoPool.QueryRow(ectx, "SELECT EXISTS(SELECT 1 FROM evo.terminal_sessions WHERE id = $1)", sid).Scan(&exists)
		ecancel()
		if !exists {
			writeEvoErr(w, http.StatusNotFound, "terminal not found")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────────────────────────────────────────────
// Host resolution + remote-terminal proxying. A terminal's host is the host
// of the project it belongs to; local terminals run via projectfs, remote ones
// proxy to that host's deploy-agent /project/terminals API. The apiserver keeps
// the evo.terminal_sessions bookkeeping row regardless of host.
// ─────────────────────────────────────────────────────────────────────

// terminalHost loads the project host for a terminal session via the
// terminal_sessions→projects join, returning false (after writing the
// response) when the pool is nil or the session is unknown.
func (s *APIServer) terminalHost(w http.ResponseWriter, r *http.Request, sid string) (string, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var host string
	err := s.evoPool.QueryRow(ctx,
		`SELECT p.host
		 FROM evo.terminal_sessions ts
		 JOIN evo.projects p ON p.id = ts.project_id
		 WHERE ts.id = $1`, sid,
	).Scan(&host)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "terminal not found")
			return "", false
		}
		writeEvoErr(w, http.StatusInternalServerError, "lookup: "+err.Error())
		return "", false
	}
	return host, true
}

// createRemoteTerminal POSTs to the host's deploy-agent /project/terminals and
// returns the agent-minted session id ("adam-"+uuid). The agent runs
// projectfs.CreateSession on its own host with the given root/command/geometry.
func (s *APIServer) createRemoteTerminal(ctx context.Context, host, root string, req TerminalCreateRequest) (string, error) {
	base, ok := agentBaseURL(host)
	if !ok {
		return "", errors.New("no deploy-agent URL for host " + host)
	}
	token := deployAgentTokenForHost(host)
	if token == "" {
		return "", errors.New("DEPLOY_AGENT_TOKEN not set on dashboard")
	}
	body, _ := json.Marshal(map[string]any{
		"root":    root,
		"command": req.Command,
		"cols":    req.Cols,
		"rows":    req.Rows,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/project/terminals", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := tracectx.Client(12 * time.Second).Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(rb))
		if msg == "" {
			msg = resp.Status
		}
		return "", errors.New(msg)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", errors.New("decode terminal id: " + err.Error())
	}
	if out.ID == "" {
		return "", errors.New("agent returned empty terminal id")
	}
	return out.ID, nil
}

// resizeRemoteTerminal best-effort POSTs the new geometry to the host's
// deploy-agent /project/terminals/{id}/resize. Errors are swallowed: the
// persisted geometry below is the source of truth for a reattach.
func (s *APIServer) resizeRemoteTerminal(parent context.Context, host, sid string, body []byte) {
	base, ok := agentBaseURL(host)
	if !ok {
		return
	}
	token := deployAgentTokenForHost(host)
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/project/terminals/"+sid+"/resize", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := tracectx.Client(8 * time.Second).Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// deleteRemoteTerminal best-effort DELETEs a terminal on the host's
// deploy-agent. Errors are swallowed (the row is flipped 'dead' regardless).
func (s *APIServer) deleteRemoteTerminal(parent context.Context, host, sid string) {
	base, ok := agentBaseURL(host)
	if !ok {
		return
	}
	token := deployAgentTokenForHost(host)
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		base+"/project/terminals/"+sid, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := tracectx.Client(8 * time.Second).Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

// checkTerminalToken authorizes the websocket handshake. When no token is
// configured, auth is disabled (mirrors authMiddleware). Otherwise it accepts,
// in preference order:
//
//  1. the API token via headers (X-API-Key / Authorization: Bearer) — for
//     non-browser clients;
//  2. the API token or a short-lived ticket via Sec-WebSocket-Protocol
//     entries ("adam.token.<tok>" / "adam.ticket.<tkt>") — browsers CAN set
//     subprotocols, unlike Authorization;
//  3. a short-lived, session-bound ticket via ?ticket= (minted by POST
//     /terminals/{sid}/ticket; see terminal_ticket.go);
//  4. DEPRECATED: the raw API token via ?token= — kept so the already-
//     deployed SPA doesn't break, logged so operators can track migration.
func (s *APIServer) checkTerminalToken(r *http.Request, sid string) bool {
	if !s.authTokenConfigured() {
		return true
	}
	if s.validAPIToken(requestHeaderToken(r)) {
		return true
	}
	subToken, subTicket := wsSubprotocolCredential(r)
	if subToken != "" && s.validAPIToken(subToken) {
		return true
	}
	now := time.Now()
	if subTicket != "" && s.verifyTerminalTicket(subTicket, sid, now) {
		return true
	}
	if tkt := strings.TrimSpace(r.URL.Query().Get("ticket")); tkt != "" &&
		s.verifyTerminalTicket(tkt, sid, now) {
		return true
	}
	if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" &&
		s.validAPIToken(token) {
		s.logger.WithField("sid", sid).
			Warn("terminal ws: DEPRECATED ?token= auth used; migrate to POST /terminals/{sid}/ticket")
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// Boot reconciler + periodic reaper. The integration agent calls these from
// NewAPIServer/Start once the routes are wired.
// ─────────────────────────────────────────────────────────────────────

// ReconcileTerminals reconciles evo.terminal_sessions against `tmux ls` once,
// at boot: any row marked 'live' whose tmux session no longer exists is flipped
// to 'dead'. This recovers the table after an apiserver restart/crash where
// some sessions were killed (e.g. host reboot) while the apiserver was down.
// No-op when terminals are disabled or the pool is nil. Safe to call with a
// short-lived context; failures are logged, not returned.
func (s *APIServer) ReconcileTerminals(ctx context.Context) {
	if !terminalsEnabled() || s.evoPool == nil {
		return
	}

	alive := s.listTmuxSessions(ctx)

	// Only reconcile sessions whose project lives on THIS host: a remote
	// session's tmux process runs on the agent, so its absence from the local
	// `tmux ls` says nothing about its liveness.
	rows, err := s.evoPool.Query(ctx,
		`SELECT ts.id
		 FROM evo.terminal_sessions ts
		 JOIN evo.projects p ON p.id = ts.project_id
		 WHERE ts.status = 'live' AND `+localHostSQLPredicate("p.host"),
		localHostSQLArg())
	if err != nil {
		s.logger.WithError(err).Warn("ReconcileTerminals: query live sessions failed")
		return
	}
	var dead []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			s.logger.WithError(err).Warn("ReconcileTerminals: scan failed")
			continue
		}
		if _, ok := alive[id]; !ok {
			dead = append(dead, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.WithError(err).Warn("ReconcileTerminals: rows iteration failed")
		return
	}

	for _, id := range dead {
		if _, err := s.evoPool.Exec(ctx,
			"UPDATE evo.terminal_sessions SET status = 'dead', ended_at = NOW() WHERE id = $1 AND status = 'live'",
			id); err != nil {
			s.logger.WithError(err).WithField("sid", id).Warn("ReconcileTerminals: mark dead failed")
		}
	}
	if len(dead) > 0 {
		s.logger.WithField("count", len(dead)).Info("ReconcileTerminals: marked vanished tmux sessions dead")
	}
}

// StartTerminalReaper runs a ~60s loop that kills tmux sessions ("adam-"…)
// whose project row is gone, and marks any 'live' row 'dead' when its tmux
// session has vanished. It returns immediately, spawning a goroutine that runs
// until ctx is cancelled. No-op when terminals are disabled. The integration
// agent calls this once from Start with the server's lifetime context.
func (s *APIServer) StartTerminalReaper(ctx context.Context) {
	if !terminalsEnabled() || s.evoPool == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		// Run once promptly so a stale session isn't held for a full minute.
		s.reapOnce(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapOnce(ctx)
			}
		}
	}()
}

// reapOnce performs a single reaper pass. Two jobs:
//  1. kill any "adam-" tmux session whose row is gone (project deleted ->
//     CASCADE removed the row, but the tmux session is a host process).
//  2. mark 'live' rows 'dead' when their tmux session has vanished (keeps the
//     table honest between boots, like ReconcileTerminals but ongoing).
func (s *APIServer) reapOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()

	alive := s.listTmuxSessions(ctx)

	// Collect the set of LOCAL ids we still know about. Remote sessions (whose
	// project lives on another host) are deliberately excluded: their tmux
	// process runs on the agent, so the local `tmux ls` is irrelevant to them.
	known := make(map[string]string) // id -> status
	rows, err := s.evoPool.Query(ctx,
		`SELECT ts.id, ts.status
		 FROM evo.terminal_sessions ts
		 JOIN evo.projects p ON p.id = ts.project_id
		 WHERE `+localHostSQLPredicate("p.host"),
		localHostSQLArg())
	if err != nil {
		s.logger.WithError(err).Warn("terminal reaper: query rows failed")
		return
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			continue
		}
		known[id] = status
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logger.WithError(err).Warn("terminal reaper: rows iteration failed")
		return
	}

	// (1) Orphan tmux sessions: alive in tmux, "adam-" prefixed, no row.
	for name := range alive {
		if !strings.HasPrefix(name, "adam-") {
			continue
		}
		if _, ok := known[name]; !ok {
			_ = projectfs.KillSession(name)
			s.logger.WithField("sid", name).Info("terminal reaper: killed orphan tmux session")
		}
	}

	// (2) Live rows whose tmux session vanished -> mark dead.
	for id, status := range known {
		if status != "live" {
			continue
		}
		if _, ok := alive[id]; !ok {
			if _, err := s.evoPool.Exec(ctx,
				"UPDATE evo.terminal_sessions SET status = 'dead', ended_at = NOW() WHERE id = $1 AND status = 'live'",
				id); err != nil {
				s.logger.WithError(err).WithField("sid", id).Warn("terminal reaper: mark dead failed")
			}
		}
	}
}

// listTmuxSessions returns the set of currently-running tmux session names. An
// empty set is returned both when no server is running ("no server running on …"
// or "error connecting …") and on any other failure — callers treat "absent
// from this set" as "not alive", and a transient tmux error shouldn't trigger a
// mass reap, so the reaper only ever kills/marks based on the (1)/(2) logic
// above which both require positive evidence. To be safe against a transient
// failure wiping the table, we return ok=false on error and callers skip.
func (s *APIServer) listTmuxSessions(ctx context.Context) map[string]struct{} {
	out := make(map[string]struct{})
	cmd := exec.CommandContext(ctx, "tmux", "list-sessions", "-F", "#{session_name}")
	stdout, err := cmd.Output()
	if err != nil {
		// `tmux ls` exits non-zero with "no server running" when there are no
		// sessions — that's a legitimate empty set, not an error to surface.
		return out
	}
	sc := bufio.NewScanner(strings.NewReader(string(stdout)))
	for sc.Scan() {
		name := strings.TrimSpace(sc.Text())
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

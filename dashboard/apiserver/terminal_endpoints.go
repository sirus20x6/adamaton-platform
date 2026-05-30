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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

// ptyBackend returns the configured PTY backend ("tmux" or "none"). Anything
// other than "none" (including the empty default) means tmux is enabled.
func ptyBackend() string {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("PTY_BACKEND"))); v == "none" {
		return "none"
	}
	return "tmux"
}

// terminalsEnabled reports whether the tmux backend is active. When false, the
// endpoints respond 503 and the boot reconciler/reaper are no-ops.
func terminalsEnabled() bool { return ptyBackend() != "none" }

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

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// Look up the project to resolve its on-disk path (the tmux working dir).
	var projectPath string
	if err := s.evoPool.QueryRow(ctx,
		"SELECT path FROM evo.projects WHERE id = $1", projectID,
	).Scan(&projectPath); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "project not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "project lookup: "+err.Error())
		return
	}

	sid := "adam-" + uuid.NewString()

	// tmux new-session -d -s <id> -x <cols> -y <rows> -c <project.path> <command>
	// Run the command through the user's login shell so e.g. "htop" or a bare
	// "bash" both work; tmux execs the final argument as the session command.
	newCmd := exec.CommandContext(ctx, "tmux", "new-session", "-d",
		"-s", sid,
		"-x", strconv.Itoa(req.Cols),
		"-y", strconv.Itoa(req.Rows),
		"-c", projectPath,
		req.Command,
	)
	if out, err := newCmd.CombinedOutput(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError,
			"tmux new-session: "+err.Error()+": "+strings.TrimSpace(string(out)))
		return
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
		// The row insert failed after we already spawned tmux — kill the
		// orphan session so we don't leak it.
		s.killTmuxSession(sid)
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, t)
}

// terminalUpgrader upgrades the /terminals/{sid}/ws GET into a websocket. The
// dashboard and the frontend dev server are same-origin via the Caddy / vite
// /evo-api proxy, and the auth token is checked explicitly below, so we accept
// any origin here rather than maintaining an allow-list.
var terminalUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
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
	// Browsers cannot set Authorization/X-API-Key headers on a WS handshake,
	// so the token rides in the query string. Mirror authMiddleware: when a
	// token is configured it must match; when unset, auth is disabled.
	if !s.checkTerminalToken(r) {
		writeEvoErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	sid := mux.Vars(r)["sid"]

	// Confirm the session exists and is live before upgrading, so a bogus sid
	// gets a clean 404 instead of a websocket that immediately dies.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var status string
	err := s.evoPool.QueryRow(ctx,
		"SELECT status FROM evo.terminal_sessions WHERE id = $1", sid,
	).Scan(&status)
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

	conn, err := terminalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response on failure.
		s.logger.WithError(err).Warn("terminalWS: upgrade failed")
		return
	}
	s.bridgeTerminal(sid, conn)
}

// bridgeTerminal wires a PTY running `tmux attach-session -t <sid>` to the
// websocket: pty output -> ws (binary frames), ws input -> pty. It blocks
// until either side closes, then kills the attach client process only (the
// tmux session persists for the next attach).
func (s *APIServer) bridgeTerminal(sid string, conn *websocket.Conn) {
	defer conn.Close()

	// `tmux attach-session` (no -d) gives this connection its own client so
	// resize is per-attach. The session outlives the attach.
	attach := exec.Command("tmux", "attach-session", "-t", sid)
	ptmx, err := pty.Start(attach)
	if err != nil {
		s.logger.WithError(err).WithField("sid", sid).Warn("terminalWS: pty start failed")
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[adam: failed to attach terminal: "+err.Error()+"]\r\n"))
		return
	}

	// Register the live pty so resizeTerminal can drive pty.Setsize on it.
	registerTerminalPTY(sid, ptmx)
	defer unregisterTerminalPTY(sid, ptmx)

	// Ensure the attach client is reaped and the pty closed on the way out.
	// Killing the attach client does NOT kill the tmux session.
	defer func() {
		_ = ptmx.Close()
		if attach.Process != nil {
			_ = attach.Process.Kill()
		}
		_ = attach.Wait()
	}()

	var once sync.Once
	done := make(chan struct{})
	closeDone := func() { once.Do(func() { close(done) }) }

	// pty -> ws. EOF (the attach client exited) ends the bridge.
	go func() {
		defer closeDone()
		buf := make([]byte, 4096)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// ws -> pty. A read error (client closed) ends the bridge.
	go func() {
		defer closeDone()
		for {
			_, msg, rerr := conn.ReadMessage()
			if rerr != nil {
				return
			}
			if len(msg) > 0 {
				if _, werr := ptmx.Write(msg); werr != nil {
					return
				}
			}
		}
	}()

	<-done
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

	// Resize every live attach pty for this session so the in-flight bridge's
	// terminal geometry follows the client's window.
	resizeTerminalPTYs(sid, req.Cols, req.Rows)

	// Also resize the tmux window itself so a fresh attach picks up the new
	// geometry. Best-effort: a missing session here is not fatal to the resize
	// of the live pty above.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resizeCmd := exec.CommandContext(ctx, "tmux", "resize-window", "-t", sid,
		"-x", strconv.Itoa(req.Cols), "-y", strconv.Itoa(req.Rows))
	_ = resizeCmd.Run()

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

	// Kill the tmux session (best-effort: it may already be gone), then flip
	// the row to 'dead'. We mark the row even if the kill found nothing so the
	// UI stops offering an attach.
	s.killTmuxSession(sid)

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
// Live-PTY registry — resizeTerminal needs a handle on the pty owned by the
// in-flight websocket bridge so it can call pty.Setsize. A session can have
// more than one live attach (two browser tabs), so we track a set per sid.
//
// This state is process-global rather than a field on APIServer: the
// integration agent owns server.go's struct definition, and a single apiserver
// process only ever runs one APIServer, so a package-level registry is both
// simpler to wire (no constructor change) and behaviourally identical.
// ─────────────────────────────────────────────────────────────────────

// ptyFile is the slice of *os.File that pty.Setsize needs. pty.Start returns an
// *os.File, which satisfies this.
type ptyFile = *os.File

var (
	termPTYMu sync.Mutex
	termPTYs  = map[string]map[ptyFile]struct{}{}
)

// registerTerminalPTY records a live attach pty for a session.
func registerTerminalPTY(sid string, ptmx ptyFile) {
	termPTYMu.Lock()
	defer termPTYMu.Unlock()
	set := termPTYs[sid]
	if set == nil {
		set = make(map[ptyFile]struct{})
		termPTYs[sid] = set
	}
	set[ptmx] = struct{}{}
}

func unregisterTerminalPTY(sid string, ptmx ptyFile) {
	termPTYMu.Lock()
	defer termPTYMu.Unlock()
	set := termPTYs[sid]
	if set == nil {
		return
	}
	delete(set, ptmx)
	if len(set) == 0 {
		delete(termPTYs, sid)
	}
}

func resizeTerminalPTYs(sid string, cols, rows int) {
	termPTYMu.Lock()
	set := termPTYs[sid]
	ptmxs := make([]ptyFile, 0, len(set))
	for p := range set {
		ptmxs = append(ptmxs, p)
	}
	termPTYMu.Unlock()

	ws := &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
	for _, p := range ptmxs {
		_ = pty.Setsize(p, ws)
	}
}

// killTmuxSession runs `tmux kill-session -t <sid>`, swallowing the
// "session not found" case. Best-effort; never returns an error to the caller.
func (s *APIServer) killTmuxSession(sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "tmux", "kill-session", "-t", sid).Run()
}

// checkTerminalToken mirrors authMiddleware for the websocket handshake, where
// browsers can't set headers. It accepts the token via ?token=, falling back
// to the same header check the middleware uses (so a non-browser client can
// still send a header). When no token is configured, auth is disabled.
func (s *APIServer) checkTerminalToken(r *http.Request) bool {
	if s.config.API.Token == "" {
		return true
	}
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	if token == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			token = strings.TrimSpace(auth[len("Bearer "):])
		}
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.config.API.Token)) == 1
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

	rows, err := s.evoPool.Query(ctx,
		"SELECT id FROM evo.terminal_sessions WHERE status = 'live'")
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

	// Collect the set of ids we still know about.
	known := make(map[string]string) // id -> status
	rows, err := s.evoPool.Query(ctx, "SELECT id, status FROM evo.terminal_sessions")
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
			s.killTmuxSession(name)
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

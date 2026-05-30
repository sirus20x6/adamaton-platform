package apiserver

// Kanban endpoints. Per-project Kanban boards backed by the evo.kanban_*
// tables (boards, columns, cards, comments). This is Phase 4 of the Projects
// feature — boards hang off projects.id, and the same REST surface is called
// by the delegator MCP server over HTTP so an agent can list ready work,
// atomically claim a card, and report its result back.
//
// All reads/writes go through s.evoPool; every handler 503s when the pool is
// nil, matching projects_endpoints.go and datasets_endpoints.go. IDs are
// slug-or-prefix + short-uuid, same flavour as projects_endpoints.go.
//
// The claim handler is the one place correctness depends on concurrency: two
// agents may race to claim the same card. We take a transaction-scoped
// advisory lock keyed on the card id, then do a conditional UPDATE that only
// touches an unclaimed card. An empty update means somebody else won the
// race -> 409. See the Kanban contract in docs/PROJECTS_KANBAN.md.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
)

// ---- wire types ------------------------------------------------------------

// Board mirrors a row in evo.kanban_boards.
type Board struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	CreatedBy *string   `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Column mirrors a row in evo.kanban_columns. is_ready flags the single
// column whose unclaimed cards are surfaced by /ready (agents pull work from
// there).
type Column struct {
	ID       string `json:"id"`
	BoardID  string `json:"board_id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	IsReady  bool   `json:"is_ready"`
}

// Card mirrors a row in evo.kanban_cards. The claim_* / result_* fields carry
// the agent-claim lifecycle: an unclaimed card in the ready column can be
// claimed (-> claim_token), moved, then completed (-> done) or released
// (-> unclaimed). claim_token is never serialized — it's a capability handed
// back to the claimer on /claim and required on /move, /complete, /release.
type Card struct {
	ID            string     `json:"id"`
	ColumnID      string     `json:"column_id"`
	BoardID       string     `json:"board_id"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	Priority      string     `json:"priority"`
	Difficulty    string     `json:"difficulty"`
	Position      int        `json:"position"`
	ClaimStatus   string     `json:"claim_status"`
	ClaimedBy     *string    `json:"claimed_by,omitempty"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	ResultTaskID  *string    `json:"result_task_id,omitempty"`
	ResultSummary *string    `json:"result_summary,omitempty"`
	ResultPRURL   *string    `json:"result_pr_url,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Comment mirrors a row in evo.kanban_comments.
type Comment struct {
	ID        string    `json:"id"`
	CardID    string    `json:"card_id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// ---- request bodies --------------------------------------------------------

type boardCreateRequest struct {
	Name string `json:"name"`
}

type cardCreateRequest struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	ColumnID   string `json:"column_id"`
	Priority   string `json:"priority"`
	Difficulty string `json:"difficulty"`
}

type cardClaimRequest struct {
	AgentID string `json:"agent_id"`
}

type cardMoveRequest struct {
	TargetColumnID string `json:"target_column_id"`
	ClaimToken     string `json:"claim_token"`
	Position       *int   `json:"position"`
}

type cardCompleteRequest struct {
	ClaimToken    string `json:"claim_token"`
	ResultSummary string `json:"result_summary"`
	ResultTaskID  string `json:"result_task_id"`
	ResultPRURL   string `json:"result_pr_url"`
}

type cardReleaseRequest struct {
	ClaimToken string `json:"claim_token"`
}

type cardCommentRequest struct {
	Author string `json:"author"`
	Text   string `json:"text"`
}

// ---- response envelopes ----------------------------------------------------

// boardWithColumns is the POST /boards reply and the inner shape of GET
// /boards/{bid} (which also carries cards).
type boardDetail struct {
	Board   Board    `json:"board"`
	Columns []Column `json:"columns"`
	Cards   []Card   `json:"cards,omitempty"`
}

// claimResult is the /claim reply: the (now claimed) card plus the capability
// token the agent must present for subsequent moves/completion.
type claimResult struct {
	Card       Card   `json:"card"`
	ClaimToken string `json:"claim_token"`
}

// ---- column selection lists ------------------------------------------------

const cardColumns = `id, column_id, board_id, title, body, priority, difficulty,
	position, claim_status, claimed_by, claimed_at, result_task_id,
	result_summary, result_pr_url, created_at, updated_at`

// ---- registration ----------------------------------------------------------

func (s *APIServer) registerKanbanEndpoints(api *mux.Router) {
	api.HandleFunc("/projects/{id}/kanban/boards", s.listKanbanBoards).Methods("GET")
	api.HandleFunc("/projects/{id}/kanban/boards", s.createKanbanBoard).Methods("POST")
	api.HandleFunc("/kanban/boards/{bid}", s.getKanbanBoard).Methods("GET")
	api.HandleFunc("/kanban/boards/{bid}/cards", s.createKanbanCard).Methods("POST")
	api.HandleFunc("/kanban/boards/{bid}/ready", s.listReadyCards).Methods("GET")
	api.HandleFunc("/kanban/cards/{cid}/claim", s.claimKanbanCard).Methods("POST")
	api.HandleFunc("/kanban/cards/{cid}/move", s.moveKanbanCard).Methods("POST")
	api.HandleFunc("/kanban/cards/{cid}/complete", s.completeKanbanCard).Methods("POST")
	api.HandleFunc("/kanban/cards/{cid}/release", s.releaseKanbanCard).Methods("POST")
	api.HandleFunc("/kanban/cards/{cid}/comment", s.commentKanbanCard).Methods("POST")
}

// ---- boards ----------------------------------------------------------------

const boardsListSQL = `
SELECT id, project_id, name, created_by, created_at
FROM evo.kanban_boards
WHERE project_id = $1
ORDER BY created_at DESC
LIMIT 500`

func (s *APIServer) listKanbanBoards(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	projectID := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.evoPool.Query(ctx, boardsListSQL, projectID)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Board, 0)
	for rows.Next() {
		var b Board
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.Name, &b.CreatedBy, &b.CreatedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

// seedColumns is the default 5-column layout for a fresh board. Ready is the
// one column whose unclaimed cards /ready surfaces.
var seedColumns = []struct {
	Name    string
	IsReady bool
}{
	{"Backlog", false},
	{"Ready", true},
	{"In Progress", false},
	{"Review", false},
	{"Done", false},
}

func (s *APIServer) createKanbanBoard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	projectID := mux.Vars(r)["id"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req boardCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeEvoErr(w, http.StatusBadRequest, "name is required")
		return
	}

	slug := slugify(req.Name)
	if slug == "" {
		slug = "board"
	}
	boardID := slug + "-" + uuid.NewString()[:8]

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	tx, err := s.evoPool.Begin(ctx)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "begin: "+err.Error())
		return
	}
	defer tx.Rollback(ctx)

	var b Board
	const insertBoardSQL = `
INSERT INTO evo.kanban_boards (id, project_id, name)
VALUES ($1, $2, $3)
RETURNING id, project_id, name, created_by, created_at`
	if err := tx.QueryRow(ctx, insertBoardSQL, boardID, projectID, req.Name).Scan(
		&b.ID, &b.ProjectID, &b.Name, &b.CreatedBy, &b.CreatedAt); err != nil {
		// 23503 = foreign_key_violation: the project_id doesn't exist.
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusNotFound, "project not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert board: "+err.Error())
		return
	}

	const insertColSQL = `
INSERT INTO evo.kanban_columns (id, board_id, name, position, is_ready)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, board_id, name, position, is_ready`
	cols := make([]Column, 0, len(seedColumns))
	for i, sc := range seedColumns {
		colID := "col-" + uuid.NewString()[:8]
		var c Column
		if err := tx.QueryRow(ctx, insertColSQL, colID, boardID, sc.Name, i, sc.IsReady).Scan(
			&c.ID, &c.BoardID, &c.Name, &c.Position, &c.IsReady); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "insert column: "+err.Error())
			return
		}
		cols = append(cols, c)
	}

	if err := tx.Commit(ctx); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, boardDetail{Board: b, Columns: cols})
}

const boardGetSQL = `
SELECT id, project_id, name, created_by, created_at
FROM evo.kanban_boards
WHERE id = $1`

const boardColumnsSQL = `
SELECT id, board_id, name, position, is_ready
FROM evo.kanban_columns
WHERE board_id = $1
ORDER BY position ASC`

const boardCardsSQL = `
SELECT ` + cardColumns + `
FROM evo.kanban_cards
WHERE board_id = $1
ORDER BY position ASC, created_at ASC`

func (s *APIServer) getKanbanBoard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	boardID := mux.Vars(r)["bid"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b Board
	if err := s.evoPool.QueryRow(ctx, boardGetSQL, boardID).Scan(
		&b.ID, &b.ProjectID, &b.Name, &b.CreatedBy, &b.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "board not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}

	colRows, err := s.evoPool.Query(ctx, boardColumnsSQL, boardID)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "columns: "+err.Error())
		return
	}
	defer colRows.Close()
	cols := make([]Column, 0)
	for colRows.Next() {
		var c Column
		if err := colRows.Scan(&c.ID, &c.BoardID, &c.Name, &c.Position, &c.IsReady); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan column: "+err.Error())
			return
		}
		cols = append(cols, c)
	}
	if err := colRows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "columns rows: "+err.Error())
		return
	}
	colRows.Close()

	cardRows, err := s.evoPool.Query(ctx, boardCardsSQL, boardID)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "cards: "+err.Error())
		return
	}
	defer cardRows.Close()
	cards := make([]Card, 0)
	for cardRows.Next() {
		c, err := scanCard(cardRows)
		if err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan card: "+err.Error())
			return
		}
		cards = append(cards, c)
	}
	if err := cardRows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "cards rows: "+err.Error())
		return
	}

	writeEvoJSON(w, boardDetail{Board: b, Columns: cols, Cards: cards})
}

// ---- cards -----------------------------------------------------------------

// scanCard reads a card row in cardColumns order. It takes the shared
// rowScanner interface (declared in skills_endpoints.go) — the common scan
// surface of pgx.Row and pgx.Rows — so it works for both a single QueryRow and
// a streamed Query result.
func scanCard(row rowScanner) (Card, error) {
	var c Card
	err := row.Scan(
		&c.ID, &c.ColumnID, &c.BoardID, &c.Title, &c.Body, &c.Priority, &c.Difficulty,
		&c.Position, &c.ClaimStatus, &c.ClaimedBy, &c.ClaimedAt, &c.ResultTaskID,
		&c.ResultSummary, &c.ResultPRURL, &c.CreatedAt, &c.UpdatedAt,
	)
	return c, err
}

func (s *APIServer) createKanbanCard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	boardID := mux.Vars(r)["bid"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	var req cardCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.ColumnID = strings.TrimSpace(req.ColumnID)
	req.Priority = strings.TrimSpace(req.Priority)
	req.Difficulty = strings.TrimSpace(req.Difficulty)
	if req.Title == "" {
		writeEvoErr(w, http.StatusBadRequest, "title is required")
		return
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if req.Difficulty == "" {
		req.Difficulty = "medium"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	// Resolve the target column. Default to this board's Backlog (the
	// lowest-position non-ready column, i.e. position 0) when the caller
	// doesn't name one. We also verify any caller-supplied column belongs to
	// this board so a card can't be filed onto a foreign board's column.
	if req.ColumnID == "" {
		const backlogSQL = `
SELECT id FROM evo.kanban_columns
WHERE board_id = $1
ORDER BY position ASC
LIMIT 1`
		if err := s.evoPool.QueryRow(ctx, backlogSQL, boardID).Scan(&req.ColumnID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeEvoErr(w, http.StatusNotFound, "board has no columns")
				return
			}
			writeEvoErr(w, http.StatusInternalServerError, "backlog lookup: "+err.Error())
			return
		}
	} else {
		var ok bool
		if err := s.evoPool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM evo.kanban_columns WHERE id = $1 AND board_id = $2)",
			req.ColumnID, boardID).Scan(&ok); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "column lookup: "+err.Error())
			return
		}
		if !ok {
			writeEvoErr(w, http.StatusBadRequest, "column_id does not belong to this board")
			return
		}
	}

	cardID := "card-" + uuid.NewString()[:8]

	// position = append to the end of the target column.
	const insertCardSQL = `
INSERT INTO evo.kanban_cards
	(id, column_id, board_id, title, body, priority, difficulty, position)
VALUES ($1, $2, $3, $4, $5, $6, $7,
	COALESCE((SELECT MAX(position) + 1 FROM evo.kanban_cards WHERE column_id = $2), 0))
RETURNING ` + cardColumns
	c, err := scanCard(s.evoPool.QueryRow(ctx, insertCardSQL,
		cardID, req.ColumnID, boardID, req.Title, req.Body, req.Priority, req.Difficulty))
	if err != nil {
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusNotFound, "board or column not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert card: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, c)
}

const readyCardsSQL = `
SELECT ` + cardColumns + `
FROM evo.kanban_cards
WHERE board_id = $1
  AND claim_status = 'unclaimed'
  AND column_id IN (SELECT id FROM evo.kanban_columns WHERE board_id = $1 AND is_ready = true)
ORDER BY position ASC, created_at ASC`

func (s *APIServer) listReadyCards(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	boardID := mux.Vars(r)["bid"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.evoPool.Query(ctx, readyCardsSQL, boardID)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Card, 0)
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

// claimKanbanCard atomically claims an unclaimed card. The advisory lock
// serializes concurrent claims of the same card id within the transaction;
// the WHERE claim_status = 'unclaimed' guard makes the UPDATE itself the
// arbiter — exactly one racer's UPDATE affects a row, everyone else gets an
// empty result and a 409.
func (s *APIServer) claimKanbanCard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	cardID := mux.Vars(r)["cid"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req cardClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.AgentID == "" {
		writeEvoErr(w, http.StatusBadRequest, "agent_id is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	tx, err := s.evoPool.Begin(ctx)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "begin: "+err.Error())
		return
	}
	defer tx.Rollback(ctx)

	// Transaction-scoped advisory lock keyed on the card id. Released
	// automatically at COMMIT/ROLLBACK — no explicit unlock needed.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", cardID); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "lock: "+err.Error())
		return
	}

	const claimSQL = `
UPDATE evo.kanban_cards
SET claim_status = 'claimed',
    claimed_by   = $2,
    claim_token  = gen_random_uuid(),
    claimed_at   = now(),
    updated_at   = now()
WHERE id = $1 AND claim_status = 'unclaimed'
RETURNING ` + cardColumns + `, claim_token`
	var c Card
	var token string
	err = tx.QueryRow(ctx, claimSQL, cardID, req.AgentID).Scan(
		&c.ID, &c.ColumnID, &c.BoardID, &c.Title, &c.Body, &c.Priority, &c.Difficulty,
		&c.Position, &c.ClaimStatus, &c.ClaimedBy, &c.ClaimedAt, &c.ResultTaskID,
		&c.ResultSummary, &c.ResultPRURL, &c.CreatedAt, &c.UpdatedAt, &token,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Either the card doesn't exist or it's already claimed.
			// Disambiguate so the caller gets 404 vs 409.
			var exists bool
			if e := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM evo.kanban_cards WHERE id = $1)", cardID).Scan(&exists); e == nil && !exists {
				writeEvoErr(w, http.StatusNotFound, "card not found")
				return
			}
			writeEvoErr(w, http.StatusConflict, "card already claimed")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "claim: "+err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}
	writeEvoJSON(w, claimResult{Card: c, ClaimToken: token})
}

// requireClaimToken loads the card's current claim state and verifies the
// supplied token. Returns false (and writes the response) when the card is
// missing, is claimed but the token is absent/wrong. A token is only required
// when the card is actually claimed; an unclaimed card can be moved freely.
func (s *APIServer) requireClaimToken(ctx context.Context, w http.ResponseWriter, cardID, token string) bool {
	const sql = `SELECT claim_status, claim_token FROM evo.kanban_cards WHERE id = $1`
	var status string
	var dbToken *string
	if err := s.evoPool.QueryRow(ctx, sql, cardID).Scan(&status, &dbToken); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "card not found")
			return false
		}
		writeEvoErr(w, http.StatusInternalServerError, "card lookup: "+err.Error())
		return false
	}
	if status == "unclaimed" {
		return true
	}
	if dbToken == nil || token == "" || token != *dbToken {
		writeEvoErr(w, http.StatusForbidden, "claim_token does not match")
		return false
	}
	return true
}

func (s *APIServer) moveKanbanCard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	cardID := mux.Vars(r)["cid"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req cardMoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.TargetColumnID = strings.TrimSpace(req.TargetColumnID)
	req.ClaimToken = strings.TrimSpace(req.ClaimToken)
	if req.TargetColumnID == "" {
		writeEvoErr(w, http.StatusBadRequest, "target_column_id is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	if !s.requireClaimToken(ctx, w, cardID, req.ClaimToken) {
		return
	}

	// The target column must belong to the same board as the card.
	var ok bool
	if err := s.evoPool.QueryRow(ctx, `
SELECT EXISTS(
	SELECT 1 FROM evo.kanban_columns col
	JOIN evo.kanban_cards card ON card.board_id = col.board_id
	WHERE col.id = $1 AND card.id = $2)`, req.TargetColumnID, cardID).Scan(&ok); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "column lookup: "+err.Error())
		return
	}
	if !ok {
		writeEvoErr(w, http.StatusBadRequest, "target_column_id does not belong to the card's board")
		return
	}

	// position: explicit value when supplied, otherwise append to the end of
	// the target column.
	var moveSQL string
	var args []any
	if req.Position != nil {
		moveSQL = `
UPDATE evo.kanban_cards
SET column_id = $2, position = $3, updated_at = now()
WHERE id = $1
RETURNING ` + cardColumns
		args = []any{cardID, req.TargetColumnID, *req.Position}
	} else {
		moveSQL = `
UPDATE evo.kanban_cards
SET column_id = $2,
    position  = COALESCE((SELECT MAX(position) + 1 FROM evo.kanban_cards WHERE column_id = $2), 0),
    updated_at = now()
WHERE id = $1
RETURNING ` + cardColumns
		args = []any{cardID, req.TargetColumnID}
	}
	c, err := scanCard(s.evoPool.QueryRow(ctx, moveSQL, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "card not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "move: "+err.Error())
		return
	}
	writeEvoJSON(w, c)
}

func (s *APIServer) completeKanbanCard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	cardID := mux.Vars(r)["cid"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	var req cardCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.ClaimToken = strings.TrimSpace(req.ClaimToken)
	if req.ClaimToken == "" {
		writeEvoErr(w, http.StatusBadRequest, "claim_token is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	if !s.requireClaimToken(ctx, w, cardID, req.ClaimToken) {
		return
	}

	// Resolve the board's Done column (highest position) and stamp the result
	// fields. Empty result_* values are stored as NULL.
	const completeSQL = `
UPDATE evo.kanban_cards
SET claim_status   = 'done',
    column_id      = (
        SELECT id FROM evo.kanban_columns
        WHERE board_id = evo.kanban_cards.board_id
        ORDER BY position DESC LIMIT 1),
    position       = COALESCE((
        SELECT MAX(position) + 1 FROM evo.kanban_cards inner_c
        WHERE inner_c.column_id = (
            SELECT id FROM evo.kanban_columns
            WHERE board_id = evo.kanban_cards.board_id
            ORDER BY position DESC LIMIT 1)), 0),
    result_summary = NULLIF($2, ''),
    result_task_id = NULLIF($3, ''),
    result_pr_url  = NULLIF($4, ''),
    updated_at     = now()
WHERE id = $1
RETURNING ` + cardColumns
	c, err := scanCard(s.evoPool.QueryRow(ctx, completeSQL,
		cardID, req.ResultSummary, req.ResultTaskID, req.ResultPRURL))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "card not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "complete: "+err.Error())
		return
	}
	writeEvoJSON(w, c)
}

func (s *APIServer) releaseKanbanCard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	cardID := mux.Vars(r)["cid"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req cardReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.ClaimToken = strings.TrimSpace(req.ClaimToken)
	if req.ClaimToken == "" {
		writeEvoErr(w, http.StatusBadRequest, "claim_token is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	if !s.requireClaimToken(ctx, w, cardID, req.ClaimToken) {
		return
	}

	const releaseSQL = `
UPDATE evo.kanban_cards
SET claim_status = 'unclaimed',
    claimed_by   = NULL,
    claim_token  = NULL,
    claimed_at   = NULL,
    updated_at   = now()
WHERE id = $1
RETURNING ` + cardColumns
	c, err := scanCard(s.evoPool.QueryRow(ctx, releaseSQL, cardID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "card not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "release: "+err.Error())
		return
	}
	writeEvoJSON(w, c)
}

// ---- comments --------------------------------------------------------------

func (s *APIServer) commentKanbanCard(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	cardID := mux.Vars(r)["cid"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	var req cardCommentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Author = strings.TrimSpace(req.Author)
	req.Text = strings.TrimSpace(req.Text)
	if req.Author == "" {
		writeEvoErr(w, http.StatusBadRequest, "author is required")
		return
	}
	if req.Text == "" {
		writeEvoErr(w, http.StatusBadRequest, "text is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	commentID := "cmt-" + uuid.NewString()[:8]
	const insertSQL = `
INSERT INTO evo.kanban_comments (id, card_id, author, text)
VALUES ($1, $2, $3, $4)
RETURNING id, card_id, author, text, created_at`
	var cm Comment
	if err := s.evoPool.QueryRow(ctx, insertSQL, commentID, cardID, req.Author, req.Text).Scan(
		&cm.ID, &cm.CardID, &cm.Author, &cm.Text, &cm.CreatedAt); err != nil {
		// 23503 = foreign_key_violation: the card_id doesn't exist.
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusNotFound, "card not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert comment: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, cm)
}

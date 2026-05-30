package apiserver

// DB-backed kanban orchestration tests (card-1cb6e76c). These lock in the
// guarantees the delegator MCP server depends on:
//   - concurrent claim of one card -> exactly one 200 + one 409;
//   - move / complete require the claim_token once a card is claimed;
//   - a wrong / missing token is rejected with 403.
//
// They construct the APIServer struct directly (logger/config/router/
// evoPool) and drive the real handlers through httptest against the local
// migrated evo DB — no Temporal boot. Skips when the DB is unavailable.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// serveKanbanConcurrent is a race-safe request driver for the concurrency
// test. Unlike the shared serveVia helper it does NOT mutate s.router —
// each call builds its own throwaway router so N goroutines can drive the
// same handlers in parallel without a data race on the APIServer struct.
func serveKanbanConcurrent(s *APIServer, method, target, body string) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	api := router.PathPrefix("/api/v1").Subrouter()
	s.registerKanbanEndpoints(api)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// kanbanFixture is a freshly-seeded board: a project, a board with the
// default 5 columns, the ready column id, and one unclaimed card sitting
// in the ready column. Cleanup tears the whole thing down.
type kanbanFixture struct {
	projectID string
	boardID   string
	readyCol  string
	doneCol   string
	cardID    string
}

func seedKanban(t *testing.T, pool *pgxpool.Pool) kanbanFixture {
	t.Helper()
	ctx := context.Background()

	projectID := "wf5-proj-" + uuid.NewString()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO evo.projects (id, path, display_name, type)
		VALUES ($1, $2, $1, 'folder')`,
		projectID, "/tmp/wf5-kanban-"+uuid.NewString()[:8])
	require.NoError(t, err)

	boardID := "wf5-board-" + uuid.NewString()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO evo.kanban_boards (id, project_id, name)
		VALUES ($1, $2, 'wf5 board')`, boardID, projectID)
	require.NoError(t, err)

	// Two columns are enough: a ready column (cards pulled from here) and a
	// done column (highest position, where complete moves cards).
	readyCol := "col-" + uuid.NewString()[:8]
	doneCol := "col-" + uuid.NewString()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO evo.kanban_columns (id, board_id, name, position, is_ready)
		VALUES ($1, $3, 'Ready', 0, true), ($2, $3, 'Done', 1, false)`,
		readyCol, doneCol, boardID)
	require.NoError(t, err)

	cardID := "card-" + uuid.NewString()[:8]
	_, err = pool.Exec(ctx, `
		INSERT INTO evo.kanban_cards (id, column_id, board_id, title, body, priority, difficulty, position)
		VALUES ($1, $2, $3, 'wf5 card', 'body', 'normal', 'medium', 0)`,
		cardID, readyCol, boardID)
	require.NoError(t, err)

	t.Cleanup(func() {
		c := context.Background()
		// CASCADE from board removes columns + cards; project removal is
		// belt-and-suspenders.
		_, _ = pool.Exec(c, `DELETE FROM evo.kanban_boards WHERE id = $1`, boardID)
		_, _ = pool.Exec(c, `DELETE FROM evo.projects WHERE id = $1`, projectID)
	})

	return kanbanFixture{
		projectID: projectID,
		boardID:   boardID,
		readyCol:  readyCol,
		doneCol:   doneCol,
		cardID:    cardID,
	}
}

// TestKanbanClaim_concurrentExactlyOnce is the headline guarantee: N
// goroutines race to claim the same unclaimed card; exactly one gets a
// 200 (with a token), and every other gets a 409. No 500s, no double
// grants.
func TestKanbanClaim_concurrentExactlyOnce(t *testing.T) {
	s := newDBTestServer(t)
	fx := seedKanban(t, s.evoPool)

	const racers = 8
	var wg sync.WaitGroup
	codes := make([]int, racers)
	tokens := make([]string, racers)

	wg.Add(racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"agent_id":"racer-%d"}`, i)
			<-start // line everyone up so the claims truly overlap
			rr := serveKanbanConcurrent(s, http.MethodPost,
				"/api/v1/kanban/cards/"+fx.cardID+"/claim", body)
			codes[i] = rr.Code
			if rr.Code == http.StatusOK {
				var res claimResult
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
				tokens[i] = res.ClaimToken
			}
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, conflict, won int
	for i := 0; i < racers; i++ {
		switch codes[i] {
		case http.StatusOK:
			ok++
			require.NotEmpty(t, tokens[i], "winner must receive a claim_token")
			won = i
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("racer %d got unexpected status %d", i, codes[i])
		}
	}
	require.Equal(t, 1, ok, "exactly one claim must succeed")
	require.Equal(t, racers-1, conflict, "every loser must get 409")

	// The DB must agree: the card is claimed by exactly the winner's agent.
	var status, claimedBy string
	require.NoError(t, s.evoPool.QueryRow(context.Background(),
		`SELECT claim_status, claimed_by FROM evo.kanban_cards WHERE id = $1`, fx.cardID).
		Scan(&status, &claimedBy))
	require.Equal(t, "claimed", status)
	require.Equal(t, fmt.Sprintf("racer-%d", won), claimedBy)
}

// TestKanbanMove_requiresClaimToken proves a claimed card cannot be moved
// without the token: a missing token -> 400 (the handler requires it for
// move only via requireClaimToken's mismatch path -> 403 when claimed),
// a wrong token -> 403, and the correct token -> 200.
func TestKanbanMove_requiresClaimToken(t *testing.T) {
	s := newDBTestServer(t)
	fx := seedKanban(t, s.evoPool)

	// Claim the card to obtain a token.
	token := claimCard(t, s, fx.cardID, "mover")

	// Wrong token -> 403.
	rr := serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+fx.cardID+"/move",
		fmt.Sprintf(`{"target_column_id":%q,"claim_token":"not-the-token"}`, fx.doneCol))
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())

	// Empty token on a claimed card -> 403 (token mismatch, claimed != unclaimed).
	rr = serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+fx.cardID+"/move",
		fmt.Sprintf(`{"target_column_id":%q,"claim_token":""}`, fx.doneCol))
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())

	// Correct token -> 200 and the card lands in the done column.
	rr = serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+fx.cardID+"/move",
		fmt.Sprintf(`{"target_column_id":%q,"claim_token":%q}`, fx.doneCol, token))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var moved Card
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &moved))
	require.Equal(t, fx.doneCol, moved.ColumnID)
}

// TestKanbanComplete_requiresClaimToken proves complete demands the token:
// missing -> 400 (explicit "claim_token is required" guard), wrong -> 403,
// correct -> 200 with claim_status flipped to done.
func TestKanbanComplete_requiresClaimToken(t *testing.T) {
	s := newDBTestServer(t)
	fx := seedKanban(t, s.evoPool)
	token := claimCard(t, s, fx.cardID, "finisher")

	// Missing token -> 400 (complete has an explicit non-empty guard).
	rr := serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+fx.cardID+"/complete", `{"result_summary":"done"}`)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "claim_token is required")

	// Wrong token -> 403.
	rr = serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+fx.cardID+"/complete",
		`{"claim_token":"wrong","result_summary":"done"}`)
	require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())

	// Correct token -> 200, card done + result recorded.
	rr = serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+fx.cardID+"/complete",
		fmt.Sprintf(`{"claim_token":%q,"result_summary":"shipped","result_pr_url":"http://pr/1"}`, token))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var done Card
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &done))
	require.Equal(t, "done", done.ClaimStatus)
	require.NotNil(t, done.ResultSummary)
	require.Equal(t, "shipped", *done.ResultSummary)
}

// claimCard claims a card via the real handler and returns the token.
func claimCard(t *testing.T, s *APIServer, cardID, agent string) string {
	t.Helper()
	rr := serveVia(s, s.registerKanbanEndpoints, http.MethodPost,
		"/api/v1/kanban/cards/"+cardID+"/claim",
		fmt.Sprintf(`{"agent_id":%q}`, agent))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var res claimResult
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	require.NotEmpty(t, res.ClaimToken)
	return res.ClaimToken
}

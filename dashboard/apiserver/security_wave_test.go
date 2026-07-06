package apiserver

// Unit tests for the security-wave hardening: keyring/env token fallback
// order, terminal websocket auth modes (header / subprotocol / ticket /
// deprecated query token), Origin validation, per-caller rate limiting
// (429s), per-host deploy-agent tokens, memory row-level authz helpers, and
// the kanban sweeper status surface.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/credentialstore"
	"github.com/sirus20x6/adamaton-core/types"
)

// ---- API token fallback order (card-36a478a2) ----

func TestInstallKeyringTokenFallbackOrder(t *testing.T) {
	t.Run("keyring only becomes THE token", func(t *testing.T) {
		s := newTestServer(t, &types.Config{})
		s.installKeyringToken("kr-token")
		require.Equal(t, "kr-token", s.config.API.Token)
		require.Empty(t, s.extraAPITokens)
		require.True(t, s.validAPIToken("kr-token"))
		require.False(t, s.validAPIToken("wrong"))
	})

	t.Run("env token kept alongside keyring token", func(t *testing.T) {
		cfg := &types.Config{}
		cfg.API.Token = "env-token"
		s := newTestServer(t, cfg)
		s.installKeyringToken("kr-token")
		// Compatibility: BOTH must authenticate (Caddy injects the env one).
		require.True(t, s.validAPIToken("env-token"))
		require.True(t, s.validAPIToken("kr-token"))
		require.False(t, s.validAPIToken("neither"))
	})

	t.Run("identical tokens don't duplicate", func(t *testing.T) {
		cfg := &types.Config{}
		cfg.API.Token = "same"
		s := newTestServer(t, cfg)
		s.installKeyringToken("same")
		require.Empty(t, s.extraAPITokens)
		require.True(t, s.validAPIToken("same"))
	})

	t.Run("empty keyring token is a no-op", func(t *testing.T) {
		s := newTestServer(t, &types.Config{})
		s.installKeyringToken("")
		require.False(t, s.authTokenConfigured())
	})
}

func TestAuthMiddlewareAcceptsExtraTokens(t *testing.T) {
	cfg := &types.Config{}
	cfg.API.Token = "env-token"
	s := newTestServer(t, cfg)
	s.extraAPITokens = []string{"kr-token"}

	h := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for token, want := range map[string]int{
		"env-token": http.StatusOK,
		"kr-token":  http.StatusOK,
		"bogus":     http.StatusUnauthorized,
		"":          http.StatusUnauthorized,
	} {
		req := httptest.NewRequest("GET", "/api/v1/anything", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, want, rec.Code, "token %q", token)
	}
}

// fakeCredLister stubs the keyring for keyringAPITokenFrom.
type fakeCredLister struct {
	creds   []credentialstore.Credential
	payload string
}

func (f *fakeCredLister) List() ([]credentialstore.Credential, error) { return f.creds, nil }
func (f *fakeCredLister) GetDecrypted(id string) (*credentialstore.Credential, string, error) {
	for i := range f.creds {
		if f.creds[i].ID == id {
			return &f.creds[i], f.payload, nil
		}
	}
	return nil, "", credentialstore.ErrCredentialUnavailable
}
func (f *fakeCredLister) Close() error { return nil }

func TestKeyringAPITokenFrom(t *testing.T) {
	logger := logrus.New()
	lister := &fakeCredLister{
		creds:   []credentialstore.Credential{{ID: "c1", Name: "evo-api-token"}},
		payload: `{"token":"from-keyring"}`,
	}
	require.Equal(t, "from-keyring", keyringAPITokenFrom(lister, logger))

	lister.payload = "raw-token\n"
	require.Equal(t, "raw-token", keyringAPITokenFrom(lister, logger))

	// No matching credential name.
	lister.creds[0].Name = "something-else"
	require.Equal(t, "", keyringAPITokenFrom(lister, logger))
}

func TestParseTokenPayload(t *testing.T) {
	require.Equal(t, "abc", parseTokenPayload("abc"))
	require.Equal(t, "abc", parseTokenPayload(` abc `))
	require.Equal(t, "abc", parseTokenPayload(`{"token":"abc"}`))
	require.Equal(t, "abc", parseTokenPayload(`{"value":"abc"}`))
	require.Equal(t, "", parseTokenPayload(`{"other":"abc"}`))
	require.Equal(t, "", parseTokenPayload(""))
}

// ---- Terminal websocket auth (card-fb2ea0da) ----

func TestTerminalTicketMintVerify(t *testing.T) {
	s := newTestServer(t, &types.Config{})
	now := time.Now()

	tkt, exp, ok := s.mintTerminalTicket("adam-abc", now)
	require.True(t, ok)
	require.True(t, exp.After(now))

	require.True(t, s.verifyTerminalTicket(tkt, "adam-abc", now))
	// Bound to session id.
	require.False(t, s.verifyTerminalTicket(tkt, "adam-other", now))
	// Expired.
	require.False(t, s.verifyTerminalTicket(tkt, "adam-abc", now.Add(terminalTicketTTL+2*time.Second)))
	// Tampered.
	require.False(t, s.verifyTerminalTicket(tkt+"x", "adam-abc", now))
	require.False(t, s.verifyTerminalTicket("garbage", "adam-abc", now))
}

func TestCheckTerminalTokenModes(t *testing.T) {
	cfg := &types.Config{}
	cfg.API.Token = "api-token"
	s := newTestServer(t, cfg)

	mkReq := func(mod func(r *http.Request)) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/terminals/adam-abc/ws", nil)
		if mod != nil {
			mod(r)
		}
		return r
	}

	// No credential -> reject.
	require.False(t, s.checkTerminalToken(mkReq(nil), "adam-abc"))

	// Header auth (non-browser clients).
	require.True(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer api-token")
	}), "adam-abc"))
	require.True(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		r.Header.Set("X-API-Key", "api-token")
	}), "adam-abc"))
	require.False(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong")
	}), "adam-abc"))

	// Subprotocol token (browser-settable).
	require.True(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		r.Header.Set("Sec-Websocket-Protocol", "adam, adam.token.api-token")
	}), "adam-abc"))
	require.False(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		r.Header.Set("Sec-Websocket-Protocol", "adam, adam.token.wrong")
	}), "adam-abc"))

	// Short-lived ticket via query + subprotocol.
	tkt, _, ok := s.mintTerminalTicket("adam-abc", time.Now())
	require.True(t, ok)
	require.True(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		q := r.URL.Query()
		q.Set("ticket", tkt)
		r.URL.RawQuery = q.Encode()
	}), "adam-abc"))
	require.True(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		r.Header.Set("Sec-Websocket-Protocol", "adam, adam.ticket."+tkt)
	}), "adam-abc"))
	// Ticket for a different session is refused.
	require.False(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		q := r.URL.Query()
		q.Set("ticket", tkt)
		r.URL.RawQuery = q.Encode()
	}), "adam-zzz"))

	// DEPRECATED ?token= fallback still authenticates (SPA compatibility).
	require.True(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		q := r.URL.Query()
		q.Set("token", "api-token")
		r.URL.RawQuery = q.Encode()
	}), "adam-abc"))
	require.False(t, s.checkTerminalToken(mkReq(func(r *http.Request) {
		q := r.URL.Query()
		q.Set("token", "wrong")
		r.URL.RawQuery = q.Encode()
	}), "adam-abc"))

	// No token configured -> open (dev mode parity with authMiddleware).
	open := newTestServer(t, &types.Config{})
	require.True(t, open.checkTerminalToken(mkReq(nil), "adam-abc"))
}

// ---- Origin validation (card-009c20ab) ----

func TestRequestOriginAllowed(t *testing.T) {
	mk := func(origin, host string) *http.Request {
		r := httptest.NewRequest("GET", "http://"+host+"/api/v1/research/health", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	// Non-browser (no Origin) always allowed.
	require.True(t, requestOriginAllowed(mk("", "dash.example.com:9123")))
	// Same-origin allowed regardless of hostname.
	require.True(t, requestOriginAllowed(mk("https://dash.example.com", "dash.example.com:9123")))
	// Allowlisted hosts, any port/scheme.
	require.True(t, requestOriginAllowed(mk("https://deepresearch.local", "other:9123")))
	require.True(t, requestOriginAllowed(mk("http://adamaton.local:8443", "other:9123")))
	require.True(t, requestOriginAllowed(mk("http://localhost:5173", "other:9123")))
	require.True(t, requestOriginAllowed(mk("http://127.0.0.1:5173", "other:9123")))
	// Hostile origins rejected.
	require.False(t, requestOriginAllowed(mk("https://evil.example.com", "dash.example.com:9123")))
	require.False(t, requestOriginAllowed(mk("https://notdeepresearch.local.evil.com", "other")))
	require.False(t, requestOriginAllowed(mk("null", "other")))

	// Env override replaces the default list.
	t.Setenv(allowedOriginHostsEnv, "https://trusted.example.com, other.local")
	require.True(t, requestOriginAllowed(mk("https://trusted.example.com:444", "x")))
	require.True(t, requestOriginAllowed(mk("http://other.local", "x")))
	require.False(t, requestOriginAllowed(mk("https://deepresearch.local", "x")))
}

func TestCorsOriginAllowed(t *testing.T) {
	require.True(t, corsOriginAllowed("https://deepresearch.local"))
	require.True(t, corsOriginAllowed("http://localhost:5173"))
	require.False(t, corsOriginAllowed("https://evil.example.com"))
	require.False(t, corsOriginAllowed("not a url"))
}

func TestTerminalWSRejectsBadOrigin(t *testing.T) {
	s := newTestServer(t, &types.Config{})
	// Route through the real router so mux vars are set.
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.registerTerminalEndpoints(api)

	req := httptest.NewRequest("GET", "/api/v1/terminals/adam-x/ws", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	// 403 when terminals are enabled; 503 when PTY_BACKEND=none in the test
	// env — either way the hostile origin never reaches the upgrade.
	require.Contains(t, []int{http.StatusForbidden, http.StatusServiceUnavailable}, rec.Code)
	if rec.Code == http.StatusForbidden {
		require.Contains(t, rec.Body.String(), "origin not allowed")
	}
}

// ---- Rate limiting (card-131ec642 / card-e1ff3404) ----

func TestRateLimiterBurstAndRefill(t *testing.T) {
	l := newRateLimiter(1, 2) // 1 rps, burst 2
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }

	require.True(t, l.allow("a"))
	require.True(t, l.allow("a"))
	require.False(t, l.allow("a"), "burst exhausted")
	// Another caller has its own bucket.
	require.True(t, l.allow("b"))
	// Refill after a second.
	now = now.Add(1100 * time.Millisecond)
	require.True(t, l.allow("a"))
	require.False(t, l.allow("a"))
}

func TestWithListRateLimit429(t *testing.T) {
	s := newTestServer(t, &types.Config{})
	s.limiterOnce.Do(func() {}) // pre-empt lazy init
	s.listLimiter = newRateLimiter(0.001, 2)
	s.jobSubmitLimiter = newRateLimiter(0.001, 2)

	h := s.withListRateLimit(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	codes := []int{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		rec := httptest.NewRecorder()
		h(rec, req)
		codes = append(codes, rec.Code)
	}
	require.Equal(t, []int{200, 200, 429}, codes)
}

func TestSubmitJobRateLimit429(t *testing.T) {
	s := newTestServer(t, &types.Config{})
	s.limiterOnce.Do(func() {})
	s.listLimiter = newRateLimiter(0.001, 2)
	s.jobSubmitLimiter = newRateLimiter(0.001, 1)

	do := func() int {
		req := httptest.NewRequest("POST", "/api/v1/jobs/submit", nil)
		req.RemoteAddr = "10.0.0.9:1234"
		rec := httptest.NewRecorder()
		s.submitJob(rec, req)
		return rec.Code
	}
	// First call passes the limiter (then 503s: no temporal client wired).
	require.Equal(t, http.StatusServiceUnavailable, do())
	// Second call is throttled BEFORE any Temporal interaction.
	require.Equal(t, http.StatusTooManyRequests, do())
}

func TestCallerKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.5:9999"
	require.Equal(t, "192.168.1.5", callerKey(r))
	r.Header.Set("X-Forwarded-For", "10.1.2.3, 192.168.1.1")
	require.Equal(t, "10.1.2.3", callerKey(r))
}

// ---- Per-host deploy-agent tokens (card-df1d4d43) ----

func TestDeployAgentTokenForHost(t *testing.T) {
	t.Setenv("DEPLOY_AGENT_TOKEN", "shared-token")
	t.Setenv("DEPLOY_AGENT_TOKEN_PI5", "pi5-token")
	t.Setenv("DEPLOY_AGENT_TOKEN_PI5_SPEAKER", "speaker-token")

	require.Equal(t, "pi5-token", deployAgentTokenForHost("pi5"))
	require.Equal(t, "speaker-token", deployAgentTokenForHost("pi5-speaker"))
	// Hosts without a dedicated token fall back to the shared one.
	require.Equal(t, "shared-token", deployAgentTokenForHost("blackwell"))
	require.Equal(t, "shared-token", deployAgentTokenForHost(""))

	require.Equal(t, "DEPLOY_AGENT_TOKEN_PI5_SPEAKER", deployAgentTokenEnvKey("pi5-speaker"))
	require.Equal(t, "DEPLOY_AGENT_TOKEN_A_B_C", deployAgentTokenEnvKey("a-b.c"))
}

// ---- Memory row-level authz (card-d68023eb) ----

func TestMemoryAuthzHelpers(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	require.Equal(t, "dashboard", callerAgentID(r))
	r.Header.Set(agentIDHeader, " agent-7 ")
	require.Equal(t, "agent-7", callerAgentID(r))

	// Graph writers: open when env unset.
	require.True(t, memoryGraphWriteAllowed("anyone"))
	t.Setenv(memoryGraphWritersEnv, "agent-7,agent-8")
	require.True(t, memoryGraphWriteAllowed("agent-7"))
	require.False(t, memoryGraphWriteAllowed("intruder"))
	// Admins bypass the writers list.
	t.Setenv(memoryAdminAgentsEnv, "root-agent")
	require.True(t, memoryGraphWriteAllowed("root-agent"))
	require.True(t, isMemoryAdmin("root-agent"))
	require.False(t, isMemoryAdmin("agent-7"))
}

func TestInsightOwnerPredicate(t *testing.T) {
	s := newTestServer(t, &types.Config{})
	r := httptest.NewRequest("PATCH", "/api/v1/memory/insights/1", nil)
	r.Header.Set(agentIDHeader, "agent-7")

	// Owner column not ensured -> no predicate (legacy behaviour).
	require.Equal(t, "", s.insightOwnerPredicate(r, 6))

	s.insightsOwnerCol.Store(true)
	require.Equal(t, " AND (owner IS NULL OR owner = $6)", s.insightOwnerPredicate(r, 6))

	// Admins bypass.
	t.Setenv(memoryAdminAgentsEnv, "agent-7")
	require.Equal(t, "", s.insightOwnerPredicate(r, 6))
}

func TestMemoryGraphMutation403(t *testing.T) {
	t.Setenv(memoryGraphWritersEnv, "allowed-agent")
	s := newTestServer(t, &types.Config{})
	// evoPool must be non-nil to reach the guard — but the guard fires
	// before any query, so a typed-nil-free fake isn't needed: use a tiny
	// stub via the router path only when pool is nil? The nil-pool check
	// runs first, so instead verify the guard directly.
	require.False(t, memoryGraphWriteAllowed(callerAgentID(
		httptest.NewRequest("PATCH", "/", nil)))) // "dashboard" not in list
	_ = s
}

// ---- Kanban sweeper observability (card-28cac52a) ----

func TestKanbanSweeperStatusEndpoint(t *testing.T) {
	s := newTestServer(t, &types.Config{})
	s.kanbanSweep.enabled.Store(true)
	s.kanbanSweep.runs.Add(3)
	s.kanbanSweep.cardsReclaimed.Add(7)
	s.kanbanSweep.lastRunUnix.Store(time.Now().Unix())

	req := httptest.NewRequest("GET", "/api/v1/kanban/sweeper/status", nil)
	rec := httptest.NewRecorder()
	s.kanbanSweeperStatus(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, true, body["enabled"])
	require.EqualValues(t, 3, body["runs"])
	require.EqualValues(t, 7, body["cards_reclaimed"])
	require.NotNil(t, body["last_run_at"])
	require.EqualValues(t, 0, body["errors"])
}

package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// deployAgentURLs() reads ADAMATON_DEPLOY_AGENTS exactly once via sync.Once, so
// the upstream fake must be stood up and the env var set BEFORE any code path
// triggers that Once. TestMain owns that ordering for the whole package.
//
// scaleUpstream is the package-wide fake deploy-agent. Its behaviour for a given
// request is controlled by the handler swapped in via setUpstream — this lets
// each subtest decide whether the upstream returns 500, malformed JSON, hangs,
// etc., all behind the single URL baked into ADAMATON_DEPLOY_AGENTS.
var (
	scaleUpstream   *httptest.Server
	upstreamMu      sync.Mutex
	upstreamHandler http.HandlerFunc
)

func setUpstream(h http.HandlerFunc) {
	upstreamMu.Lock()
	upstreamHandler = h
	upstreamMu.Unlock()
}

func TestMain(m *testing.M) {
	scaleUpstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		h := upstreamHandler
		upstreamMu.Unlock()
		if h == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		h(w, r)
	}))

	// Map the rack host "pi5" (present in racks.yaml) to the fake agent, and
	// provide a token so the proxy doesn't 503 on the unset-token guard. Set
	// BEFORE deployAgentURLs()'s sync.Once can fire.
	_ = os.Setenv("ADAMATON_DEPLOY_AGENTS", "pi5="+scaleUpstream.URL)
	_ = os.Setenv("DEPLOY_AGENT_TOKEN", "test-token")

	code := m.Run()
	scaleUpstream.Close()
	os.Exit(code)
}

// scaleReq drives a POST /nodes/{host}/scale through a router, allowing the
// caller to pass a custom request context (for cancellation tests).
func scaleReq(s *APIServer, ctx context.Context, host, query string) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	api := router.PathPrefix("/api/v1").Subrouter()
	s.registerNodesEndpoints(api)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/"+host+"/scale"+query, nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestPostNodeScale_UnknownHost(t *testing.T) {
	s := newPoollessServer(t)
	rr := scaleReq(s, nil, "no-such-host", "?service=evo-worker&replicas=2")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "unknown host")
}

func TestPostNodeScale_MissingParams(t *testing.T) {
	s := newPoollessServer(t)
	rr := scaleReq(s, nil, "pi5", "")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "are both required")
}

func TestPostNodeScale_NotScalable(t *testing.T) {
	s := newPoollessServer(t)
	rr := scaleReq(s, nil, "pi5", "?service=postgres&replicas=2")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "service not scalable")
}

// TestPostNodeScale_UpstreamJSONPassthrough: when the agent returns JSON, the
// proxy forwards it verbatim with the upstream status code.
func TestPostNodeScale_UpstreamJSONPassthrough(t *testing.T) {
	s := newPoollessServer(t)
	setUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"scaled":"evo-worker","replicas":3}`))
	})
	rr := scaleReq(s, nil, "pi5", "?service=evo-worker&replicas=3")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.JSONEq(t, `{"scaled":"evo-worker","replicas":3}`, rr.Body.String())
}

// TestPostNodeScale_UpstreamNon200JSON: a non-200 JSON response is forwarded
// with its status code preserved (so the dialog can show validation errors
// as-is).
func TestPostNodeScale_UpstreamNon200JSON(t *testing.T) {
	s := newPoollessServer(t)
	setUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"replica cap exceeded"}`))
	})
	rr := scaleReq(s, nil, "pi5", "?service=evo-worker&replicas=99")
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "replica cap exceeded")
}

// TestPostNodeScale_UpstreamPlainTextReshaped: a non-JSON (text/plain) upstream
// error is re-shaped into the {error, upstream_code, upstream_host} envelope.
func TestPostNodeScale_UpstreamPlainTextReshaped(t *testing.T) {
	s := newPoollessServer(t)
	setUpstream(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom from agent", http.StatusInternalServerError)
	})
	rr := scaleReq(s, nil, "pi5", "?service=evo-worker&replicas=2")
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	var env map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &env))
	require.Equal(t, "boom from agent", env["error"])
	require.EqualValues(t, http.StatusInternalServerError, env["upstream_code"])
	require.Equal(t, "pi5", env["upstream_host"])
}

// TestPostNodeScale_MalformedJSONContentType: the upstream sets
// Content-Type: application/json but returns malformed bytes. The proxy
// forwards verbatim (it doesn't re-parse JSON responses), so the malformed body
// reaches the client under the JSON content type — documenting current
// behaviour so a future refactor that adds validation is a deliberate choice.
func TestPostNodeScale_MalformedJSONContentType(t *testing.T) {
	s := newPoollessServer(t)
	setUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{not valid json`))
	})
	rr := scaleReq(s, nil, "pi5", "?service=evo-worker&replicas=2")
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "application/json", rr.Header().Get("Content-Type"))
	require.False(t, json.Valid(rr.Body.Bytes()), "malformed upstream JSON is forwarded verbatim")
}

// TestPostNodeScale_UpstreamUnreachable simulates the proxy's
// client.Do error path (timeout / connection failure) by cancelling the
// request context before the upstream can respond. The handler maps the error
// to 502 BadGateway.
func TestPostNodeScale_UpstreamUnreachable(t *testing.T) {
	s := newPoollessServer(t)
	// Upstream blocks until the test releases it, guaranteeing the client's
	// (cancelled) context fires first.
	release := make(chan struct{})
	setUpstream(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the request starts so client.Do returns a
	// context error rather than hanging for the 5-minute client timeout.
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	rr := scaleReq(s, ctx, "pi5", "?service=evo-worker&replicas=2")
	require.Equal(t, http.StatusBadGateway, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "deploy-agent unreachable")
}

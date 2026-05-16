// /thearray/gogents/internal/gitea/client_test.go
//
// Unit tests for the low-level Gitea HTTP client. Each test uses
// httptest.NewServer to simulate Gitea responses and confirm the client
// behaves correctly without round-tripping a real Gitea instance.
package gitea

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamomaton-core/types"
)

// newTestClient returns a GiteaClient pointed at the given test server URL
// with discardable logging, so tests don't print to stderr.
func newTestClient(t *testing.T, baseURL string) *GiteaClient {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return NewGiteaClient(types.GiteaConfig{
		BaseURL: baseURL,
		Token:   "test-token",
		Timeout: 5 * time.Second,
	}, logger)
}

// newClientWithHook returns a GiteaClient with a logrus hook attached so the
// caller can inspect emitted log entries (used to assert the rate-limit
// warning is logged exactly when expected).
func newClientWithHook(t *testing.T, baseURL string) (*GiteaClient, *test.Hook) {
	t.Helper()
	logger, hook := test.NewNullLogger()
	logger.SetLevel(logrus.WarnLevel)
	c := NewGiteaClient(types.GiteaConfig{
		BaseURL: baseURL,
		Token:   "test-token",
		Timeout: 5 * time.Second,
	}, logger)
	return c, hook
}

// --- truncateForError / error body truncation ----------------------------

func TestMakeRequest_ErrorBodyTruncation(t *testing.T) {
	// Server returns a 1KB error body. The client should embed only the
	// first errorBodyExcerptLen bytes followed by the truncation marker.
	long := strings.Repeat("X", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(long))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.makeRequest(context.Background(), "GET", "/anything", nil, nil)
	require.Error(t, err)

	expectedExcerpt := strings.Repeat("X", errorBodyExcerptLen) + "...[truncated]"
	require.Contains(t, err.Error(), expectedExcerpt, "error body should be truncated to %d bytes plus marker", errorBodyExcerptLen)
	require.Contains(t, err.Error(), "status 500")
}

func TestMakeRequest_ShortErrorBodyNotTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.makeRequest(context.Background(), "GET", "/anything", nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nope")
	require.NotContains(t, err.Error(), "truncated")
}

// --- truncateForError direct unit -----------------------------------------

func TestTruncateForError(t *testing.T) {
	require.Equal(t, "", truncateForError([]byte("")))
	require.Equal(t, "abc", truncateForError([]byte("abc")))
	require.Equal(t, strings.Repeat("a", errorBodyExcerptLen)+"...[truncated]",
		truncateForError([]byte(strings.Repeat("a", errorBodyExcerptLen+10))))
}

// --- checkRateLimit --------------------------------------------------------

func TestCheckRateLimit_LogsWhenExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Limit", "1000")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		// 200 + empty JSON body so makeRequest doesn't error first.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, hook := newClientWithHook(t, srv.URL)
	var dest map[string]interface{}
	require.NoError(t, c.makeRequest(context.Background(), "GET", "/x", nil, &dest))

	// Find the rate-limit warning entry.
	var found bool
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.WarnLevel && strings.Contains(e.Message, "rate limit exhausted") {
			found = true
			break
		}
	}
	require.True(t, found, "expected rate-limit warning in log; got %v", hook.AllEntries())
}

func TestCheckRateLimit_SilentWhenHeaderMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, hook := newClientWithHook(t, srv.URL)
	var dest map[string]interface{}
	require.NoError(t, c.makeRequest(context.Background(), "GET", "/x", nil, &dest))

	for _, e := range hook.AllEntries() {
		require.NotContains(t, e.Message, "rate limit", "no rate-limit warning should fire when header is absent")
	}
}

func TestCheckRateLimit_SilentWhenHeaderMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "not-a-number")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, hook := newClientWithHook(t, srv.URL)
	var dest map[string]interface{}
	require.NoError(t, c.makeRequest(context.Background(), "GET", "/x", nil, &dest))

	for _, e := range hook.AllEntries() {
		require.NotContains(t, e.Message, "rate limit", "malformed header should be silently ignored")
	}
}

func TestCheckRateLimit_SilentWhenNonZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c, hook := newClientWithHook(t, srv.URL)
	var dest map[string]interface{}
	require.NoError(t, c.makeRequest(context.Background(), "GET", "/x", nil, &dest))

	for _, e := range hook.AllEntries() {
		require.NotContains(t, e.Message, "rate limit", "non-zero remaining should not warn")
	}
}

// --- Insecure transport ----------------------------------------------------

func TestInsecureTransport_HasMinTLS12(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	c := NewGiteaClient(types.GiteaConfig{
		BaseURL:  "https://example.invalid",
		Token:    "x",
		Insecure: true,
	}, logger)

	transport, ok := c.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "Insecure mode must install an *http.Transport")
	require.NotNil(t, transport.TLSClientConfig)
	require.True(t, transport.TLSClientConfig.InsecureSkipVerify, "InsecureSkipVerify should be true")
	require.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion,
		"even with InsecureSkipVerify, MinVersion must be TLS 1.2")
	// Insecure mode must still get the tuned pool — the same fan-out concerns
	// apply whether or not certificates are verified.
	require.Equal(t, 32, transport.MaxIdleConnsPerHost,
		"insecure mode must inherit the tuned pool, not just the TLS override")
	require.NotNil(t, transport.TLSClientConfig.ClientSessionCache,
		"TLS session cache must survive an insecure-mode build")
}

func TestSecureTransport_HasTunedPoolAndTLS(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	c := NewGiteaClient(types.GiteaConfig{
		BaseURL:  "https://example.invalid",
		Token:    "x",
		Insecure: false,
	}, logger)

	// The default transport's MaxIdleConnsPerHost=2 caused 10 of every 12
	// concurrent agent calls to redo a full handshake; the tuned transport
	// fixes that. Pin the values so a future regression that drops back to
	// http.DefaultTransport can't sneak in unnoticed.
	transport, ok := c.httpClient.Transport.(*http.Transport)
	require.True(t, ok, "secure mode must install an *http.Transport (was nil → http.DefaultTransport in audit-pass-12)")
	require.Equal(t, 32, transport.MaxIdleConnsPerHost,
		"MaxIdleConnsPerHost must be raised above the default of 2 for fan-out workloads")
	require.Equal(t, 100, transport.MaxIdleConns)
	require.True(t, transport.ForceAttemptHTTP2, "HTTP/2 should be enabled so one conn can multiplex")
	require.NotNil(t, transport.TLSClientConfig)
	require.False(t, transport.TLSClientConfig.InsecureSkipVerify, "secure mode must verify certificates")
	require.Equal(t, uint16(tls.VersionTLS12), transport.TLSClientConfig.MinVersion)
	require.NotNil(t, transport.TLSClientConfig.ClientSessionCache,
		"TLS session cache must be configured so resumption survives idle conn pruning")
}

// --- sanitizePathSegment --------------------------------------------------

func TestSanitizePathSegment(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{in: "owner", want: "owner"},
		{in: "..", want: ".."},                          // url.PathEscape leaves ".." alone
		{in: "owner/repo", want: "owner%2Frepo"},        // slash gets escaped
		{in: "with space", want: "with%20space"},        // space gets escaped
		{in: "café", want: "caf%C3%A9"},                 // unicode gets escaped
		{in: "../../etc/passwd", want: "..%2F..%2Fetc%2Fpasswd"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := sanitizePathSegment(tc.in)
			require.Equal(t, tc.want, got)
		})
	}
}

// --- buildRequest sets headers --------------------------------------------

func TestBuildRequest_SetsAuthAndAcceptHeaders(t *testing.T) {
	c := newTestClient(t, "https://example.invalid")
	req, err := c.buildRequest(context.Background(), "GET", "/api/v1/foo", nil)
	require.NoError(t, err)

	require.Equal(t, "token test-token", req.Header.Get("Authorization"))
	require.Equal(t, "application/json", req.Header.Get("Accept"))
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Contains(t, req.Header.Get("User-Agent"), "GoGents")
}

func TestBuildRequest_PayloadEncoded(t *testing.T) {
	c := newTestClient(t, "https://example.invalid")
	payload := map[string]string{"foo": "bar"}
	req, err := c.buildRequest(context.Background(), "POST", "/api/v1/foo", payload)
	require.NoError(t, err)

	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `"foo":"bar"`)
}

// --- Timeout sanity --------------------------------------------------------

func TestNewGiteaClient_AppliesDefaultTimeout(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	c := NewGiteaClient(types.GiteaConfig{
		BaseURL: "https://example.invalid",
		Token:   "x",
		// No Timeout set -> defaultGiteaTimeout
	}, logger)
	require.Equal(t, defaultGiteaTimeout, c.httpClient.Timeout)
}

func TestNewGiteaClient_HonorsExplicitTimeout(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	c := NewGiteaClient(types.GiteaConfig{
		BaseURL: "https://example.invalid",
		Token:   "x",
		Timeout: 17 * time.Second,
	}, logger)
	require.Equal(t, 17*time.Second, c.httpClient.Timeout)
}

func TestNewGiteaClient_TrimsTrailingSlash(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	c := NewGiteaClient(types.GiteaConfig{
		BaseURL: "https://example.invalid/",
		Token:   "x",
	}, logger)
	require.Equal(t, "https://example.invalid", c.baseURL)
}

// --- makeRequest decodes JSON into result ---------------------------------

func TestMakeRequest_DecodesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"name":"alice","id":42}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	var got struct {
		Name string `json:"name"`
		ID   int    `json:"id"`
	}
	require.NoError(t, c.makeRequest(context.Background(), "GET", "/x", nil, &got))
	require.Equal(t, "alice", got.Name)
	require.Equal(t, 42, got.ID)
}

// --- HTTP error vs. transport error ---------------------------------------

func TestMakeRequest_TransportError(t *testing.T) {
	// An unreachable URL — the dial should fail and we want the wrapped
	// error message to surface so the operator can see "HTTP request failed".
	c := newTestClient(t, "http://127.0.0.1:1") // port 1 is reserved
	c.httpClient.Timeout = 200 * time.Millisecond
	err := c.makeRequest(context.Background(), "GET", "/x", nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP request failed")
}

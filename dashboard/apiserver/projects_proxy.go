package apiserver

// Host-aware project plumbing. A project folder lives on a specific host (the
// local machine, a Pi, blackwell, …); evo.projects.host names it ("" = the
// local host, for back-compat with pre-migration-0018 rows). When a project is
// remote, the apiserver doesn't touch the filesystem itself — it forwards the
// request to that host's deploy-agent /project/* API (bearer-authenticated),
// which runs the same core/projectfs primitives on the agent host.
//
// This file holds the shared helpers: host classification, the deploy-agent
// base-URL + token lookup, and the reverse-proxy used by the tree/file/terminal
// endpoints. The local path is always served directly via core/projectfs.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// deployAgentToken returns the bearer token used to call deploy-agent /project
// APIs. It prefers the configured API token's sibling env (DEPLOY_AGENT_TOKEN),
// matching nodes_endpoints.go, so the dashboard authenticates to agents the
// same way for projects and scaling.
func deployAgentToken() string {
	return os.Getenv("DEPLOY_AGENT_TOKEN")
}

// isLocalHost reports whether a project host string refers to the machine this
// apiserver runs on. The empty string is local by definition (pre-0018 rows),
// as is an explicit match for inferLocalHost().
func isLocalHost(host string) bool {
	host = strings.TrimSpace(host)
	return host == "" || host == inferLocalHost()
}

// registerableHosts is the deduped set of hosts a project can be registered on:
// the local host plus every host with a configured deploy-agent URL. Sorted
// with the local host first so the UI can default to it.
func registerableHosts() []string {
	local := inferLocalHost()
	seen := map[string]struct{}{local: {}}
	out := []string{local}
	for host := range deployAgentURLs() {
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// agentBaseURL resolves the deploy-agent base URL for a host, returning ""
// (and false) when no URL is configured for it.
func agentBaseURL(host string) (string, bool) {
	base, ok := deployAgentURLs()[host]
	return base, ok && base != ""
}

// localHostSQLPredicate renders a SQL boolean that is true when the host column
// (named by col) refers to the local machine: the empty string (pre-0018 rows /
// freshly-local) or an explicit match for inferLocalHost(). The match value is
// bound as $1, supplied via localHostSQLArg(); keep them paired.
func localHostSQLPredicate(col string) string {
	return "(" + col + " = '' OR " + col + " = $1)"
}

// localHostSQLArg returns the bind value for localHostSQLPredicate ($1).
func localHostSQLArg() string { return inferLocalHost() }

// agentProjectError is the error surfaced when a remote host can't be reached
// or isn't configured. Handlers translate it to a 502/503 for the browser.
var errAgentUnreachable = errors.New("deploy-agent unreachable")

// proxyAgentGET performs a bearer-authenticated GET to the host's deploy-agent
// at path+query and copies the upstream status code and body verbatim onto w.
// It is used by the tree and file endpoints, which mirror projectfs results
// from the agent host 1:1 (including the agent's 400/404/413 error shapes).
func (s *APIServer) proxyAgentGET(w http.ResponseWriter, r *http.Request, host, path, rawQuery string) {
	base, ok := agentBaseURL(host)
	if !ok {
		writeEvoErr(w, http.StatusServiceUnavailable,
			"no deploy-agent URL for host "+host+" (set ADAMATON_DEPLOY_AGENTS env)")
		return
	}
	token := deployAgentToken()
	if token == "" {
		writeEvoErr(w, http.StatusServiceUnavailable, "DEPLOY_AGENT_TOKEN not set on dashboard")
		return
	}

	upstream := base + path
	if rawQuery != "" {
		upstream += "?" + rawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "build request: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "deploy-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "deploy-agent body: "+err.Error())
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// agentValidateResult is the deploy-agent /project/validate response shape:
// {abs, type, git_remote}. git_remote is "" when there is no origin.
type agentValidateResult struct {
	Abs       string `json:"abs"`
	Type      string `json:"type"`
	GitRemote string `json:"git_remote"`
}

// validateOnAgent calls GET /project/validate?path=<abs> on the host's
// deploy-agent and returns the canonicalised absolute path, git type, and
// origin remote. A non-200 from the agent (e.g. the path is missing on that
// host) is surfaced as an error carrying the agent's message.
func validateOnAgent(ctx context.Context, host, path string) (*agentValidateResult, error) {
	base, ok := agentBaseURL(host)
	if !ok {
		return nil, errors.New("no deploy-agent URL for host " + host + " (set ADAMATON_DEPLOY_AGENTS env)")
	}
	token := deployAgentToken()
	if token == "" {
		return nil, errors.New("DEPLOY_AGENT_TOKEN not set on dashboard")
	}

	upstream := base + "/project/validate?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errAgentUnreachable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, errors.New(msg)
	}
	var out agentValidateResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("decode validate response: " + err.Error())
	}
	return &out, nil
}

// dialAgentTerminalWS opens a bearer-authenticated websocket to the host's
// deploy-agent at /project/terminals/{id}/ws. The caller bridges the returned
// connection to the browser-facing conn.
func dialAgentTerminalWS(ctx context.Context, host, id string) (*websocket.Conn, error) {
	base, ok := agentBaseURL(host)
	if !ok {
		return nil, errors.New("no deploy-agent URL for host " + host)
	}
	token := deployAgentToken()
	// http(s):// -> ws(s)://
	wsURL := base + "/project/terminals/" + id + "/ws"
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	conn, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// bridgeWSConns pipes frames between two websocket connections until either
// side closes. Used to reverse-proxy a browser terminal websocket to the
// remote host's deploy-agent. Both connections are closed on return.
func bridgeWSConns(browser, agent *websocket.Conn) {
	defer browser.Close()
	defer agent.Close()

	done := make(chan struct{}, 2)
	copyFrames := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, msg, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}
	go copyFrames(agent, browser)
	go copyFrames(browser, agent)
	<-done
}

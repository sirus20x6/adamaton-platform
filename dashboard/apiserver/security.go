package apiserver

// Shared security plumbing for the apiserver: browser Origin validation
// (websocket upgrades, the research proxy, and the CORS layer), a small
// in-process token-bucket rate limiter used by the high-cardinality list
// endpoints + /jobs/submit, and the per-host deploy-agent token lookup.
//
// Origin allowlist contract: requests without an Origin header (curl, Go
// clients, same-process callers) are always allowed — Origin validation is
// a browser-facing CSRF/abuse guard, not an auth layer (auth is the API
// token). When an Origin IS present it must be either same-origin with the
// request Host or on the allowlist (deepresearch.local, adamaton.local,
// localhost/dev loopback by default; override via EVO_ALLOWED_ORIGINS).

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// Origin validation
// ─────────────────────────────────────────────────────────────────────

// allowedOriginHostsEnv overrides the default Origin-host allowlist. Comma-
// separated; entries may be bare hostnames ("adamaton.local") or full
// origins ("https://adamaton.local:8443") — only the hostname is matched,
// any port is accepted.
const allowedOriginHostsEnv = "EVO_ALLOWED_ORIGINS"

// defaultAllowedOriginHosts is the built-in allowlist: the two LAN names the
// dashboard is served under plus loopback for dev servers (vite on
// localhost:5173 etc.). Matched case-insensitively against the Origin
// hostname, ignoring the port.
var defaultAllowedOriginHosts = []string{
	"deepresearch.local",
	"adamaton.local",
	"localhost",
	"127.0.0.1",
	"::1",
}

// allowedOriginHosts resolves the active allowlist. Re-read per call (cheap)
// so tests and runtime tightening are honoured without a restart.
func allowedOriginHosts() []string {
	raw := strings.TrimSpace(os.Getenv(allowedOriginHostsEnv))
	if raw == "" {
		return defaultAllowedOriginHosts
	}
	var out []string
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		if strings.Contains(e, "://") {
			if u, err := url.Parse(e); err == nil && u.Hostname() != "" {
				out = append(out, u.Hostname())
				continue
			}
		}
		// Bare host (possibly host:port) — keep just the host.
		if h, _, err := net.SplitHostPort(e); err == nil && h != "" {
			out = append(out, h)
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return defaultAllowedOriginHosts
	}
	return out
}

// hostOnly strips a port (and IPv6 brackets) from a Host-header style value.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// originHostAllowed reports whether a bare Origin hostname is on the
// allowlist (any port). Used by both request-level checks and the CORS
// AllowOriginFunc.
func originHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, a := range allowedOriginHosts() {
		if host == a {
			return true
		}
	}
	return false
}

// corsOriginAllowed adapts the allowlist to the rs/cors AllowOriginFunc
// signature (full origin string like "https://adamaton.local").
func corsOriginAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return originHostAllowed(u.Hostname())
}

// requestOriginAllowed is the per-request Origin guard used by the terminal
// websocket upgrade and the research proxy. Non-browser requests (no Origin
// header) pass; browser requests must be same-origin with the request Host
// or come from an allowlisted host.
func requestOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	// "Origin: null" (sandboxed iframe, data: URL, some redirects) parses to
	// an empty hostname below and is rejected.
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	oh := strings.ToLower(u.Hostname())
	// Same-origin: the browser is on the page this server (or its fronting
	// proxy) served. Compare hostnames only — the port differs across the
	// Caddy/vite/direct paths and TLS termination.
	if rh := strings.ToLower(hostOnly(r.Host)); rh != "" && oh == rh {
		return true
	}
	return originHostAllowed(oh)
}

// ─────────────────────────────────────────────────────────────────────
// Token-bucket rate limiter (per caller)
// ─────────────────────────────────────────────────────────────────────

// rateLimiter is a small in-process per-key token bucket. Buckets refill at
// `rate` tokens/second up to `burst`. Stale buckets are swept opportunistically
// so a scan across many source IPs can't grow the map without bound.
type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	rate      float64
	burst     float64
	lastSweep time.Time
	now       func() time.Time // injectable clock for tests
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    ratePerSec,
		burst:   burst,
		now:     time.Now,
	}
}

// allow takes one token from key's bucket, reporting false when the bucket
// is empty (caller should 429).
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	// Opportunistic sweep: drop buckets idle long enough to be full again.
	if now.Sub(l.lastSweep) > 10*time.Minute {
		idle := time.Duration(l.burst/l.rate)*time.Second + 10*time.Minute
		for k, b := range l.buckets {
			if now.Sub(b.last) > idle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// callerKey identifies the caller for rate-limiting purposes: the first
// X-Forwarded-For hop when present (the dashboard sits behind Caddy),
// otherwise the socket peer address.
func callerKey(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// envFloat reads a float env var, falling back to def when unset/invalid.
func envFloat(key string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// Limiter tuning env vars. Rates are requests/second per caller; bursts are
// 2x list rate and a small fixed pool for job submission.
const (
	listRateEnv      = "EVO_LIST_RATE_LIMIT_RPS"   // default 20 rps / caller
	jobSubmitRateEnv = "EVO_JOB_SUBMIT_RATE_LIMIT" // default 0.5 rps (30/min) / caller
)

// limiters lazily builds the per-server limiters so tests that construct
// APIServer directly (without NewAPIServer) still get working limits.
func (s *APIServer) limiters() (*rateLimiter, *rateLimiter) {
	s.limiterOnce.Do(func() {
		listRate := envFloat(listRateEnv, 20)
		s.listLimiter = newRateLimiter(listRate, listRate*2)
		s.jobSubmitLimiter = newRateLimiter(envFloat(jobSubmitRateEnv, 0.5), 5)
	})
	return s.listLimiter, s.jobSubmitLimiter
}

// withListRateLimit wraps a list handler with the shared list limiter.
func (s *APIServer) withListRateLimit(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, _ := s.limiters()
		if !list.allow(callerKey(r)) {
			w.Header().Set("Retry-After", "1")
			writeEvoErr(w, http.StatusTooManyRequests, "rate limit exceeded; slow down")
			return
		}
		h(w, r)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Per-host deploy-agent tokens
// ─────────────────────────────────────────────────────────────────────

// deployAgentTokenEnvKey renders the per-host env var name for a host:
// "pi5-speaker" -> DEPLOY_AGENT_TOKEN_PI5_SPEAKER.
func deployAgentTokenEnvKey(host string) string {
	up := strings.ToUpper(host)
	up = strings.NewReplacer("-", "_", ".", "_").Replace(up)
	return "DEPLOY_AGENT_TOKEN_" + up
}

// deployAgentTokenForHost resolves the bearer token for a specific host's
// deploy-agent. Precedence: DEPLOY_AGENT_TOKEN_<HOST> (host and, when the
// host resolves through racks.yaml, its canonical name + aliases) then the
// shared DEPLOY_AGENT_TOKEN. Per-host tokens keep a compromised token for
// one host from pivoting to the rest of the fleet; the shared fallback keeps
// existing single-token deploys working unchanged.
func deployAgentTokenForHost(host string) string {
	host = strings.TrimSpace(host)
	if host != "" {
		if v := os.Getenv(deployAgentTokenEnvKey(host)); v != "" {
			return v
		}
		if rack, ok := resolveRack(host); ok {
			for _, alias := range append([]string{rack.Host}, rack.Aliases...) {
				if alias == "" || alias == host {
					continue
				}
				if v := os.Getenv(deployAgentTokenEnvKey(alias)); v != "" {
					return v
				}
			}
		}
	}
	return os.Getenv("DEPLOY_AGENT_TOKEN")
}

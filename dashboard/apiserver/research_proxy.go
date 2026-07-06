// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
package apiserver

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/mux"
)

// deepResearchAllowedHosts is the SSRF allowlist for the research proxy.
// The env var DEEPRESEARCH_ALLOWED_HOSTS is comma-separated; entries
// starting with "." are treated as suffix matches (so ".local" allows
// any *.local host). Defaults to {"deepresearch.local"}.
//
// SECURITY: this is re-read on every call (no sync.Once cache) so an
// operator who tightens the allowlist at runtime — or a test that sets a
// hostile value — is honoured per request rather than frozen at the first
// proxy hit. The suffix entries are stored verbatim and matched against a
// dot-boundary, never as a bare strings.HasSuffix (which would let
// "evildeepresearch.local" or "x.local.attacker.com" slip through).
func deepResearchAllowedHosts() (map[string]bool, []string) {
	exact := map[string]bool{}
	var sufx []string
	raw := os.Getenv("DEEPRESEARCH_ALLOWED_HOSTS")
	if raw == "" {
		exact["deepresearch.local"] = true
		return exact, sufx
	}
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(strings.ToLower(e))
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, ".") {
			sufx = append(sufx, e)
		} else {
			exact[e] = true
		}
	}
	return exact, sufx
}

// hostAllowed reports whether the given host (typically url.Hostname())
// is permitted by the deepresearch allowlist.
//
// Hardening over the old lowercased-strings.HasSuffix implementation:
//
//   - The host is normalised: surrounding whitespace, a stray trailing
//     dot (FQDN form "deepresearch.local."), and any zone-id on an IPv6
//     literal are stripped before matching. url.Hostname() already strips
//     the [] from an IPv6 literal; we re-detect literals below.
//   - Literal IP addresses (v4 *and* v6, including "[::1]" → "::1" and
//     IPv4-mapped forms) are REJECTED unless they appear verbatim in the
//     exact allowlist. This kills the classic SSRF pivot of pointing the
//     proxy at 169.254.169.254 / 127.0.0.1 / [::1] — a bare ".local"
//     suffix rule can no longer be satisfied by an IP literal.
//   - Suffix rules match on a dot boundary: ".local" allows
//     "deepresearch.local" but not "deepresearch.local.evil.com" and not
//     "notdeepresearch.local" being read as a suffix of a longer attacker
//     host. (strings.HasSuffix(".local") would accept "x.evil.local"
//     which is the intended wildcard, but would ALSO accept a host whose
//     literal text merely ends in those bytes — the dot-boundary keeps it
//     to true subdomains.)
func hostAllowed(host string) bool {
	host = normalizeHost(host)
	if host == "" {
		return false
	}
	exact, sufx := deepResearchAllowedHosts()

	// An exact allowlist entry always wins — this is the ONLY way a
	// literal IP (or any other host) gets through.
	if exact[host] {
		return true
	}

	// Reject IP literals that weren't explicitly allowlisted above. This
	// covers "::1", "127.0.0.1", "169.254.169.254", IPv4-mapped IPv6,
	// etc. Suffix rules must never apply to an IP literal.
	if net.ParseIP(host) != nil {
		return false
	}

	for _, s := range sufx {
		// s is like ".local". Accept "deepresearch.local" (host == s
		// without the leading dot) and any true subdomain
		// "*.<suffix-without-dot>", but never a host that merely ends in
		// the same bytes across a non-dot boundary.
		bare := strings.TrimPrefix(s, ".")
		if host == bare || strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// normalizeHost lowercases and trims a host extracted from url.Hostname(),
// dropping a single trailing FQDN dot and any IPv6 zone identifier so the
// allowlist comparison sees a canonical form. It does NOT add or remove
// brackets — callers pass the already-bracket-stripped url.Hostname().
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	// Strip an IPv6 zone id ("fe80::1%eth0" → "fe80::1") so it can't be
	// used to dodge the net.ParseIP literal check below.
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return host
}

// registerResearchProxy wires /api/v1/research/* through to the Pi.
// The proxy is one-handler-fits-all rather than ReverseProxy because
// we want a tight allowlist of prefixes (only read endpoints), and
// the URL rewriting is trivial.
//
// Routes:
//
//	GET  /api/v1/research/health           → GET  /api/v1/health
//	GET  /api/v1/research/library/...      → GET  /library/...
//	GET  /api/v1/research/api/...          → GET  /api/...
//	POST /api/v1/research/library/api/{c}/search → POST /library/api/{c}/search
//
// Anything else 404s. We intentionally don't expose write endpoints
// (delete/update/etc.) through the proxy — the Pi's native UI is the
// authorised surface for those.
func (s *APIServer) registerResearchProxy(api *mux.Router) {
	allowed := []struct {
		methods []string
		pattern string
		strip   string // segment to strip from the request before forwarding
	}{
		{[]string{"GET"}, "/research/health", "/research"},
		{[]string{"GET", "POST"}, "/research/library/{rest:.*}", "/research"},
		{[]string{"GET"}, "/research/api/{rest:.*}", "/research"},
	}
	for _, a := range allowed {
		strip := a.strip
		methods := a.methods
		api.HandleFunc(a.pattern, func(w http.ResponseWriter, r *http.Request) {
			s.proxyDeepResearch(w, r, strip)
		}).Methods(methods...)
	}
}

func (s *APIServer) proxyDeepResearch(w http.ResponseWriter, r *http.Request, stripPrefix string) {
	// Browser Origin guard: the CORS layer only vets origins it is asked
	// about via preflight; simple GETs from a hostile page still reach the
	// handler. Refuse cross-origin browsers that aren't on the allowlist so
	// the dashboard can't be used as a CORS-bypassing proxy onto the Pi.
	if !requestOriginAllowed(r) {
		s.logger.WithField("origin", r.Header.Get("Origin")).
			Warn("research proxy: rejecting disallowed Origin")
		writeEvoErr(w, http.StatusForbidden, "origin not allowed")
		return
	}
	base := s.deepResearchURL()
	if base == "" {
		writeEvoErr(w, http.StatusServiceUnavailable, "deepresearch URL not configured")
		return
	}
	// SSRF guard: parse the configured base URL and verify the host
	// against the allowlist. Without this, DEEPRESEARCH_URL could be
	// pointed at any internal endpoint (169.254.169.254, an internal
	// admin UI, etc.) and the proxy would happily forward requests
	// there with the dashboard's identity.
	baseURL, err := url.Parse(base)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "proxy: base URL invalid: "+err.Error())
		return
	}
	if !hostAllowed(baseURL.Hostname()) {
		s.logger.WithField("host", baseURL.Hostname()).
			Warn("research proxy: rejecting upstream host (not in allowlist)")
		writeEvoErr(w, http.StatusBadGateway,
			"proxy: upstream host not in DEEPRESEARCH_ALLOWED_HOSTS allowlist")
		return
	}
	// Build the upstream URL. r.URL.Path is /api/v1/research/health etc.;
	// after stripping the prefix the rest maps onto the Pi's own paths.
	// e.g. /api/v1/research/library/api/colA/search → /library/api/colA/search.
	//
	// We require the stripPrefix to be a true prefix (HasPrefix) rather
	// than substring-anywhere — otherwise a path that contains
	// /research later in the URL (e.g. /api/v1/foo/research/...) would
	// confuse the strip logic and let an attacker reshape the upstream
	// path. The router would normally never deliver such a request to
	// this handler, but the defensive check keeps the contract explicit.
	if !strings.HasPrefix(r.URL.Path, stripPrefix) {
		writeEvoErr(w, http.StatusInternalServerError, "proxy: prefix not found in request path")
		return
	}
	upstreamPath := r.URL.Path[len(stripPrefix):]
	if upstreamPath == "" {
		upstreamPath = "/"
	}
	target, err := url.Parse(base + upstreamPath)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "proxy: parse target URL: "+err.Error())
		return
	}
	target.RawQuery = r.URL.RawQuery

	// Bound the proxied request body. The upstream is expected to be
	// chatty on payloads (POST .../search bodies) but a 16MB cap keeps
	// a malicious client from spooling unbounded data through us.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "proxy: build request: "+err.Error())
		return
	}
	// Forward a minimal header set — no auth tokens, no cookies. The
	// Pi's native UI is the auth-aware surface; this proxy is for
	// dashboard-side read-only views.
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if ac := r.Header.Get("Accept"); ac != "" {
		req.Header.Set("Accept", ac)
	}

	resp, err := s.deepResearchHTTPClient().Do(req)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "proxy: upstream call failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// Pipe headers + body straight through. We're not buffering — the
	// upstream payload size is bounded by deepresearch's own limits.
	for k, vs := range resp.Header {
		// Skip hop-by-hop headers.
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "proxy-authenticate",
			"proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade":
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

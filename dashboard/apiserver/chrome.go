// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"os"
	"strings"
)

// chrome.go holds the shared visual chrome (nav bar + global CSS) for
// the inline-HTML pages of the unified dashboard. The React app in
// ui/dist has its own chrome; this surface is for the inline-HTML
// pages we serve directly (/, /evo, /research) so they feel like one
// product even without a frontend build step.

// dashboardCSS is the global stylesheet for inline pages — colour
// palette + typography + reusable components (pill, tag, card,
// table). Kept as a single block so each page can include it via
// <style>{{ dashboardCSS }}</style> and stay self-contained.
const dashboardCSS = `
  * { box-sizing: border-box }
  html, body { margin:0; padding:0; background:#0d1117; color:#c9d1d9;
               font-family: -apple-system, "Segoe UI", "Helvetica Neue", sans-serif;
               font-size: 14px; line-height: 1.45 }
  a { color: #79c0ff; text-decoration: none }
  a:hover { text-decoration: underline }
  code, pre { font-family: "JetBrains Mono", Menlo, monospace; font-size: 12px }

  nav.top {
    display:flex; align-items:center; gap:12px;
    padding: 10px 24px; border-bottom: 1px solid #21262d;
    background:#0d1117;
  }
  nav.top .brand { font-weight: 600; font-size: 15px; color:#c9d1d9; margin-right:12px }
  nav.top a.link {
    padding: 6px 12px; border-radius: 6px; color:#c9d1d9;
    font-size: 13px;
  }
  nav.top a.link:hover { background:#21262d; text-decoration:none }
  nav.top a.link.active { background:#1f2a36; color:#79c0ff }
  nav.top .spacer { flex: 1 }
  nav.top .meta { color:#8b949e; font-size: 12px }
  nav.top .refresh-tick {
    display:inline-block; width:8px; height:8px; background:#7ee2a8;
    border-radius:50%; opacity:0; margin-left:6px; vertical-align:middle;
    transition: opacity 0.4s;
  }
  nav.top .refresh-tick.live { opacity:1 }

  h2 { margin:0 0 12px 0; font-size: 14px; color:#8b949e;
       text-transform: uppercase; letter-spacing: 0.04em }

  section { background:#161b22; border:1px solid #21262d; border-radius:8px;
            padding:16px; min-width:0 }

  .pill { display:inline-block; padding:1px 8px; border-radius:10px;
          font-size:11px; font-weight:500 }
  .pill.ok       { background:#1a4731; color:#7ee2a8 }
  .pill.degraded { background:#3a2f1a; color:#f0b056 }
  .pill.offline  { background:#3a1d22; color:#ff9492 }
  .pill.pending  { background:#28313d; color:#79c0ff }
  .pill.done     { background:#28323d; color:#a1b3c9 }

  .domain-tag { background:#1f2a36; color:#79c0ff; padding:0 6px;
                border-radius:3px; font-size:11px; margin-right:4px }
  .tag { background:#21262d; color:#c9d1d9; padding:0 6px;
         border-radius:3px; font-size:10px; margin-right:3px }
  .meta { color:#6e7681; font-size:11px }

  .speedup-good    { color:#7ee2a8 }
  .speedup-neutral { color:#c9d1d9 }
  .speedup-none    { color:#6e7681 }
  .no-emb { color:#ffa657 }

  table { width:100%; border-collapse: collapse; font-size:13px }
  th { text-align:left; padding:6px 8px; color:#8b949e; font-weight:500;
       border-bottom:1px solid #21262d }
  td { padding:6px 8px; border-bottom:1px solid #1a1f27; vertical-align:top }
  tr.clickable { cursor: pointer }
  tr.clickable:hover { background:#1a1f27 }

  .err { color:#ff9492; padding:8px; background:#3a1d22;
         border-radius:4px; margin:8px 0; font-size:12px }
  .empty { color:#6e7681; padding:12px; text-align:center }

  footer { padding: 12px 24px; color:#6e7681; font-size:11px;
           border-top: 1px solid #21262d; margin-top: 24px }
`

// dashboardBasePath returns the URL prefix the dashboard is mounted at,
// without a trailing slash. Set via DASHBOARD_BASE_PATH for Pi-style
// deployments where Caddy proxies /evo-api/* → evo-api:9123. Empty in
// local dev (binary served at the root). Resolved on every call so
// tests can flip the env in-process.
func dashboardBasePath() string {
	v := os.Getenv("DASHBOARD_BASE_PATH")
	v = strings.TrimRight(v, "/")
	return v
}

// dashboardHref prefixes a root-relative path with the dashboard's
// base path. dashboardHref("/skills") returns "/evo-api/skills" when
// DASHBOARD_BASE_PATH=/evo-api, and "/skills" when unset.
func dashboardHref(p string) string {
	base := dashboardBasePath()
	if base == "" {
		return p
	}
	if p == "" || p == "/" {
		return base + "/"
	}
	return base + p
}

// navBar returns the top-of-page nav, with the given page slug
// rendered as active. Slug is one of: "home", "delegator", "skills",
// "evo", "workflows", "research". Anything else just leaves nothing
// highlighted, which is the right behaviour for sub-pages.
func navBar(active string) string {
	a := func(slug, href, label string) string {
		cls := "link"
		if slug == active {
			cls += " active"
		}
		return `<a class="` + cls + `" href="` + dashboardHref(href) + `">` + label + `</a>`
	}
	// Prepend a tiny script that prefixes every root-rooted fetch URL
	// with the dashboard's base path when one is configured. This lets
	// inline-page JS keep using bare ``/api/v1/...`` paths even when
	// the dashboard is served under ``/evo-api`` behind Caddy.
	prelude := ""
	if base := dashboardBasePath(); base != "" {
		prelude = `<script>(function(){
  const B = ` + jsString(base) + `;
  const F = window.fetch;
  window.fetch = function(input, opts){
    if (typeof input === 'string' && input.length && input[0] === '/' && !input.startsWith(B)) {
      input = B + input;
    } else if (input && typeof input === 'object' && typeof input.url === 'string' &&
               input.url[0] === '/' && !input.url.startsWith(B)) {
      input = new Request(B + input.url, input);
    }
    return F.call(this, input, opts);
  };
})();</script>` + "\n"
	}

	return prelude + `<nav class="top">
  <span class="brand">evo · suite</span>
  ` + a("home", "/", "home") + `
  ` + a("delegator", "/delegator", "delegator") + `
  ` + a("skills", "/skills", "skills") + `
  ` + a("evo", "/evo", "evo") + `
  ` + a("workflows", "/workflows", "workflows") + `
  ` + a("research", "/research", "research") + `
  <span class="spacer"></span>
  <span class="meta">refresh 5s<span id="tick" class="refresh-tick"></span></span>
</nav>`
}

// jsString quotes ``s`` as a JavaScript string literal. Tiny helper —
// only used by the navBar fetch-prelude where ``s`` is a config string.
func jsString(s string) string {
	// JSON encoding is a strict subset of JS, so the trip through
	// strconv-equivalent escaping is safe (and supports unicode).
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '<':
			// Avoid closing the surrounding <script> tag accidentally.
			b.WriteString(`<`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
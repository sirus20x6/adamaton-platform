// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import "net/http"

// serveResearchPage renders the deepresearch dashboard panel. It
// shows local-side knowledge of the Pi (health, latency from our
// status fan-out) and a deep-link to the Pi's native UI for full
// operations. The proxy under /api/v1/research/* is what powers the
// "list collections" pull below — we don't try to recreate the Pi's
// rich library UI inline.
func (s *APIServer) serveResearchPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>evo · research</title>
<style>` + dashboardCSS + `
  .body { display:grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
          gap: 24px; padding: 24px }
  @media (max-width: 1000px) { .body { grid-template-columns: 1fr } }
  .big-link { display:inline-block; padding: 8px 14px; border-radius: 6px;
              background:#1f2a36; color:#79c0ff; font-size: 13px;
              border: 1px solid #21262d; margin-top: 10px }
  .big-link:hover { background:#28323d; text-decoration: none }
  .stat-grid { display:grid; grid-template-columns: repeat(auto-fit, minmax(120px,1fr));
               gap: 12px; margin-top: 12px }
  .stat-grid .stat { background:#0d1117; border:1px solid #21262d; border-radius:6px;
                     padding:10px }
  .stat-grid .stat .k { color:#8b949e; font-size: 11px; text-transform:uppercase;
                        letter-spacing: 0.04em }
  .stat-grid .stat .v { color:#c9d1d9; font-size: 18px;
                        font-variant-numeric: tabular-nums; margin-top: 4px }
</style>
</head>
<body>
` + navBar("research") + `

<div class="body">

<section>
  <h2>deepresearch — pi node</h2>
  <div id="dr-status">loading…</div>
  <div id="dr-meta" class="meta" style="margin-top:8px"></div>
  <a id="dr-open" class="big-link" target="_blank" rel="noreferrer">open the Pi's native UI →</a>
  <div class="stat-grid">
    <div class="stat"><div class="k">health latency</div><div class="v" id="dr-latency">—</div></div>
    <div class="stat"><div class="k">last check</div><div class="v" id="dr-when">—</div></div>
  </div>
  <h2 style="margin-top:20px">how this surface works</h2>
  <p style="color:#8b949e; font-size: 13px">
    The Pi runs LearningCircuit/local-deep-research behind a self-signed
    Caddy reverse proxy. The browser can't talk to it directly without
    cert prompts + CORS pain, so the dashboard proxies through
    <code>/api/v1/research/*</code> server-side. For research operations
    (kick off a new query, browse session results, manage collections),
    use the Pi's native UI via the link above.
  </p>
  <p style="color:#8b949e; font-size: 13px">
    Cross-tool flow: evo's <code>MemoryQueryActivity</code> calls this
    same Pi over a configured <code>DEEPRESEARCH_URL</code>. So insights
    from evo runs can pull from the same library you curate in the Pi's
    UI.
  </p>
</section>

<section>
  <h2>upstream endpoints</h2>
  <p style="color:#8b949e; font-size: 13px">Proxied through this dashboard so they're CORS-safe + cert-safe:</p>
  <table>
    <thead><tr><th>method</th><th>route</th><th>upstream</th><th>status</th></tr></thead>
    <tbody id="probes">
      <tr><td colspan="4" class="empty">probing…</td></tr>
    </tbody>
  </table>
</section>

</div>

<footer>
  <code>GET /api/v1/research/health</code> ·
  <code>GET /api/v1/research/library/api/&lt;collection&gt;/search</code> (POST) ·
  set <code>DEEPRESEARCH_URL</code> env to retarget
</footer>

<script>
const esc = s => (s || '').replace(/[&<>"']/g, c => ({
  '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
}[c]));
const fmtTime = ts => {
  if (!ts) return '';
  const d = new Date(ts);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return Math.floor(diff) + 's ago';
  if (diff < 3600) return Math.floor(diff/60) + 'm ago';
  return d.toLocaleString();
};
const statusPill = s => '<span class="pill ' + esc(s) + '">' + esc(s) + '</span>';

async function fetchJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + ' → ' + r.status);
  return r.json();
}

async function loadStatus() {
  let st;
  try { st = await fetchJSON('/api/v1/system/status'); }
  catch (e) { document.getElementById('dr-status').innerHTML = '<div class="err">' + esc(e.message) + '</div>'; return; }
  const dr = (st.subsystems || []).find(x => x.name === 'deepresearch');
  if (!dr) {
    document.getElementById('dr-status').innerHTML = '<div class="err">deepresearch not in status response</div>';
    return;
  }
  document.getElementById('dr-status').innerHTML =
    '<div style="font-size:18px">' + statusPill(dr.status) +
    '<span style="margin-left:10px; color:#8b949e">' + esc(dr.detail || '') + '</span></div>';
  document.getElementById('dr-meta').textContent = 'target: ' + (dr.url || 'unset');
  document.getElementById('dr-open').href = dr.url || '#';
  document.getElementById('dr-latency').textContent = dr.latency_ms.toFixed(1) + ' ms';
  document.getElementById('dr-when').textContent = fmtTime(st.generated_at);
}

async function probeRoutes() {
  const routes = [
    {m:'GET',  via:'/api/v1/research/health',          up:'/api/v1/health'},
    {m:'GET',  via:'/api/v1/research/api/v1/health',   up:'/api/v1/health'},
    {m:'GET',  via:'/api/v1/research/library',         up:'/library'},
  ];
  const probes = document.getElementById('probes');
  const results = await Promise.all(routes.map(async r => {
    try {
      const resp = await fetch(r.via, {method: r.m});
      return {...r, status: resp.status};
    } catch (e) {
      return {...r, status: 'err'};
    }
  }));
  probes.innerHTML = results.map(r => {
    const ok = r.status >= 200 && r.status < 400;
    const pill = ok ? statusPill('ok') : statusPill('offline');
    return '<tr>' +
      '<td><code>' + r.m + '</code></td>' +
      '<td><code>' + esc(r.via) + '</code></td>' +
      '<td><code>' + esc(r.up) + '</code></td>' +
      '<td>' + pill + ' <span class="meta">HTTP ' + esc(String(r.status)) + '</span></td>' +
    '</tr>';
  }).join('');
}

async function refresh() {
  const tick = document.getElementById('tick');
  if (tick) { tick.classList.add('live'); setTimeout(() => tick.classList.remove('live'), 400); }
  await Promise.all([loadStatus(), probeRoutes()]);
}

refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
	_, _ = w.Write([]byte(page))
}
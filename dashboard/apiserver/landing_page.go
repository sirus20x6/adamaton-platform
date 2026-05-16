// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import "net/http"

// serveLanding renders the unified-suite landing page. Inline HTML so
// it works without a frontend build; reuses chrome.go's nav + CSS so
// it stays visually consistent with /evo and /research.
func (s *APIServer) serveLanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>evo · suite</title>
<style>` + dashboardCSS + `
  .cards { display:grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
           gap: 16px; padding: 24px }
  .card { background:#161b22; border:1px solid #21262d; border-radius:8px; padding:16px }
  .card .head { display:flex; align-items:center; justify-content:space-between; margin-bottom:12px }
  .card .head .name { font-size: 16px; font-weight:600 }
  .card .stats { margin: 8px 0; }
  .card .stat-row { display:flex; justify-content:space-between; padding: 2px 0; font-size: 13px }
  .card .stat-row .k { color:#8b949e }
  .card .stat-row .v { color:#c9d1d9; font-variant-numeric: tabular-nums }
  .card .open { margin-top: 12px; display:inline-block; font-size: 12px }
  .card .latency { color:#6e7681; font-size: 11px; font-variant-numeric: tabular-nums }
  .card .detail { color:#ff9492; font-size: 12px; margin-top: 6px; font-family: "JetBrains Mono", Menlo, monospace }

  .activity { padding: 0 24px 24px 24px }
  .activity-grid { display:grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
                   gap: 16px }
  .activity-block { background:#161b22; border:1px solid #21262d; border-radius:8px;
                    padding:16px; min-width:0 }
  .activity-block h3 { margin: 0 0 10px 0; font-size: 13px; color:#8b949e;
                       text-transform: uppercase; letter-spacing: 0.04em }
  .activity-row { padding: 6px 0; border-bottom: 1px solid #1a1f27;
                  font-size: 13px; display:flex; justify-content:space-between; gap: 10px }
  .activity-row:last-child { border-bottom: none }
  .activity-row .when { color:#6e7681; font-size: 11px; white-space: nowrap }
  .activity-row .what { color:#c9d1d9; min-width: 0; overflow:hidden; text-overflow:ellipsis;
                        white-space: nowrap }
</style>
</head>
<body>
` + navBar("home") + `

<div class="cards" id="cards">
  <div class="empty">loading subsystem status…</div>
</div>

<div class="activity">
  <h2 style="padding-left:0">recent activity</h2>
  <div class="activity-grid" id="activity">
    <div class="empty">loading…</div>
  </div>
</div>

<footer>
  <code>GET /api/v1/system/status</code> · <code>/api/v1/evo/runs</code> · <code>/api/v1/delegator/tasks</code> · <code>/api/v1/research/health</code>
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
  if (diff < 86400) return Math.floor(diff/3600) + 'h ago';
  return d.toLocaleString();
};
const fmtNum = v => {
  if (typeof v !== 'number') return esc(String(v));
  if (Number.isInteger(v)) return v.toLocaleString();
  return v.toFixed(3);
};
const statusPill = s => '<span class="pill ' + esc(s) + '">' + esc(s) + '</span>';

async function fetchJSON(url, opts={}) {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(url + ' → ' + r.status);
  return r.json();
}

async function loadStatus() {
  const cards = document.getElementById('cards');
  let data;
  try {
    data = await fetchJSON('/api/v1/system/status');
  } catch (e) {
    cards.innerHTML = '<div class="err">status fetch failed: ' + esc(e.message) + '</div>';
    return null;
  }
  cards.innerHTML = data.subsystems.map(sub => {
    const statRows = Object.entries(sub.stats || {}).map(([k,v]) =>
      '<div class="stat-row"><span class="k">' + esc(k) + '</span>' +
      '<span class="v">' + fmtNum(v) + '</span></div>').join('');
    const detail = (sub.detail && sub.status !== 'ok')
      ? '<div class="detail">' + esc(sub.detail) + '</div>' : '';
    const openHref = sub.url && sub.url.startsWith('http')
      ? sub.url : (sub.url || '#');
    const openLabel = sub.url && sub.url.startsWith('http')
      ? 'open native UI →' : 'open ' + esc(sub.name) + ' →';
    return '<div class="card">' +
      '<div class="head">' +
        '<span class="name">' + esc(sub.name) + '</span>' +
        statusPill(sub.status) +
      '</div>' +
      '<div class="stats">' + statRows + '</div>' +
      detail +
      '<a class="open" href="' + esc(openHref) + '"' +
        (sub.url && sub.url.startsWith('http') ? ' target="_blank" rel="noreferrer"' : '') +
        '>' + openLabel + '</a>' +
      '<div class="latency">' + sub.latency_ms.toFixed(1) + ' ms</div>' +
    '</div>';
  }).join('');
  return data;
}

async function loadActivity() {
  const blocks = document.getElementById('activity');
  const [runsR, insightsR, tasksR] = await Promise.allSettled([
    fetchJSON('/api/v1/evo/runs'),
    fetchJSON('/api/v1/evo/insights'),
    fetchJSON('/api/v1/delegator/tasks?status=&agent='),
  ]);
  const block = (title, body) =>
    '<div class="activity-block"><h3>' + esc(title) + '</h3>' + body + '</div>';

  let evoBody = '';
  if (runsR.status === 'fulfilled' && runsR.value.length) {
    evoBody = runsR.value.slice(0, 5).map(r =>
      '<div class="activity-row">' +
        '<span class="what"><span class="domain-tag">' + esc(r.domain) + '</span> ' +
          esc(r.task_name) + ' · ' + r.num_programs + ' programs · ' +
          esc(r.status) +
        '</span>' +
        '<span class="when">' + fmtTime(r.started_at) + '</span>' +
      '</div>').join('');
  } else if (runsR.status === 'rejected') {
    evoBody = '<div class="err">' + esc(runsR.reason.message) + '</div>';
  } else {
    evoBody = '<div class="empty">no evo runs yet</div>';
  }

  let insightsBody = '';
  if (insightsR.status === 'fulfilled' && insightsR.value.length) {
    insightsBody = insightsR.value.slice(0, 5).map(i =>
      '<div class="activity-row">' +
        '<span class="what"><span class="domain-tag">' + esc(i.domain) + '</span> ' +
          esc(i.title) +
        '</span>' +
        '<span class="when">' + fmtTime(i.created_at) + '</span>' +
      '</div>').join('');
  } else if (insightsR.status === 'rejected') {
    insightsBody = '<div class="err">' + esc(insightsR.reason.message) + '</div>';
  } else {
    insightsBody = '<div class="empty">no insights yet</div>';
  }

  let tasksBody = '';
  if (tasksR.status === 'fulfilled' && Array.isArray(tasksR.value) && tasksR.value.length) {
    tasksBody = tasksR.value.slice(0, 5).map(t =>
      '<div class="activity-row">' +
        '<span class="what"><span class="domain-tag">' + esc(t.agent || '?') + '</span> ' +
          esc((t.prompt || '').slice(0, 80)) +
        '</span>' +
        '<span class="when">' + fmtTime(t.created_at) + '</span>' +
      '</div>').join('');
  } else if (tasksR.status === 'rejected') {
    tasksBody = '<div class="err">' + esc(tasksR.reason.message) + '</div>';
  } else {
    tasksBody = '<div class="empty">no delegator tasks yet</div>';
  }

  blocks.innerHTML =
    block('evo runs', evoBody) +
    block('evo insights', insightsBody) +
    block('delegator tasks', tasksBody);
}

async function refresh() {
  const tick = document.getElementById('tick');
  if (tick) { tick.classList.add('live'); setTimeout(() => tick.classList.remove('live'), 400); }
  await Promise.all([loadStatus(), loadActivity()]);
}

refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>`
	_, _ = w.Write([]byte(page))
}
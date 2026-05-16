// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"net/http"
)

// serveEvoDashboard returns the evo dashboard page. Inline HTML so the
// page works even when ui/dist hasn't been built (the React app is the
// long-term home; this MVP unblocks visibility without waiting on a
// frontend build). The page fetches /api/v1/evo/* via the same auth
// middleware as everything else.
func (s *APIServer) serveEvoDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(evoDashboardHTML))
}

// evoDashboardHTML is a single-file dashboard. Three panels:
//
//  1. Recent runs — last 50 evo.runs rows with task name, status,
//     program count, best speedup, click-to-expand programs.
//  2. Programs (expanded inline) — programs in a run by generation,
//     parent_id, speedup, correctness.
//  3. Recent insights — last 50 evo.insights with domain + title +
//     body + tags, color-coded by has_embedding.
//
// Built as a var (not const) so it can splice in dashboardCSS +
// navBar("evo"), both of which are runtime-constructed strings.
// Materialised once at init and served as a byte slice from there
// on — no per-request overhead.
var evoDashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>evo · runs</title>
<style>` + dashboardCSS + `
  .grid { display:grid; grid-template-columns: minmax(0,2fr) minmax(0,1fr);
          gap: 24px; padding: 24px }
  @media (max-width: 1100px) { .grid { grid-template-columns: 1fr } }
  tr.programs-row > td { padding: 0 0 8px 8px; background:#0d1117 }
  .insight-body { color:#8b949e; font-size:12px; margin-top:4px;
                  white-space: pre-wrap; word-break: break-word }
</style>
</head>
<body>
` + navBar("evo") + `

<div class="grid">

<section>
  <h2>Recent runs</h2>
  <div id="runs-err"></div>
  <table id="runs-table">
    <thead><tr>
      <th>run id</th><th>task</th><th>domain</th><th>status</th>
      <th>started</th><th>#progs</th><th>best speedup</th>
    </tr></thead>
    <tbody id="runs-body"></tbody>
  </table>
</section>

<section>
  <h2>Recent insights</h2>
  <div id="insights-err"></div>
  <div id="insights-list"></div>
</section>

</div>

<footer>
  refresh every 5s · <code>GET /api/v1/evo/runs</code> · <code>/runs/{id}/programs</code> · <code>/insights</code>
</footer>

<script>
const fmtTime = ts => {
  const d = new Date(ts);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff < 60) return Math.floor(diff) + 's ago';
  if (diff < 3600) return Math.floor(diff/60) + 'm ago';
  if (diff < 86400) return Math.floor(diff/3600) + 'h ago';
  return d.toLocaleString();
};
const fmtSpeedup = v => {
  if (v == null) return '<span class="speedup-none">—</span>';
  const cls = v >= 1.1 ? 'speedup-good' : 'speedup-neutral';
  return '<span class="' + cls + '">' + v.toFixed(3) + '×</span>';
};
const statusPill = s => {
  const cls = s === 'completed' ? 'done'
            : s === 'running'   ? 'pending'
            : 'offline';
  return '<span class="pill ' + cls + '">' + s + '</span>';
};
const boolPill = (b, ok='ok', no='no') =>
  '<span class="pill ' + (b ? 'ok' : 'offline') + '">' + (b ? ok : no) + '</span>';
const esc = s => (s || '').replace(/[&<>"']/g, c => ({
  '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
}[c]));

async function fetchJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + ' → ' + r.status);
  return r.json();
}

async function loadPrograms(runId, container) {
  try {
    const progs = await fetchJSON('/api/v1/evo/runs/' + encodeURIComponent(runId) + '/programs');
    if (!progs.length) {
      container.innerHTML = '<div class="empty">(no programs yet)</div>';
      return;
    }
    const rows = progs.map(p => '<tr>' +
      '<td>' + p.id + '</td>' +
      '<td>' + (p.parent_id ?? '—') + '</td>' +
      '<td>' + p.generation + '</td>' +
      '<td>' + boolPill(p.compiled, 'c') + ' ' + boolPill(p.correct, 'ok') + '</td>' +
      '<td>' + fmtSpeedup(p.speedup) + '</td>' +
      '<td class="meta">' + esc(p.backend) + ' · ' + fmtTime(p.created_at) + '</td>' +
    '</tr>').join('');
    container.innerHTML =
      '<table><thead><tr><th>id</th><th>parent</th><th>gen</th>' +
      '<th>compiled / correct</th><th>speedup</th><th>backend · created</th>' +
      '</tr></thead><tbody>' + rows + '</tbody></table>';
  } catch (e) {
    container.innerHTML = '<div class="err">programs: ' + esc(e.message) + '</div>';
  }
}

let expandedRunId = null;

async function loadRuns() {
  const errEl = document.getElementById('runs-err');
  errEl.innerHTML = '';
  let runs;
  try {
    runs = await fetchJSON('/api/v1/evo/runs');
  } catch (e) {
    errEl.innerHTML = '<div class="err">' + esc(e.message) + '</div>';
    return;
  }
  const body = document.getElementById('runs-body');
  if (!runs.length) {
    body.innerHTML = '<tr><td colspan="7" class="empty">(no runs yet — try ./bin/evo-cli run --task ... --skip-eval)</td></tr>';
    return;
  }
  body.innerHTML = runs.map(r => {
    const idHtml = '<code>' + esc(r.id) + '</code>';
    return '<tr class="run" data-run-id="' + esc(r.id) + '">' +
      '<td>' + idHtml + '</td>' +
      '<td>' + esc(r.task_name) + '</td>' +
      '<td><span class="domain-tag">' + esc(r.domain) + '</span></td>' +
      '<td>' + statusPill(r.status) + '</td>' +
      '<td class="meta">' + fmtTime(r.started_at) + '</td>' +
      '<td>' + r.num_programs + '</td>' +
      '<td>' + fmtSpeedup(r.best_speedup) + '</td>' +
    '</tr>' +
    '<tr class="programs-row" id="progs-' + esc(r.id) + '" style="display:none">' +
      '<td colspan="7"><div class="progs-container"></div></td>' +
    '</tr>';
  }).join('');

  body.querySelectorAll('tr.run').forEach(tr => {
    tr.addEventListener('click', () => {
      const id = tr.dataset.runId;
      const progRow = document.getElementById('progs-' + id);
      if (expandedRunId === id) {
        progRow.style.display = 'none';
        expandedRunId = null;
        return;
      }
      body.querySelectorAll('tr.programs-row').forEach(r => r.style.display = 'none');
      expandedRunId = id;
      progRow.style.display = '';
      loadPrograms(id, progRow.querySelector('.progs-container'));
    });
  });
}

async function loadInsights() {
  const errEl = document.getElementById('insights-err');
  errEl.innerHTML = '';
  let insights;
  try {
    insights = await fetchJSON('/api/v1/evo/insights');
  } catch (e) {
    errEl.innerHTML = '<div class="err">' + esc(e.message) + '</div>';
    return;
  }
  const list = document.getElementById('insights-list');
  if (!insights.length) {
    list.innerHTML = '<div class="empty">(no insights yet — they\'re minted after improvement on a prior best)</div>';
    return;
  }
  list.innerHTML = insights.map(i => {
    const tags = (i.tags || []).map(t => '<span class="tag">' + esc(t) + '</span>').join('');
    const embed = i.has_embedding ? '' : '<span class="no-emb"> · NULL embedding</span>';
    return '<div style="margin-bottom:14px">' +
      '<div><span class="domain-tag">' + esc(i.domain) + '</span> ' +
      '<strong>' + esc(i.title) + '</strong>' + embed + '</div>' +
      '<div style="margin-top:2px">' + tags + '</div>' +
      '<div class="insight-body">' + esc(i.body) + '</div>' +
      '<div class="meta">id=' + i.id + ' · ' + fmtTime(i.created_at) +
      (i.source_program_id ? ' · program ' + i.source_program_id : '') + '</div>' +
    '</div>';
  }).join('');
}

async function refresh() {
  const tick = document.getElementById('tick');
  tick.classList.add('live');
  setTimeout(() => tick.classList.remove('live'), 400);
  await Promise.all([loadRuns(), loadInsights()]);
}

refresh();
setInterval(refresh, 5000);
</script>
</body>
</html>
`
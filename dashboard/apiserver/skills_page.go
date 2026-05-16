// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import "net/http"

// serveSkillsPage renders the inline-HTML /skills page. Same convention
// as serveEvoDashboard: server-rendered shell, all data fetched via
// /api/v1/skills/* with inline JS. Phase 1 ships browse + create +
// edit + delete; Phase 3 adds an Import button + dialog; Phase 4 adds
// a Graph tab.
func (s *APIServer) serveSkillsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(skillsPageHTML))
}

var skillsPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>evo · skills</title>
<style>` + dashboardCSS + `
  .toolbar { display:flex; gap:8px; align-items:center;
             padding: 16px 24px; flex-wrap: wrap;
             border-bottom:1px solid #21262d }
  .tabs { display:flex; gap:4px; margin-right: 8px;
          background:#0d1117; border:1px solid #21262d; border-radius:8px; padding:2px }
  .tab { background:transparent; border:0; color:#8b949e;
         padding:5px 12px; border-radius:6px; font: inherit; font-size:13px; cursor:pointer }
  .tab.active { background:#1f2a36; color:#79c0ff }
  .sem-toggle { display:flex; align-items:center; gap:4px; color:#8b949e; font-size:12px; cursor:pointer }
  .sem-toggle input { margin:0 }
  .toolbar input, .toolbar select {
    background:#0d1117; color:#c9d1d9; border:1px solid #21262d;
    border-radius:6px; padding:6px 10px; font: inherit; font-size:13px;
  }
  .toolbar input.search { flex:1; min-width:200px; max-width:360px }
  .toolbar button {
    background:#1f6feb; color:#fff; border:0;
    border-radius:6px; padding:7px 14px; font: inherit; font-size:13px;
    cursor: pointer;
  }
  .toolbar button.ghost { background:#21262d; color:#c9d1d9 }
  .toolbar button:hover { filter: brightness(1.1) }

  .body { padding: 16px 24px }
  .empty-page { color:#6e7681; padding: 40px; text-align:center }

  .group { margin-bottom: 28px }
  .group-head {
    color:#8b949e; font-size:12px; text-transform: uppercase;
    letter-spacing: 0.04em; margin-bottom:10px;
  }
  .group-head .count { color:#6e7681; font-weight:400; margin-left:4px }

  .cards { display:grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
           gap: 12px }
  .card {
    background:#161b22; border:1px solid #21262d; border-radius:8px;
    padding:12px; cursor:pointer;
  }
  .card:hover { border-color:#30363d; background:#1a1f27 }
  .card.active { border-color:#79c0ff }
  .card .name { font-weight:600; color:#c9d1d9; font-size:13px;
                margin-bottom:4px; overflow:hidden; text-overflow:ellipsis;
                white-space:nowrap }
  .card .desc { color:#8b949e; font-size:12px; line-height:1.4;
                display:-webkit-box; -webkit-line-clamp:2;
                -webkit-box-orient:vertical; overflow:hidden }
  .card .row { display:flex; gap:6px; margin-top:8px; flex-wrap: wrap; align-items:center }
  .card .row .meta { margin-left:auto }

  .detail {
    margin-top: 16px;
    background:#0f1419; border:1px solid #2a3441; border-radius:8px;
    padding: 18px;
  }
  .detail h3 { margin: 0 0 4px 0; font-size:16px; color:#c9d1d9 }
  .detail .desc { color:#8b949e; font-size:13px; margin-bottom: 12px }
  .detail .src {
    color:#6e7681; font-size:11px; margin-bottom:12px;
    border-top:1px solid #21262d; padding-top:10px;
  }
  .detail .src code { background:#161b22; padding:1px 5px; border-radius:3px }
  .detail .section-label {
    color:#8b949e; font-size:11px; text-transform:uppercase;
    letter-spacing:0.04em; margin-top:14px; margin-bottom:4px;
  }
  .detail pre.body {
    background:#161b22; border:1px solid #21262d; border-radius:6px;
    padding:12px; overflow-x:auto; white-space: pre-wrap; word-break: break-word;
    font-size:12px; color:#c9d1d9; max-height:50vh; overflow-y:auto;
  }
  .detail .deps { display:flex; gap:6px; flex-wrap: wrap }
  .detail .actions { display:flex; gap:8px; margin-top:16px;
                     padding-top:12px; border-top:1px solid #21262d }
  .detail .actions button {
    background:#21262d; color:#c9d1d9; border:0;
    border-radius:6px; padding:7px 14px; font: inherit; font-size:13px;
    cursor: pointer;
  }
  .detail .actions button.primary { background:#1f6feb; color:#fff }
  .detail .actions button.danger  { background:#3a1d22; color:#ff9492 }
  .detail .actions button:hover { filter: brightness(1.15) }

  /* Modal */
  .modal-bg {
    position:fixed; inset:0; background:rgba(0,0,0,0.6);
    display:none; align-items:center; justify-content:center; z-index:50;
  }
  .modal-bg.open { display:flex }
  .modal {
    background:#0d1117; border:1px solid #30363d; border-radius:10px;
    width: min(720px, 92vw); max-height: 90vh; display:flex; flex-direction:column;
  }
  .modal-head, .modal-foot {
    padding: 14px 18px; border-bottom:1px solid #21262d;
    display:flex; justify-content:space-between; align-items:center;
  }
  .modal-foot { border-top:1px solid #21262d; border-bottom:0; gap:8px; justify-content:flex-end }
  .modal-head h3 { margin:0; font-size:14px; color:#c9d1d9 }
  .modal-body { padding: 18px; overflow-y:auto }
  .field { margin-bottom: 14px }
  .field label { display:block; color:#8b949e; font-size:11px;
                 text-transform:uppercase; letter-spacing:0.04em; margin-bottom:5px }
  .field input, .field textarea, .field select {
    width:100%; background:#0d1117; color:#c9d1d9;
    border:1px solid #21262d; border-radius:6px;
    padding:8px 10px; font: inherit; font-size:13px;
  }
  .field textarea { font-family: "JetBrains Mono", Menlo, monospace; font-size:12px;
                    resize: vertical; min-height: 100px }
  .field textarea.body { min-height: 220px }
  .field .hint { color:#6e7681; font-size:11px; margin-top:4px }
  .field-row { display:grid; grid-template-columns:1fr 1fr; gap:14px }
  button.x { background:transparent; color:#8b949e; border:0; font-size:18px; cursor:pointer; padding:0 4px }
  button.x:hover { color:#c9d1d9 }

  .graph-meta { color:#8b949e; font-size:12px; padding: 0 0 8px 0 }
  #graph-svg { background:#0f1419; border:1px solid #21262d; border-radius:8px }
  #graph-svg .node { cursor: pointer }
  #graph-svg .node circle { stroke:#30363d; stroke-width:1.5 }
  #graph-svg .node.selected circle { stroke:#79c0ff; stroke-width:2.5 }
  #graph-svg .node text { fill:#c9d1d9; font-size:11px; pointer-events:none;
                          paint-order: stroke; stroke:#0f1419; stroke-width:3 }
  #graph-svg .edge { stroke-opacity:0.4; fill:none }
  #graph-svg .edge.depends_on { stroke:#79c0ff }
  #graph-svg .edge.r2r        { stroke:#7ee2a8; stroke-dasharray: 4 3 }
  .legend { display:flex; gap:14px; font-size:11px; color:#8b949e; margin-top:8px }
  .legend .swatch { display:inline-block; width:14px; height:2px; vertical-align:middle; margin-right:5px }
  .legend .swatch.depends_on { background:#79c0ff }
  .legend .swatch.r2r        { background:#7ee2a8 }
  .search-hits { display:grid; gap:10px }
  .search-hit { background:#161b22; border:1px solid #21262d; border-radius:8px; padding:12px; cursor:pointer }
  .search-hit:hover { border-color:#30363d; background:#1a1f27 }
  .search-hit .hit-head { display:flex; align-items:baseline; gap:8px }
  .search-hit .hit-head .name { font-weight:600 }
  .search-hit .hit-head .score { color:#7ee2a8; font-size:11px; margin-left:auto }
  .search-hit .hit-text { color:#8b949e; font-size:12px; margin-top:6px;
                          white-space:pre-wrap; word-break:break-word;
                          display:-webkit-box; -webkit-line-clamp:4;
                          -webkit-box-orient:vertical; overflow:hidden }
</style>
</head>
<body>
` + navBar("skills") + `

<div class="toolbar">
  <div class="tabs">
    <button class="tab active" data-tab="cards">Cards</button>
    <button class="tab" data-tab="graph">Graph</button>
  </div>
  <input class="search" id="q" placeholder="search name or description...">
  <select id="filter-community"><option value="">(all communities)</option></select>
  <select id="filter-origin">
    <option value="">(all origins)</option>
    <option value="manual">manual</option>
    <option value="claude-files">claude-files</option>
    <option value="github">github</option>
    <option value="mined">mined</option>
  </select>
  <label class="sem-toggle"><input type="checkbox" id="sem-search"> semantic</label>
  <span class="spacer" style="flex:1"></span>
  <button class="ghost" id="import-btn">Import…</button>
  <button id="add-btn">+ Add Skill</button>
</div>

<div class="body" id="body">
  <div id="cards-view">
    <div class="empty-page" id="empty-msg">loading skills...</div>
  </div>
  <div id="graph-view" style="display:none">
    <div class="graph-meta" id="graph-meta"></div>
    <svg id="graph-svg" width="100%" height="640" preserveAspectRatio="xMidYMid meet"></svg>
  </div>
</div>

<!-- import modal -->
<div class="modal-bg" id="import-bg">
  <div class="modal">
    <div class="modal-head">
      <h3>Import skills</h3>
      <button class="x" id="import-x">&times;</button>
    </div>
    <div class="modal-body">
      <div class="field">
        <label>source</label>
        <select id="imp-kind">
          <option value="claude-files">Claude Code skills (~/.claude/skills/)</option>
          <option value="local-dir">Local directory</option>
          <option value="local-file">Local file</option>
          <option value="github-file">GitHub file (raw or blob URL)</option>
          <option value="github-repo">GitHub repo (recursive)</option>
        </select>
      </div>
      <div class="field" id="imp-path-row">
        <label>path</label>
        <input id="imp-path" placeholder="leave blank for default ~/.claude/skills">
      </div>
      <div class="field" id="imp-url-row" style="display:none">
        <label>url</label>
        <input id="imp-url" placeholder="https://github.com/owner/repo/blob/main/skills/x.md">
      </div>
      <div class="field" id="imp-repo-row" style="display:none">
        <label>repo url</label>
        <input id="imp-repo" placeholder="https://github.com/owner/repo">
      </div>
      <div class="field" id="imp-subpath-row" style="display:none">
        <label>subpath inside repo</label>
        <input id="imp-subpath" placeholder="skills/ (default)">
      </div>
      <div class="field-row">
        <div class="field">
          <label>community (default for imported skills)</label>
          <input id="imp-community" placeholder="optional">
        </div>
        <div class="field">
          <label>extra tags (comma-separated)</label>
          <input id="imp-tags" placeholder="optional">
        </div>
      </div>
      <div class="hint">github authentication uses <code>GITHUB_TOKEN</code> from the server env if present.</div>
      <div id="imp-results" style="margin-top:16px"></div>
    </div>
    <div class="modal-foot">
      <div id="imp-err" style="color:#ff9492; font-size:12px; margin-right:auto"></div>
      <button class="ghost" id="imp-close">Close</button>
      <button id="imp-run">Run import</button>
    </div>
  </div>
</div>

<!-- add/edit modal -->
<div class="modal-bg" id="modal-bg">
  <div class="modal">
    <div class="modal-head">
      <h3 id="modal-title">Add skill</h3>
      <button class="x" id="modal-x">&times;</button>
    </div>
    <div class="modal-body">
      <div class="field-row">
        <div class="field">
          <label>name</label>
          <input id="f-name" placeholder="extract-method">
        </div>
        <div class="field">
          <label>community</label>
          <input id="f-community" placeholder="code-refactoring" list="communities-list">
          <datalist id="communities-list"></datalist>
        </div>
      </div>
      <div class="field">
        <label>description</label>
        <input id="f-description" placeholder="one-line summary of what this skill does">
      </div>
      <div class="field-row">
        <div class="field">
          <label>tags (comma-separated)</label>
          <input id="f-tags" placeholder="python, ast, refactor">
        </div>
        <div class="field">
          <label>depends_on (skill ids, comma-separated)</label>
          <input id="f-depends" placeholder="optional">
        </div>
      </div>
      <div class="field">
        <label>when to use (optional)</label>
        <textarea id="f-when"></textarea>
      </div>
      <div class="field">
        <label>example (optional)</label>
        <textarea id="f-example"></textarea>
      </div>
      <div class="field">
        <label>body (markdown)</label>
        <textarea id="f-body" class="body" required></textarea>
        <div class="hint">supports markdown; code blocks render with triple backticks</div>
      </div>
    </div>
    <div class="modal-foot">
      <div id="modal-err" style="color:#ff9492; font-size:12px; margin-right:auto"></div>
      <button class="ghost" id="modal-cancel">Cancel</button>
      <button id="modal-save">Save</button>
    </div>
  </div>
</div>

<footer>
  <code>GET /api/v1/skills</code> · <code>POST /api/v1/skills</code> ·
  <code>PUT /api/v1/skills/{id}</code> · <code>DELETE /api/v1/skills/{id}</code>
</footer>

<script>
const esc = s => (s || '').replace(/[&<>"']/g, c => ({
  '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
}[c]));
const fmtTime = ts => {
  if (!ts) return '—';
  const d = new Date(ts);
  return d.toLocaleString();
};
const originPill = o => '<span class="pill done">' + esc(o || 'manual') + '</span>';

let allSkills = [];
let selectedId = null;
let editingId = null;
let activeTab = 'cards';
let semanticDebounce = null;
let lastSemHits = null;

async function fetchJSON(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) {
    let detail = r.status + '';
    try { const j = await r.json(); detail = j.error || detail; } catch {}
    throw new Error(detail);
  }
  if (r.status === 204) return null;
  return r.json();
}

function tagsHTML(tags) {
  return (tags || []).map(t => '<span class="tag">' + esc(t) + '</span>').join('');
}

function cardHTML(sk) {
  const cls = sk.id === selectedId ? 'card active' : 'card';
  const usage = (sk.usage_count > 0)
    ? '<span class="meta" title="surfaced into delegator tasks">· ' + sk.usage_count + '×</span>'
    : '';
  return '<div class="' + cls + '" data-id="' + sk.id + '">' +
    '<div class="name">' + esc(sk.name) + '</div>' +
    '<div class="desc">' + esc(sk.description) + '</div>' +
    '<div class="row">' + tagsHTML(sk.tags) +
      '<span class="meta">' + originPill(sk.origin) + usage + '</span>' +
    '</div>' +
  '</div>';
}

function detailHTML(sk) {
  const deps = (sk.depends_on || []).map(d => {
    const dep = allSkills.find(x => x.id === d);
    return '<span class="tag">' + esc(dep ? dep.name : d) + '</span>';
  }).join('');
  const optBlock = (label, val) => val ?
    '<div class="section-label">' + label + '</div><pre class="body">' + esc(val) + '</pre>' : '';
  return '<div class="detail" id="detail">' +
    '<h3>' + esc(sk.name) + '</h3>' +
    '<div class="desc">' + esc(sk.description) + '</div>' +
    '<div>' + tagsHTML(sk.tags) + ' ' + originPill(sk.origin) +
      (sk.community ? ' <span class="tag">community: ' + esc(sk.community) + '</span>' : '') +
    '</div>' +
    '<div class="src">' +
      (sk.source_url ? 'source: <code>' + esc(sk.source_url) + '</code><br>' : '') +
      (sk.source_sha ? 'sha: <code>' + esc(sk.source_sha.slice(0, 12)) + '…</code> · ' : '') +
      'created: ' + esc(fmtTime(sk.created_at)) + ' · updated: ' + esc(fmtTime(sk.updated_at)) +
    '</div>' +
    optBlock('when to use', sk.when_to_use) +
    optBlock('example', sk.example) +
    '<div class="section-label">body</div>' +
    '<pre class="body">' + esc(sk.body) + '</pre>' +
    (deps ? '<div class="section-label">depends on</div><div class="deps">' + deps + '</div>' : '') +
    '<div class="actions">' +
      '<button class="primary" id="edit-btn">Edit</button>' +
      (sk.source_url ? '<button id="checksrc-btn">Check Source</button>' : '') +
      '<button class="danger" id="delete-btn">Delete</button>' +
    '</div>' +
    (sk.usage_count > 0
      ? '<div class="meta" style="margin-top:10px">Surfaced into <strong>' + sk.usage_count +
        '</strong> delegator task' + (sk.usage_count === 1 ? '' : 's') + ' so far.</div>'
      : '') +
  '</div>';
}

function render() {
  if (activeTab === 'graph') { renderGraph(); return; }
  if (lastSemHits) { renderSemanticHits(); return; }
  const q = document.getElementById('q').value.toLowerCase();
  const comm = document.getElementById('filter-community').value;
  const origin = document.getElementById('filter-origin').value;
  const filtered = allSkills.filter(sk => {
    if (comm && sk.community !== comm) return false;
    if (origin && sk.origin !== origin) return false;
    if (q && !sk.name.toLowerCase().includes(q) &&
            !sk.description.toLowerCase().includes(q)) return false;
    return true;
  });

  const body = document.getElementById('cards-view');
  if (!filtered.length) {
    body.innerHTML = '<div class="empty-page">' +
      (allSkills.length ? 'no skills match the current filters.' :
        'no skills yet. click <strong>+ Add Skill</strong> to create one.') +
      '</div>';
    return;
  }
  const groups = {};
  filtered.forEach(sk => {
    const k = sk.community || '(uncategorised)';
    (groups[k] = groups[k] || []).push(sk);
  });
  const keys = Object.keys(groups).sort();
  body.innerHTML = keys.map(k =>
    '<div class="group">' +
      '<div class="group-head">' + esc(k) + ' <span class="count">(' + groups[k].length + ')</span></div>' +
      '<div class="cards">' + groups[k].map(cardHTML).join('') + '</div>' +
    '</div>'
  ).join('') +
  (selectedId ? detailHTML(allSkills.find(x => x.id === selectedId) || {}) : '');
  bindCardHandlers(body);
}

function bindCardHandlers(body) {

  body.querySelectorAll('.card,.search-hit').forEach(el => {
    el.addEventListener('click', () => {
      selectedId = selectedId === el.dataset.id ? null : el.dataset.id;
      render();
    });
  });
  const editBtn = document.getElementById('edit-btn');
  if (editBtn) editBtn.addEventListener('click', () => openModal(selectedId));
  const delBtn = document.getElementById('delete-btn');
  if (delBtn) delBtn.addEventListener('click', () => deleteSkill(selectedId));
  const csBtn = document.getElementById('checksrc-btn');
  if (csBtn) csBtn.addEventListener('click', () => checkSource(selectedId));
}

// ---- semantic search ----
async function runSemanticSearch() {
  const q = document.getElementById('q').value.trim();
  if (!q) { lastSemHits = null; render(); return; }
  try {
    const r = await fetchJSON('/api/v1/skills/search', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({ query: q, limit: 20 }),
    });
    lastSemHits = r.hits || [];
  } catch (e) {
    lastSemHits = [];
    document.getElementById('cards-view').innerHTML =
      '<div class="err">semantic search failed: ' + esc(e.message) + '</div>';
    return;
  }
  render();
}

function renderSemanticHits() {
  const body = document.getElementById('cards-view');
  if (!lastSemHits.length) {
    body.innerHTML = '<div class="empty-page">no semantic matches.</div>';
    return;
  }
  const cards = lastSemHits.map(h => {
    const sk = allSkills.find(s => s.id === h.skill_id);
    const name = h.skill_name || (sk && sk.name) || h.document_id || '?';
    return '<div class="search-hit" data-id="' + esc(h.skill_id || '') + '">' +
      '<div class="hit-head">' +
        '<span class="name">' + esc(name) + '</span>' +
        (h.chunk_kind ? '<span class="tag">' + esc(h.chunk_kind) + '</span>' : '') +
        (h.community ? '<span class="tag">' + esc(h.community) + '</span>' : '') +
        '<span class="score">score ' + (h.score||0).toFixed(3) + '</span>' +
      '</div>' +
      '<div class="hit-text">' + esc(h.text || '') + '</div>' +
    '</div>';
  }).join('');
  body.innerHTML = '<div class="meta" style="margin-bottom:12px">' +
      lastSemHits.length + ' semantic results for "' +
      esc(document.getElementById('q').value) + '"</div>' +
    '<div class="search-hits">' + cards + '</div>' +
    (selectedId ? detailHTML(allSkills.find(x => x.id === selectedId) || {}) : '');
  bindCardHandlers(body);
}

// ---- graph view ----
async function renderGraph() {
  const meta = document.getElementById('graph-meta');
  const svg = document.getElementById('graph-svg');
  meta.textContent = 'loading graph…';
  svg.innerHTML = '';
  let data;
  try {
    data = await fetchJSON('/api/v1/skills/graph');
  } catch (e) {
    meta.innerHTML = '<span class="err">graph load failed: ' + esc(e.message) + '</span>';
    return;
  }
  const r2rNote = data.r2r_available
    ? 'R2R graph: ' + esc(data.r2r_note || 'connected')
    : 'R2R graph: <em>' + esc(data.r2r_note || 'unavailable') + '</em>';
  meta.innerHTML = data.nodes.length + ' nodes, ' + data.edges.length + ' edges · ' + r2rNote +
    '<div class="legend">' +
      '<span><span class="swatch depends_on"></span>depends_on (evo)</span>' +
      '<span><span class="swatch r2r"></span>r2r-extracted</span>' +
    '</div>';
  layoutAndDrawGraph(data, svg);
}

// layoutAndDrawGraph runs a tiny Verlet-style force simulation and
// writes SVG nodes/edges. Capped at ~300 nodes; bigger graphs need a
// proper d3-force pass which we'll vendor when we hit that wall.
function layoutAndDrawGraph(data, svg) {
  const W = svg.clientWidth || 800;
  const H = svg.clientHeight || 640;
  svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);

  // Community-coloured palette (deterministic via hash so colours stick
  // across reloads).
  function colorFor(community) {
    if (!community) return '#8b949e';
    let h = 0; for (let i = 0; i < community.length; i++) h = (h*31 + community.charCodeAt(i)) & 0xffffffff;
    const hue = Math.abs(h) % 360;
    return 'hsl(' + hue + ', 55%, 60%)';
  }

  // Initialise positions on a circle so the simulation doesn't start
  // from a degenerate point.
  const N = data.nodes.length;
  const nodes = data.nodes.map((n, i) => ({
    id: n.id, name: n.name, community: n.community, origin: n.origin,
    x: W/2 + Math.cos(2*Math.PI*i/Math.max(N,1)) * Math.min(W, H)*0.35,
    y: H/2 + Math.sin(2*Math.PI*i/Math.max(N,1)) * Math.min(W, H)*0.35,
    vx: 0, vy: 0,
  }));
  const byId = {}; nodes.forEach(n => byId[n.id] = n);
  const edges = (data.edges || []).filter(e => byId[e.source] && byId[e.target]);

  const repulsion = 1200;
  const spring = 0.04;
  const restLen = 90;
  const damping = 0.82;
  const steps = 220;

  for (let step = 0; step < steps; step++) {
    // Pairwise repulsion (O(N²) — fine up to a few hundred nodes).
    for (let i = 0; i < nodes.length; i++) {
      for (let j = i+1; j < nodes.length; j++) {
        const a = nodes[i], b = nodes[j];
        let dx = a.x - b.x, dy = a.y - b.y;
        let d2 = dx*dx + dy*dy + 0.01;
        const f = repulsion / d2;
        const d = Math.sqrt(d2);
        const fx = f * dx / d, fy = f * dy / d;
        a.vx += fx; a.vy += fy;
        b.vx -= fx; b.vy -= fy;
      }
    }
    // Spring along edges.
    edges.forEach(e => {
      const a = byId[e.source], b = byId[e.target];
      const dx = b.x - a.x, dy = b.y - a.y;
      const d = Math.sqrt(dx*dx + dy*dy) + 0.01;
      const f = spring * (d - restLen);
      const fx = f * dx / d, fy = f * dy / d;
      a.vx += fx; a.vy += fy;
      b.vx -= fx; b.vy -= fy;
    });
    // Centring pull so disconnected components don't fly off to infinity.
    nodes.forEach(n => {
      n.vx += (W/2 - n.x) * 0.0008;
      n.vy += (H/2 - n.y) * 0.0008;
    });
    nodes.forEach(n => {
      n.vx *= damping; n.vy *= damping;
      n.x += n.vx;     n.y += n.vy;
      n.x = Math.max(30, Math.min(W-30, n.x));
      n.y = Math.max(30, Math.min(H-30, n.y));
    });
  }

  // Render — edges first so nodes sit on top.
  const svgNS = 'http://www.w3.org/2000/svg';
  const frag = document.createDocumentFragment();
  edges.forEach(e => {
    const a = byId[e.source], b = byId[e.target];
    const line = document.createElementNS(svgNS, 'line');
    line.setAttribute('x1', a.x); line.setAttribute('y1', a.y);
    line.setAttribute('x2', b.x); line.setAttribute('y2', b.y);
    line.setAttribute('class', 'edge ' + (e.kind || 'depends_on'));
    line.setAttribute('stroke-width', e.weight ? Math.min(3, 1 + e.weight) : 1.4);
    if (e.label) {
      const t = document.createElementNS(svgNS, 'title');
      t.textContent = e.label;
      line.appendChild(t);
    }
    frag.appendChild(line);
  });
  nodes.forEach(n => {
    const g = document.createElementNS(svgNS, 'g');
    g.setAttribute('class', 'node' + (n.id === selectedId ? ' selected' : ''));
    g.setAttribute('transform', 'translate(' + n.x.toFixed(1) + ',' + n.y.toFixed(1) + ')');
    g.dataset.id = n.id;
    const c = document.createElementNS(svgNS, 'circle');
    c.setAttribute('r', 9);
    c.setAttribute('fill', colorFor(n.community));
    const tx = document.createElementNS(svgNS, 'text');
    tx.setAttribute('y', -12);
    tx.setAttribute('text-anchor', 'middle');
    tx.textContent = n.name;
    g.appendChild(c);
    g.appendChild(tx);
    g.addEventListener('click', () => {
      selectedId = (selectedId === n.id) ? null : n.id;
      // Switch back to the cards view with this skill expanded.
      switchTab('cards');
    });
    frag.appendChild(g);
  });
  svg.appendChild(frag);
}

function switchTab(name) {
  activeTab = name;
  document.querySelectorAll('.tab').forEach(t => {
    t.classList.toggle('active', t.dataset.tab === name);
  });
  document.getElementById('cards-view').style.display = (name === 'cards') ? '' : 'none';
  document.getElementById('graph-view').style.display = (name === 'graph') ? '' : 'none';
  render();
}

async function checkSource(id) {
  if (!id) return;
  try {
    const r = await fetchJSON('/api/v1/skills/' + encodeURIComponent(id) + '/check-source', { method:'POST' });
    if (!r.changed) {
      alert('source unchanged · ' + (r.reason || ''));
      return;
    }
    const sk = allSkills.find(s => s.id === id);
    if (!sk) return;
    if (!confirm('Source has new content for "' + (sk.name||id) + '". Replace the stored body with the upstream version?')) {
      return;
    }
    await fetchJSON('/api/v1/skills/' + encodeURIComponent(id), {
      method:'PUT', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({
        name: sk.name, description: sk.description, body: r.new_body,
        when_to_use: sk.when_to_use || null, example: sk.example || null,
        community: sk.community || null, tags: sk.tags || [], depends_on: sk.depends_on || [],
      }),
    });
    await reload();
  } catch (e) {
    alert('check failed: ' + e.message);
  }
}

function refreshCommunities() {
  const opts = Array.from(new Set(allSkills.map(s => s.community).filter(Boolean))).sort();
  document.getElementById('filter-community').innerHTML =
    '<option value="">(all communities)</option>' +
    opts.map(c => '<option value="' + esc(c) + '">' + esc(c) + '</option>').join('');
  document.getElementById('communities-list').innerHTML =
    opts.map(c => '<option value="' + esc(c) + '">').join('');
}

async function reload() {
  try {
    allSkills = await fetchJSON('/api/v1/skills');
  } catch (e) {
    document.getElementById('body').innerHTML =
      '<div class="err">failed to load: ' + esc(e.message) + '</div>';
    return;
  }
  refreshCommunities();
  render();
}

function openModal(id) {
  editingId = id || null;
  document.getElementById('modal-title').textContent = id ? 'Edit skill' : 'Add skill';
  document.getElementById('modal-err').textContent = '';
  const fields = ['name','description','body','when','example','community','tags','depends'];
  fields.forEach(f => { const el = document.getElementById('f-' + f); if (el) el.value = ''; });
  if (id) {
    const sk = allSkills.find(s => s.id === id);
    if (sk) {
      document.getElementById('f-name').value = sk.name;
      document.getElementById('f-description').value = sk.description;
      document.getElementById('f-body').value = sk.body;
      document.getElementById('f-when').value = sk.when_to_use || '';
      document.getElementById('f-example').value = sk.example || '';
      document.getElementById('f-community').value = sk.community || '';
      document.getElementById('f-tags').value = (sk.tags || []).join(', ');
      document.getElementById('f-depends').value = (sk.depends_on || []).join(', ');
    }
  }
  document.getElementById('modal-bg').classList.add('open');
  document.getElementById('f-name').focus();
}

function closeModal() {
  document.getElementById('modal-bg').classList.remove('open');
  editingId = null;
}

async function saveSkill() {
  const parseList = v => (v || '').split(',').map(s => s.trim()).filter(Boolean);
  const payload = {
    name:        document.getElementById('f-name').value.trim(),
    description: document.getElementById('f-description').value.trim(),
    body:        document.getElementById('f-body').value,
    when_to_use: document.getElementById('f-when').value || null,
    example:     document.getElementById('f-example').value || null,
    community:   document.getElementById('f-community').value || null,
    tags:        parseList(document.getElementById('f-tags').value),
    depends_on:  parseList(document.getElementById('f-depends').value),
  };
  if (!payload.name || !payload.description || !payload.body) {
    document.getElementById('modal-err').textContent = 'name, description and body are required';
    return;
  }
  try {
    if (editingId) {
      await fetchJSON('/api/v1/skills/' + encodeURIComponent(editingId), {
        method:'PUT',
        headers:{'Content-Type':'application/json'},
        body: JSON.stringify(payload),
      });
    } else {
      const created = await fetchJSON('/api/v1/skills', {
        method:'POST',
        headers:{'Content-Type':'application/json'},
        body: JSON.stringify(payload),
      });
      if (created && created.id) selectedId = created.id;
    }
    closeModal();
    await reload();
  } catch (e) {
    document.getElementById('modal-err').textContent = 'save failed: ' + e.message;
  }
}

async function deleteSkill(id) {
  if (!id) return;
  const sk = allSkills.find(s => s.id === id);
  if (!confirm('Delete skill "' + (sk ? sk.name : id) + '"? This cannot be undone.')) return;
  try {
    await fetchJSON('/api/v1/skills/' + encodeURIComponent(id), { method:'DELETE' });
    selectedId = null;
    await reload();
  } catch (e) {
    alert('delete failed: ' + e.message);
  }
}

document.getElementById('add-btn').addEventListener('click', () => openModal(null));
document.getElementById('modal-x').addEventListener('click', closeModal);
document.getElementById('modal-cancel').addEventListener('click', closeModal);
document.getElementById('modal-save').addEventListener('click', saveSkill);
document.getElementById('q').addEventListener('input', () => {
  const sem = document.getElementById('sem-search').checked;
  if (!sem) { lastSemHits = null; render(); return; }
  clearTimeout(semanticDebounce);
  semanticDebounce = setTimeout(runSemanticSearch, 350);
});
document.getElementById('sem-search').addEventListener('change', () => {
  lastSemHits = null;
  if (document.getElementById('sem-search').checked) {
    runSemanticSearch();
  } else {
    render();
  }
});
document.getElementById('filter-community').addEventListener('change', () => { lastSemHits = null; render(); });
document.getElementById('filter-origin').addEventListener('change', () => { lastSemHits = null; render(); });
document.querySelectorAll('.tab').forEach(t => {
  t.addEventListener('click', () => switchTab(t.dataset.tab));
});
document.getElementById('modal-bg').addEventListener('click', e => {
  if (e.target.id === 'modal-bg') closeModal();
});

// ---- import dialog ----
const impRows = {
  'claude-files': ['imp-path-row'],
  'local-dir':    ['imp-path-row'],
  'local-file':   ['imp-path-row'],
  'github-file':  ['imp-url-row'],
  'github-repo':  ['imp-repo-row','imp-subpath-row'],
};
function syncImpRows() {
  const kind = document.getElementById('imp-kind').value;
  ['imp-path-row','imp-url-row','imp-repo-row','imp-subpath-row'].forEach(id => {
    document.getElementById(id).style.display = 'none';
  });
  (impRows[kind] || []).forEach(id => {
    document.getElementById(id).style.display = '';
  });
}
function openImport() {
  document.getElementById('imp-results').innerHTML = '';
  document.getElementById('imp-err').textContent = '';
  document.getElementById('import-bg').classList.add('open');
  syncImpRows();
}
function closeImport() { document.getElementById('import-bg').classList.remove('open'); }
async function runImport() {
  const kind = document.getElementById('imp-kind').value;
  const body = { kind,
    path:      document.getElementById('imp-path').value.trim(),
    url:       document.getElementById('imp-url').value.trim(),
    repo_url:  document.getElementById('imp-repo').value.trim(),
    subpath:   document.getElementById('imp-subpath').value.trim(),
    community: document.getElementById('imp-community').value.trim() || null,
    tags:      document.getElementById('imp-tags').value.split(',').map(s => s.trim()).filter(Boolean),
  };
  document.getElementById('imp-err').textContent = '';
  document.getElementById('imp-results').innerHTML = '<div class="meta">importing…</div>';
  try {
    const resp = await fetchJSON('/api/v1/skills/import', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify(body),
    });
    const rows = (resp.results || []).map(r => {
      const cls = r.status === 'error' ? 'offline' :
                  r.status === 'created' ? 'ok' :
                  r.status === 'updated' ? 'pending' : 'done';
      return '<tr><td><span class="pill ' + cls + '">' + esc(r.status) + '</span></td>' +
             '<td>' + esc(r.name || r.path || '?') + '</td>' +
             '<td class="meta">' + esc(r.error || r.path || '') + '</td></tr>';
    }).join('');
    const summary = Object.entries(resp.summary || {}).map(
      ([k,v]) => '<span class="tag">' + esc(k) + ': ' + v + '</span>'
    ).join(' ');
    document.getElementById('imp-results').innerHTML =
      '<div style="margin-bottom:8px">' + summary + '</div>' +
      '<table><thead><tr><th>status</th><th>name</th><th>note</th></tr></thead><tbody>' + rows + '</tbody></table>';
    await reload();
  } catch (e) {
    document.getElementById('imp-results').innerHTML = '';
    document.getElementById('imp-err').textContent = 'import failed: ' + e.message;
  }
}
document.getElementById('import-btn').addEventListener('click', openImport);
document.getElementById('import-x').addEventListener('click', closeImport);
document.getElementById('imp-close').addEventListener('click', closeImport);
document.getElementById('imp-run').addEventListener('click', runImport);
document.getElementById('imp-kind').addEventListener('change', syncImpRows);
document.getElementById('import-bg').addEventListener('click', e => {
  if (e.target.id === 'import-bg') closeImport();
});

reload();
</script>
</body>
</html>`
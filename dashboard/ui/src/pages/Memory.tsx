// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  memoryApi,
  type MemoryEntity,
  type MemoryInsight,
  type MemoryItem,
  type MemoryRelationship,
  type MemorySource,
} from '../api/memory'

// A unified row shape for the list view — every source flattens to
// this so the row component and search filter don't care about which
// table/file the record came from. Keeping `raw` carries the original
// record for the detail pane to pull source-specific fields out.
type UnifiedRow = {
  key: string
  sourceKey: string
  sourceLabel: string
  title: string
  subtitle: string
  body?: string
  lastModified: string | undefined
  badge: string
  raw:
    | { kind: 'file'; data: MemoryItem }
    | { kind: 'insight'; data: MemoryInsight }
    | { kind: 'entity'; data: MemoryEntity }
    | { kind: 'relationship'; data: MemoryRelationship }
}

// File-backed agents the source picker can target for create/list. The
// keys mirror the {agent} URL segment the Go side accepts. Keeping the
// canonical order here keeps the stat-card row stable across renders.
const FILE_AGENTS = [
  { key: 'claude-code', label: 'Claude Code', kind: 'files' as const },
  { key: 'claude-global', label: 'Claude global', kind: 'file' as const },
  { key: 'claude-projects', label: 'Per-repo CLAUDE.md', kind: 'files' as const },
  { key: 'codex', label: 'Codex', kind: 'files' as const },
  { key: 'gemini', label: 'Gemini', kind: 'file' as const },
  { key: 'opencode', label: 'OpenCode', kind: 'files' as const },
]

const DB_AGENT_KEYS = ['insights', 'entities', 'relationships'] as const

const SOURCE_COLORS: Record<string, string> = {
  'claude-code': 'bg-indigo-500/20 text-indigo-300 border-indigo-500/40',
  'claude-global': 'bg-indigo-500/10 text-indigo-300 border-indigo-500/30',
  'claude-projects': 'bg-violet-500/20 text-violet-300 border-violet-500/40',
  codex: 'bg-sky-500/20 text-sky-300 border-sky-500/40',
  gemini: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
  opencode: 'bg-rose-500/20 text-rose-300 border-rose-500/40',
  insights: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
  entities: 'bg-teal-500/20 text-teal-300 border-teal-500/40',
  relationships: 'bg-cyan-500/20 text-cyan-300 border-cyan-500/40',
}

function badgeClass(key: string): string {
  return SOURCE_COLORS[key] ?? 'bg-zinc-500/20 text-zinc-300 border-zinc-500/40'
}

// Cheap debounce for the search box — 200ms keeps the typing latency
// imperceptible while still skipping the cascading state churn on
// every keystroke for a 500-row dataset.
function useDebounced<T>(value: T, delay = 200): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const id = setTimeout(() => setV(value), delay)
    return () => clearTimeout(id)
  }, [value, delay])
  return v
}

export default function Memory() {
  const [sources, setSources] = useState<MemorySource[]>([])
  const [items, setItems] = useState<UnifiedRow[]>([])
  const [activeFilter, setActiveFilter] = useState<string | null>(null)
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebounced(search, 200)
  const [selected, setSelected] = useState<UnifiedRow | null>(null)
  const [creating, setCreating] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  const refreshAll = useCallback(async () => {
    setLoading(true)
    try {
      const srcs = await memoryApi.listSources()
      setSources(srcs)
      const all: UnifiedRow[] = []
      for (const agent of FILE_AGENTS) {
        try {
          const items = await memoryApi.listAgent(agent.key)
          for (const it of items) {
            all.push({
              key: `${agent.key}:${it.id}`,
              sourceKey: agent.key,
              sourceLabel: agent.label,
              title: it.name || it.path.split('/').pop() || agent.label,
              subtitle: it.description || it.scope || '',
              lastModified: it.last_modified,
              badge: agent.label,
              raw: { kind: 'file', data: it },
            })
          }
        } catch (e) {
          // One failing agent must not wipe the whole list. Log and
          // continue — the source card shows the count discrepancy.
          console.warn(`memory: failed to load ${agent.key}`, e)
        }
      }
      try {
        const insights = await memoryApi.listInsights()
        for (const i of insights) {
          all.push({
            key: `insights:${i.id}`,
            sourceKey: 'insights',
            sourceLabel: 'evo.insights',
            title: i.title,
            subtitle: `${i.domain}${i.tags.length ? ' · ' + i.tags.join(', ') : ''}`,
            body: i.body,
            lastModified: i.created_at,
            badge: 'insights',
            raw: { kind: 'insight', data: i },
          })
        }
      } catch (e) {
        console.warn('memory: failed to load insights', e)
      }
      try {
        const ents = await memoryApi.listEntities()
        for (const e of ents) {
          all.push({
            key: `entities:${e.id}`,
            sourceKey: 'entities',
            sourceLabel: 'entities',
            title: e.name,
            subtitle: e.category ? `${e.category}` : '',
            body: e.description,
            lastModified: e.updated_at,
            badge: 'entities',
            raw: { kind: 'entity', data: e },
          })
        }
      } catch (e) {
        console.warn('memory: failed to load entities', e)
      }
      try {
        const rels = await memoryApi.listRelationships()
        for (const r of rels) {
          all.push({
            key: `relationships:${r.id}`,
            sourceKey: 'relationships',
            sourceLabel: 'relationships',
            title: `${r.subject} —[${r.predicate || '?'}]→ ${r.object}`,
            subtitle: r.description,
            body: r.description,
            lastModified: r.updated_at,
            badge: 'relationships',
            raw: { kind: 'relationship', data: r },
          })
        }
      } catch (e) {
        console.warn('memory: failed to load relationships', e)
      }
      all.sort((a, b) => (b.lastModified ?? '').localeCompare(a.lastModified ?? ''))
      setItems(all)
      setError(null)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Failed to load memory')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    refreshAll()
  }, [refreshAll])

  // Filter pipeline: source filter first (cheap, often selective), then
  // substring match across title/subtitle/body. Materialized via useMemo
  // so typing into the search box doesn't re-sort or re-filter the
  // entire dataset on every render.
  const filtered = useMemo(() => {
    let rows = items
    if (activeFilter) {
      rows = rows.filter(r => r.sourceKey === activeFilter)
    }
    const q = debouncedSearch.trim().toLowerCase()
    if (q) {
      rows = rows.filter(
        r =>
          r.title.toLowerCase().includes(q) ||
          r.subtitle.toLowerCase().includes(q) ||
          (r.body?.toLowerCase().includes(q) ?? false),
      )
    }
    return rows
  }, [items, activeFilter, debouncedSearch])

  const chartData = useMemo(() => {
    const byKey = new Map<string, { label: string; count: number }>()
    for (const s of sources) {
      if (s.count > 0) byKey.set(s.key, { label: s.label, count: s.count })
    }
    return [...byKey.entries()].map(([key, v]) => ({ key, ...v }))
  }, [sources])

  const maxCount = chartData.reduce((m, c) => Math.max(m, c.count), 0)

  const handleSave = async (row: UnifiedRow, patch: UnifiedPatch) => {
    try {
      switch (row.raw.kind) {
        case 'file':
          await memoryApi.updateItem(row.raw.data.agent, row.raw.data.id, {
            body: patch.body,
            description: patch.description,
            type: patch.type,
          })
          break
        case 'insight':
          await memoryApi.updateInsight(row.raw.data.id, {
            title: patch.title,
            body: patch.body,
            domain: patch.domain,
            tags: patch.tags,
          })
          break
        case 'entity':
          await memoryApi.updateEntity(row.raw.data.id, {
            description: patch.body,
            category: patch.category,
          })
          break
        case 'relationship':
          await memoryApi.updateRelationship(row.raw.data.id, {
            description: patch.body,
            predicate: patch.predicate,
            weight: patch.weight,
          })
          break
      }
      setSelected(null)
      await refreshAll()
    } catch (e) {
      alert(`Save failed: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  const handleDelete = async (row: UnifiedRow) => {
    const label = row.title.slice(0, 80)
    // Confirm-on-delete is mandatory per the page spec — the user can
    // accidentally wipe their own memory and there is no soft-delete.
    if (!confirm(`Delete "${label}"?\n\nThis cannot be undone.`)) return
    try {
      switch (row.raw.kind) {
        case 'file':
          await memoryApi.deleteItem(row.raw.data.agent, row.raw.data.id)
          break
        case 'insight':
          await memoryApi.deleteInsight(row.raw.data.id)
          break
        case 'entity':
          await memoryApi.deleteEntity(row.raw.data.id)
          break
        case 'relationship':
          await memoryApi.deleteRelationship(row.raw.data.id)
          break
      }
      setSelected(null)
      await refreshAll()
    } catch (e) {
      alert(`Delete failed: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-white">Memory</h1>
          <p className="text-sm text-zinc-500 mt-1">
            View, search, and edit memory across every agent and store.{' '}
            <a href="/skills" className="text-indigo-400 hover:text-indigo-300 underline">
              See also: Skills
            </a>
          </p>
        </div>
        <button
          type="button"
          onClick={() => setCreating('claude-code')}
          className="px-4 py-2 bg-indigo-600 hover:bg-indigo-500 text-white text-sm font-medium rounded-lg transition-colors"
        >
          New memory
        </button>
      </div>

      {error && (
        <div className="rounded-lg border border-red-900/50 bg-red-950/30 px-4 py-3 text-sm text-red-300">
          {error}
        </div>
      )}

      <StatCardRow
        sources={sources}
        activeFilter={activeFilter}
        onPick={key => setActiveFilter(activeFilter === key ? null : key)}
      />

      <div className="bg-zinc-900 border border-zinc-800 rounded-xl p-5">
        <div className="flex items-center justify-between mb-4">
          <h2 className="text-sm font-medium text-zinc-300">Memory by source</h2>
          <span className="text-xs text-zinc-600">Click a bar to filter the list below</span>
        </div>
        {chartData.length === 0 ? (
          <div className="text-zinc-600 text-sm">No memory yet.</div>
        ) : (
          <div className="space-y-2">
            {chartData.map(d => {
              const widthPct = maxCount > 0 ? Math.max(2, (d.count / maxCount) * 100) : 0
              const isActive = activeFilter === d.key
              return (
                <button
                  key={d.key}
                  type="button"
                  onClick={() => setActiveFilter(isActive ? null : d.key)}
                  className={`w-full flex items-center gap-3 text-left transition-opacity ${
                    isActive ? 'opacity-100' : 'opacity-90 hover:opacity-100'
                  }`}
                >
                  <span className="w-44 text-xs text-zinc-400 truncate">{d.label}</span>
                  <span className="flex-1 h-5 rounded bg-zinc-800/70 overflow-hidden">
                    <span
                      className={`block h-full rounded ${
                        isActive ? 'bg-indigo-500' : 'bg-indigo-500/60'
                      }`}
                      style={{ width: `${widthPct}%` }}
                    />
                  </span>
                  <span className="w-12 text-xs text-zinc-400 text-right tabular-nums">{d.count}</span>
                </button>
              )
            })}
          </div>
        )}
      </div>

      <div className="flex items-center gap-3">
        <input
          type="search"
          placeholder="Search across all memory…"
          value={search}
          onChange={e => setSearch(e.target.value)}
          className="flex-1 bg-zinc-900 border border-zinc-800 rounded-lg px-3 py-2 text-sm text-zinc-100 placeholder:text-zinc-600 focus:outline-none focus:ring-2 focus:ring-indigo-500/30 focus:border-indigo-500"
        />
        {activeFilter && (
          <button
            type="button"
            onClick={() => setActiveFilter(null)}
            className="px-3 py-2 text-xs text-zinc-300 bg-zinc-800 hover:bg-zinc-700 rounded-md"
          >
            Clear filter
          </button>
        )}
      </div>

      {loading && items.length === 0 ? (
        <div className="text-zinc-500 text-sm">Loading…</div>
      ) : filtered.length === 0 ? (
        <div className="border border-dashed border-zinc-700 rounded-xl p-12 text-center text-zinc-500 text-sm">
          No memory matches your search.
        </div>
      ) : (
        <div className="border border-zinc-800 rounded-xl divide-y divide-zinc-800 overflow-hidden">
          {filtered.slice(0, 500).map(row => (
            <button
              key={row.key}
              type="button"
              onClick={() => setSelected(row)}
              className="w-full text-left px-4 py-3 hover:bg-zinc-900/60 transition-colors flex items-start gap-3"
            >
              <span
                className={`mt-0.5 inline-block px-2 py-0.5 rounded border text-[10px] font-medium uppercase tracking-wide ${badgeClass(row.sourceKey)}`}
              >
                {row.sourceLabel}
              </span>
              <span className="flex-1 min-w-0">
                <span className="block text-sm text-white truncate">{row.title}</span>
                {row.subtitle && (
                  <span className="block text-xs text-zinc-500 truncate mt-0.5">{row.subtitle}</span>
                )}
              </span>
              {row.lastModified && (
                <span className="text-xs text-zinc-600 whitespace-nowrap mt-0.5">
                  {new Date(row.lastModified).toLocaleDateString()}
                </span>
              )}
            </button>
          ))}
          {filtered.length > 500 && (
            <div className="px-4 py-3 text-xs text-zinc-600 bg-zinc-950">
              Showing first 500 of {filtered.length} matches. Refine your search to narrow.
            </div>
          )}
        </div>
      )}

      {selected && (
        <DetailModal
          row={selected}
          onClose={() => setSelected(null)}
          onSave={patch => handleSave(selected, patch)}
          onDelete={() => handleDelete(selected)}
        />
      )}

      {creating && (
        <CreateModal
          defaultSource={creating}
          onClose={() => setCreating(null)}
          onCreated={async () => {
            setCreating(null)
            await refreshAll()
          }}
        />
      )}
    </div>
  )
}

// ---- presentational subcomponents ----

function StatCardRow({
  sources,
  activeFilter,
  onPick,
}: {
  sources: MemorySource[]
  activeFilter: string | null
  onPick: (key: string) => void
}) {
  return (
    <div className="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-3">
      {sources.map(s => {
        const isActive = activeFilter === s.key
        return (
          <button
            key={s.key}
            type="button"
            onClick={() => onPick(s.key)}
            className={`text-left rounded-xl border px-4 py-3 transition-colors ${
              isActive
                ? 'border-indigo-500 bg-indigo-500/10'
                : 'border-zinc-800 bg-zinc-900 hover:bg-zinc-900/80'
            }`}
          >
            <div className="text-[10px] uppercase tracking-wide text-zinc-500">{s.kind}</div>
            <div className="text-sm font-medium text-zinc-200 truncate">{s.label}</div>
            <div className="mt-1 flex items-baseline gap-1.5">
              <span className="text-2xl font-semibold text-white tabular-nums">{Math.max(0, s.count)}</span>
              {!s.available && (
                <span className="text-[10px] text-amber-400">unavailable</span>
              )}
            </div>
            {s.note && <div className="text-[10px] text-zinc-600 mt-0.5 truncate">{s.note}</div>}
          </button>
        )
      })}
    </div>
  )
}

// UnifiedPatch is the unified edit payload. Each field is optional;
// only the ones the source uses get sent on save.
type UnifiedPatch = {
  title?: string
  body?: string
  description?: string
  type?: string
  domain?: string
  tags?: string[]
  category?: string
  predicate?: string
  weight?: number
}

function DetailModal({
  row,
  onClose,
  onSave,
  onDelete,
}: {
  row: UnifiedRow
  onClose: () => void
  onSave: (patch: UnifiedPatch) => void
  onDelete: () => void
}) {
  // Pre-populate the editor fields from the row. Filling them as
  // controlled inputs (instead of editing a single Markdown blob) means
  // the user can edit structured metadata without learning frontmatter.
  const [body, setBody] = useState(() => row.body ?? initialBody(row))
  const [description, setDescription] = useState(() => initialDescription(row))
  const [type, setType] = useState(() => initialType(row))
  const [domain, setDomain] = useState(() => (row.raw.kind === 'insight' ? row.raw.data.domain : ''))
  const [title, setTitle] = useState(() => row.title)
  const [tags, setTags] = useState(() => (row.raw.kind === 'insight' ? row.raw.data.tags.join(', ') : ''))
  const [category, setCategory] = useState(() => (row.raw.kind === 'entity' ? row.raw.data.category : ''))
  const [predicate, setPredicate] = useState(() =>
    row.raw.kind === 'relationship' ? row.raw.data.predicate : '',
  )
  const [weight, setWeight] = useState(() =>
    row.raw.kind === 'relationship' ? String(row.raw.data.weight) : '0',
  )
  const [showRaw, setShowRaw] = useState(false)
  const [loadingBody, setLoadingBody] = useState(false)

  // File-backed rows arrive in the list without their body (we keep
  // the listing cheap). Fetch the full body on open so the editor
  // shows the same content the user would write back. Errors fall
  // back to whatever we already have.
  useEffect(() => {
    if (row.raw.kind !== 'file' || row.body) return
    let cancelled = false
    setLoadingBody(true)
    memoryApi
      .getItem(row.raw.data.agent, row.raw.data.id)
      .then(full => {
        if (cancelled) return
        setBody(full.body ?? '')
        if (full.description) setDescription(full.description)
        if (full.type) setType(full.type)
      })
      .catch(() => {
        // Leave body empty; the user can still write a fresh body and save.
      })
      .finally(() => {
        if (!cancelled) setLoadingBody(false)
      })
    return () => {
      cancelled = true
    }
  }, [row])

  const buildPatch = (): UnifiedPatch => {
    switch (row.raw.kind) {
      case 'file':
        return { body, description, type }
      case 'insight':
        return {
          title,
          body,
          domain,
          tags: tags
            .split(',')
            .map(t => t.trim())
            .filter(Boolean),
        }
      case 'entity':
        return { body, category }
      case 'relationship': {
        const w = parseFloat(weight)
        return { body, predicate, weight: Number.isFinite(w) ? w : 0 }
      }
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 bg-black/70 backdrop-blur-sm flex items-center justify-center p-4"
      onClick={onClose}
    >
      <div
        onClick={e => e.stopPropagation()}
        className="w-full max-w-3xl max-h-[85vh] bg-zinc-950 border border-zinc-800 rounded-xl overflow-hidden flex flex-col"
      >
        <div className="px-5 py-4 border-b border-zinc-800 flex items-center justify-between">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <span
                className={`px-2 py-0.5 rounded border text-[10px] font-medium uppercase tracking-wide ${badgeClass(row.sourceKey)}`}
              >
                {row.sourceLabel}
              </span>
              <h3 className="text-white font-semibold truncate">{title || row.title}</h3>
            </div>
            {row.raw.kind === 'file' && (
              <p className="text-xs text-zinc-600 mt-1 truncate">{row.raw.data.path}</p>
            )}
          </div>
          <button
            type="button"
            onClick={onClose}
            className="text-zinc-500 hover:text-zinc-200 px-2"
            aria-label="Close"
          >
            ×
          </button>
        </div>

        <div className="px-5 py-4 overflow-y-auto space-y-4">
          {row.raw.kind === 'insight' && (
            <>
              <Field label="Title">
                <input
                  type="text"
                  value={title}
                  onChange={e => setTitle(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              <Field label="Domain">
                <input
                  type="text"
                  value={domain}
                  onChange={e => setDomain(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              <Field label="Tags (comma-separated)">
                <input
                  type="text"
                  value={tags}
                  onChange={e => setTags(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
            </>
          )}

          {row.raw.kind === 'file' && (
            <>
              <Field label="Description">
                <input
                  type="text"
                  value={description}
                  onChange={e => setDescription(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              {row.sourceKey === 'claude-code' && (
                <Field label="Type">
                  <select
                    value={type}
                    onChange={e => setType(e.target.value)}
                    className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                  >
                    <option value="">(unset)</option>
                    <option value="user">user</option>
                    <option value="feedback">feedback</option>
                    <option value="project">project</option>
                    <option value="reference">reference</option>
                  </select>
                </Field>
              )}
            </>
          )}

          {row.raw.kind === 'entity' && (
            <>
              <Field label="Name (read-only — managed by extract pipeline)">
                <input
                  type="text"
                  value={row.raw.data.name}
                  readOnly
                  className="w-full bg-zinc-900/60 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-400"
                />
              </Field>
              <Field label="Category">
                <input
                  type="text"
                  value={category}
                  onChange={e => setCategory(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
            </>
          )}

          {row.raw.kind === 'relationship' && (
            <>
              <Field label="Triple (read-only)">
                <input
                  type="text"
                  value={`${row.raw.data.subject} —[${row.raw.data.predicate}]→ ${row.raw.data.object}`}
                  readOnly
                  className="w-full bg-zinc-900/60 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-400"
                />
              </Field>
              <Field label="Predicate">
                <input
                  type="text"
                  value={predicate}
                  onChange={e => setPredicate(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              <Field label="Weight">
                <input
                  type="number"
                  step="0.01"
                  value={weight}
                  onChange={e => setWeight(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              <p className="text-[11px] text-zinc-600 -mt-2">
                Subject + object are managed by the extract pipeline; edit them by re-running extraction.
              </p>
            </>
          )}

          <Field
            label={
              row.raw.kind === 'entity' || row.raw.kind === 'relationship'
                ? 'Description'
                : 'Body (Markdown)'
            }
          >
            <textarea
              value={body}
              onChange={e => setBody(e.target.value)}
              rows={14}
              className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100 font-mono"
              placeholder={loadingBody ? 'Loading body…' : ''}
            />
          </Field>

          {row.raw.kind === 'file' && (
            <button
              type="button"
              onClick={() => setShowRaw(v => !v)}
              className="text-xs text-zinc-500 hover:text-zinc-300"
            >
              {showRaw ? 'Hide' : 'Show'} raw frontmatter info
            </button>
          )}
          {showRaw && row.raw.kind === 'file' && (
            <pre className="text-[11px] text-zinc-500 bg-zinc-900 border border-zinc-800 rounded p-2 overflow-x-auto">
              {JSON.stringify(
                {
                  agent: row.raw.data.agent,
                  scope: row.raw.data.scope,
                  has_frontmatter: row.raw.data.has_matter,
                  path: row.raw.data.path,
                },
                null,
                2,
              )}
            </pre>
          )}
        </div>

        <div className="px-5 py-3 border-t border-zinc-800 flex items-center justify-between gap-2">
          <button
            type="button"
            onClick={onDelete}
            className="px-3 py-1.5 text-sm rounded-md bg-red-950/40 text-red-300 hover:bg-red-900/40 border border-red-900/40"
          >
            Delete
          </button>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onClose}
              className="px-3 py-1.5 text-sm rounded-md text-zinc-300 hover:bg-zinc-800"
            >
              Cancel
            </button>
            <button
              type="button"
              onClick={() => onSave(buildPatch())}
              className="px-4 py-1.5 text-sm rounded-md bg-indigo-600 text-white hover:bg-indigo-500"
            >
              Save
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}

function initialBody(row: UnifiedRow): string {
  switch (row.raw.kind) {
    case 'file':
      return row.raw.data.body ?? ''
    case 'insight':
      return row.raw.data.body
    case 'entity':
      return row.raw.data.description
    case 'relationship':
      return row.raw.data.description
  }
}

function initialDescription(row: UnifiedRow): string {
  if (row.raw.kind === 'file') return row.raw.data.description ?? ''
  return ''
}

function initialType(row: UnifiedRow): string {
  if (row.raw.kind === 'file') return row.raw.data.type ?? ''
  return ''
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block">
      <span className="block text-[11px] text-zinc-500 uppercase tracking-wide mb-1">{label}</span>
      {children}
    </label>
  )
}

function CreateModal({
  defaultSource,
  onClose,
  onCreated,
}: {
  defaultSource: string
  onClose: () => void
  onCreated: () => void
}) {
  const [source, setSource] = useState(defaultSource)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [type, setType] = useState('project')
  const [scope, setScope] = useState('')
  const [body, setBody] = useState('')
  const [domain, setDomain] = useState('')
  const [title, setTitle] = useState('')
  const [tags, setTags] = useState('')
  const [saving, setSaving] = useState(false)

  const isFile = FILE_AGENTS.some(a => a.key === source)
  const isReadOnly = ['claude-global', 'gemini', 'claude-projects'].includes(source)

  const submit = async () => {
    setSaving(true)
    try {
      if (source === 'insights') {
        if (!domain.trim() || !title.trim() || !body.trim()) {
          alert('domain, title, and body are required')
          return
        }
        await memoryApi.createInsight({
          domain: domain.trim(),
          title: title.trim(),
          body,
          tags: tags
            .split(',')
            .map(t => t.trim())
            .filter(Boolean),
        })
      } else if (isFile) {
        if (!name.trim() || !body.trim()) {
          alert('name and body are required')
          return
        }
        await memoryApi.createItem(source, {
          name: name.trim(),
          description: description.trim(),
          type: type.trim(),
          scope: scope.trim(),
          body,
        })
      } else {
        alert(`Creating ${source} via the dashboard is not supported. Use the upstream pipeline.`)
        return
      }
      onCreated()
    } catch (e) {
      alert(`Create failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      className="fixed inset-0 z-50 bg-black/70 backdrop-blur-sm flex items-center justify-center p-4"
      onClick={onClose}
    >
      <div
        onClick={e => e.stopPropagation()}
        className="w-full max-w-2xl max-h-[85vh] bg-zinc-950 border border-zinc-800 rounded-xl overflow-hidden flex flex-col"
      >
        <div className="px-5 py-4 border-b border-zinc-800 flex items-center justify-between">
          <h3 className="text-white font-semibold">New memory</h3>
          <button type="button" onClick={onClose} className="text-zinc-500 hover:text-zinc-200 px-2" aria-label="Close">
            ×
          </button>
        </div>

        <div className="px-5 py-4 overflow-y-auto space-y-4">
          <Field label="Destination">
            <select
              value={source}
              onChange={e => setSource(e.target.value)}
              className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
            >
              <optgroup label="Coding agents">
                {FILE_AGENTS.map(a => (
                  <option key={a.key} value={a.key}>
                    {a.label}
                  </option>
                ))}
              </optgroup>
              <optgroup label="Databases">
                {DB_AGENT_KEYS.map(k => (
                  <option key={k} value={k}>
                    {k}
                  </option>
                ))}
              </optgroup>
            </select>
          </Field>

          {isReadOnly && (
            <div className="rounded-lg border border-amber-900/40 bg-amber-950/20 px-3 py-2 text-xs text-amber-300">
              {source === 'claude-projects'
                ? 'Per-repo CLAUDE.md files cannot be created from the dashboard. Add the file directly under the repo root and it will appear here.'
                : 'Single-file memory cannot be created — edit the existing file instead.'}
            </div>
          )}

          {source === 'insights' && (
            <>
              <Field label="Domain">
                <input
                  type="text"
                  value={domain}
                  onChange={e => setDomain(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                  placeholder="e.g. gemm-rocm"
                />
              </Field>
              <Field label="Title">
                <input
                  type="text"
                  value={title}
                  onChange={e => setTitle(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              <Field label="Tags (comma-separated)">
                <input
                  type="text"
                  value={tags}
                  onChange={e => setTags(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
            </>
          )}

          {(source === 'entities' || source === 'relationships') && (
            <div className="rounded-lg border border-amber-900/40 bg-amber-950/20 px-3 py-2 text-xs text-amber-300">
              Entities and relationships are populated by the extract pipeline. The dashboard
              supports editing and deleting them, but new rows should land via re-extraction so
              they round-trip to chunk references correctly.
            </div>
          )}

          {isFile && !isReadOnly && (
            <>
              <Field label="Name">
                <input
                  type="text"
                  value={name}
                  onChange={e => setName(e.target.value)}
                  placeholder="short kebab-slug name"
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              <Field label="Description (one line)">
                <input
                  type="text"
                  value={description}
                  onChange={e => setDescription(e.target.value)}
                  className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                />
              </Field>
              {source === 'claude-code' && (
                <>
                  <Field label="Project scope (slug)">
                    <input
                      type="text"
                      value={scope}
                      onChange={e => setScope(e.target.value)}
                      placeholder="-thearray-git-evo"
                      className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                    />
                  </Field>
                  <Field label="Type">
                    <select
                      value={type}
                      onChange={e => setType(e.target.value)}
                      className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100"
                    >
                      <option value="user">user</option>
                      <option value="feedback">feedback</option>
                      <option value="project">project</option>
                      <option value="reference">reference</option>
                    </select>
                  </Field>
                </>
              )}
            </>
          )}

          {(isFile || source === 'insights') && (
            <Field label="Body (Markdown)">
              <textarea
                value={body}
                onChange={e => setBody(e.target.value)}
                rows={14}
                className="w-full bg-zinc-900 border border-zinc-800 rounded px-2 py-1.5 text-sm text-zinc-100 font-mono"
              />
            </Field>
          )}
        </div>

        <div className="px-5 py-3 border-t border-zinc-800 flex items-center justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            className="px-3 py-1.5 text-sm rounded-md text-zinc-300 hover:bg-zinc-800"
          >
            Cancel
          </button>
          <button
            type="button"
            disabled={saving || isReadOnly || source === 'entities' || source === 'relationships'}
            onClick={submit}
            className="px-4 py-1.5 text-sm rounded-md bg-indigo-600 text-white hover:bg-indigo-500 disabled:opacity-50 disabled:cursor-not-allowed"
          >
            {saving ? 'Saving…' : 'Create'}
          </button>
        </div>
      </div>
    </div>
  )
}
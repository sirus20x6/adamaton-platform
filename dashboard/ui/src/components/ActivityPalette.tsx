// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useEffect, useState, useMemo } from 'react'
import { api, type ActivityMeta } from '../api/client'

const categoryMeta: Record<string, { label: string; icon: string }> = {
  data:         { label: 'Data Fetching',    icon: '📥' },
  security:     { label: 'Security',         icon: '🔒' },
  cpp:          { label: 'C/C++',            icon: '⚙️' },
  build:        { label: 'Build & CI',       icon: '🔨' },
  performance:  { label: 'Performance',      icon: '⚡' },
  quality:      { label: 'Code Quality',     icon: '✨' },
  testing:      { label: 'Testing',          icon: '🧪' },
  architecture: { label: 'Architecture',     icon: '🏗️' },
  compliance:   { label: 'Compliance',       icon: '📋' },
  go:           { label: 'Go',               icon: '🐹' },
  python:       { label: 'Python',           icon: '🐍' },
  jsts:         { label: 'JS / TypeScript',  icon: '🟨' },
  rust:         { label: 'Rust',             icon: '🦀' },
  devops:       { label: 'DevOps & Infra',   icon: '☁️' },
  docs:         { label: 'Documentation',    icon: '📖' },
  a11y:         { label: 'Accessibility',    icon: '♿' },
  role:         { label: 'Role-Based Gates', icon: '🛡️' },
  resource:     { label: 'Agent Resources',  icon: '🤖' },
  action:       { label: 'Actions',          icon: '🎯' },
}

const categoryOrder = [
  'data', 'security', 'cpp', 'build', 'performance', 'quality',
  'testing', 'architecture', 'compliance', 'go', 'python', 'jsts',
  'rust', 'devops', 'docs', 'a11y', 'role', 'resource', 'action',
]

export default function ActivityPalette() {
  const [activities, setActivities] = useState<ActivityMeta[]>([])
  const [search, setSearch] = useState('')
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    const ctrl = new AbortController()
    let cancelled = false
    api.listActivities({ signal: ctrl.signal })
      .then(data => {
        if (cancelled) return
        setActivities(data)
        setLoadError(null)
      })
      .catch(e => {
        if (cancelled) return
        if (e instanceof Error && e.name === 'AbortError') return
        setLoadError(e instanceof Error ? e.message : 'Failed to load activities')
      })
    return () => {
      cancelled = true
      ctrl.abort()
    }
  }, [])

  const filtered = useMemo(() => {
    if (!search) return activities
    const q = search.toLowerCase()
    return activities.filter(
      a => a.name.toLowerCase().includes(q) || a.description.toLowerCase().includes(q) || a.category.toLowerCase().includes(q)
    )
  }, [activities, search])

  const grouped = useMemo(() => {
    const map: Record<string, ActivityMeta[]> = {}
    for (const a of filtered) {
      const bucket = map[a.category] ?? (map[a.category] = [])
      bucket.push(a)
    }
    return map
  }, [filtered])

  const toggle = (cat: string) => {
    setCollapsed(prev => {
      const next = new Set(prev)
      if (next.has(cat)) {
        next.delete(cat)
      } else {
        next.add(cat)
      }
      return next
    })
  }

  return (
    <div className="w-60 bg-zinc-900 border-r border-zinc-800 overflow-y-auto flex-shrink-0 flex flex-col">
      <div className="p-3 pb-2 sticky top-0 bg-zinc-900 z-10 border-b border-zinc-800">
        <div className="text-xs font-medium text-zinc-500 uppercase tracking-wider mb-2">Activities</div>
        <input
          type="text"
          value={search}
          onChange={e => setSearch(e.target.value)}
          placeholder="Search..."
          className="w-full bg-zinc-800 border border-zinc-700 rounded-md px-2.5 py-1.5 text-xs text-zinc-200 placeholder:text-zinc-600 focus:outline-none focus:border-indigo-500 transition-colors"
        />
        <div className="text-[10px] text-zinc-400 mt-1">{filtered.length} activities</div>
      </div>

      <div className="flex-1 overflow-y-auto p-3 pt-1">
        {loadError && (
          <div className="mb-3 px-2 py-2 bg-red-500/10 border border-red-500/30 rounded text-[10px] text-red-300">
            Failed to load activities: {loadError}
          </div>
        )}
        {!loadError && activities.length === 0 && (
          <div className="mb-3 px-2 py-2 text-[10px] text-zinc-500 text-center">
            Loading activities...
          </div>
        )}
        {categoryOrder.map(cat => {
          const items = grouped[cat]
          if (!items || items.length === 0) return null
          const meta = categoryMeta[cat] || { label: cat, icon: '📦' }
          const isCollapsed = collapsed.has(cat)

          return (
            <div key={cat} className="mb-2">
              <button
                onClick={() => toggle(cat)}
                className="w-full flex items-center gap-1.5 px-1 py-1 text-left group"
              >
                <span className="text-xs">{meta.icon}</span>
                <span className="text-[10px] font-semibold text-zinc-400 uppercase tracking-wider flex-1 group-hover:text-zinc-300 transition-colors">
                  {meta.label}
                </span>
                <span className="text-zinc-400 text-[10px]">{items.length}</span>
                <span className={`text-zinc-400 text-[10px] transition-transform ${isCollapsed ? '' : 'rotate-90'}`}>
                  ▶
                </span>
              </button>

              {!isCollapsed && (
                <div className="space-y-0.5 ml-1">
                  {items.map(activity => (
                    <div
                      key={activity.name}
                      draggable
                      onDragStart={e => {
                        e.dataTransfer.setData('application/json', JSON.stringify(activity))
                        e.dataTransfer.effectAllowed = 'move'
                      }}
                      className="px-2 py-1.5 bg-zinc-800/40 hover:bg-zinc-800 rounded cursor-grab active:cursor-grabbing transition-colors group"
                      title={activity.description}
                    >
                      <div className="text-[11px] font-medium text-zinc-300 group-hover:text-white transition-colors truncate">
                        {activity.name.replace('Activity', '').replace('Check', '')}
                      </div>
                      <div className="text-[9px] text-zinc-600 truncate leading-tight">{activity.description}</div>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )
        })}

        {/* Flow control nodes */}
        <div className="mb-2 mt-3 pt-3 border-t border-zinc-800">
          <div className="flex items-center gap-1.5 px-1 py-1">
            <span className="text-xs">🔀</span>
            <span className="text-[10px] font-semibold text-zinc-400 uppercase tracking-wider">Flow Control</span>
          </div>
          <div className="space-y-0.5 ml-1">
            <div
              draggable
              onDragStart={e => {
                e.dataTransfer.setData('application/json', JSON.stringify({ name: '__parallel', category: 'flow' }))
                e.dataTransfer.effectAllowed = 'move'
              }}
              className="px-2 py-1.5 bg-zinc-800/40 hover:bg-zinc-800 rounded cursor-grab active:cursor-grabbing transition-colors"
            >
              <div className="text-[11px] font-medium text-zinc-300">Parallel</div>
              <div className="text-[9px] text-zinc-600">Run branches concurrently</div>
            </div>
            <div
              draggable
              onDragStart={e => {
                e.dataTransfer.setData('application/json', JSON.stringify({ name: '__condition', category: 'flow' }))
                e.dataTransfer.effectAllowed = 'move'
              }}
              className="px-2 py-1.5 bg-zinc-800/40 hover:bg-zinc-800 rounded cursor-grab active:cursor-grabbing transition-colors"
            >
              <div className="text-[11px] font-medium text-zinc-300">Condition</div>
              <div className="text-[9px] text-zinc-600">Branch on expression</div>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}
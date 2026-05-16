// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { api, type WorkflowRun } from '../api/client'

const statusColors: Record<string, string> = {
  running: 'bg-blue-500/20 text-blue-400',
  completed: 'bg-emerald-500/20 text-emerald-400',
  failed: 'bg-red-500/20 text-red-400',
  cancelled: 'bg-zinc-500/20 text-zinc-400',
}

export default function Runs() {
  const [runs, setRuns] = useState<WorkflowRun[]>([])
  const [loading, setLoading] = useState(true)
  // Surface fetch failures rather than rendering "no runs" indistinguishably
  // from a 500.
  const [error, setError] = useState<string | null>(null)
  const [searchParams] = useSearchParams()
  const definitionId = searchParams.get('definition_id') || undefined
  const highlightId = searchParams.get('highlight') || undefined

  // Single in-flight controller for both interval-driven and manual refreshes.
  // Holding only the most recent controller bounds memory regardless of how
  // long the page is left open.
  const currentRef = useRef<AbortController | null>(null)

  useEffect(() => {
    let cancelled = false

    const refresh = () => {
      currentRef.current?.abort()
      const ctrl = new AbortController()
      currentRef.current = ctrl
      api.listRuns(definitionId, { signal: ctrl.signal })
        .then(r => { if (!cancelled) { setRuns(r); setError(null) } })
        .catch(e => {
          if (cancelled) return
          if (e instanceof Error && e.name === 'AbortError') return
          setError(e instanceof Error ? e.message : 'Failed to load runs')
        })
        .finally(() => { if (!cancelled) setLoading(false) })
    }

    refresh()
    // Skip polling while the tab is hidden — every tab the user has parked on
    // /runs would otherwise hammer the API at 12 req/min indefinitely. We still
    // run one immediate refresh on visibilitychange so the user sees current
    // state when they tab back in.
    const tick = () => {
      if (document.visibilityState === 'visible') {
        refresh()
      }
    }
    const interval = setInterval(tick, 5000)
    const onVisibility = () => {
      if (document.visibilityState === 'visible') refresh()
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      cancelled = true
      clearInterval(interval)
      document.removeEventListener('visibilitychange', onVisibility)
      currentRef.current?.abort()
    }
  }, [definitionId])

  const handleManualRefresh = () => {
    currentRef.current?.abort()
    const ctrl = new AbortController()
    currentRef.current = ctrl
    api.listRuns(definitionId, { signal: ctrl.signal })
      .then(r => { setRuns(r); setError(null) })
      .catch(e => {
        if (e instanceof Error && e.name === 'AbortError') return
        setError(e instanceof Error ? e.message : 'Failed to load runs')
      })
      .finally(() => setLoading(false))
  }

  const title = useMemo(() => {
    if (definitionId) {
      return 'Workflow Runs'
    }
    return 'Runs'
  }, [definitionId])

  return (
    <div>
      <div className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-semibold text-white">{title}</h1>
          <p className="text-sm text-zinc-500 mt-1">Workflow execution history</p>
        </div>
        <button
          onClick={handleManualRefresh}
          className="px-3 py-1.5 bg-zinc-800 text-zinc-300 hover:bg-zinc-700 text-sm rounded-md transition-colors"
        >
          Refresh
        </button>
      </div>

      {error && (
        <div className="mb-4 bg-red-500/10 border border-red-500/40 text-red-300 rounded-lg px-4 py-3 text-sm">
          Failed to load runs: {error}
        </div>
      )}

      {loading ? (
        <div className="text-zinc-400 text-sm">Loading...</div>
      ) : runs.length === 0 && !error ? (
        <div className="border border-dashed border-zinc-700 rounded-xl p-12 text-center">
          <p className="text-zinc-500">
            {definitionId ? 'No runs yet for this workflow.' : 'No runs yet. Trigger a workflow to see results here.'}
          </p>
        </div>
      ) : (
        <div className="overflow-hidden rounded-xl border border-zinc-800">
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-zinc-900 text-zinc-400 text-left">
                <th className="px-4 py-3 font-medium">Status</th>
                <th className="px-4 py-3 font-medium">Temporal ID</th>
                <th className="px-4 py-3 font-medium">Started</th>
                <th className="px-4 py-3 font-medium">Duration</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-zinc-800">
              {runs.map(run => {
                const started = new Date(run.started_at)
                const finished = run.finished_at ? new Date(run.finished_at) : null
                const duration = finished
                  ? `${((finished.getTime() - started.getTime()) / 1000).toFixed(1)}s`
                  : 'running...'

                return (
                  <tr
                    key={run.id}
                    className={`bg-zinc-950 hover:bg-zinc-900/50 transition-colors ${
                      highlightId === run.id ? 'ring-1 ring-emerald-500/60 bg-emerald-500/10' : ''
                    }`}
                  >
                    <td className="px-4 py-3">
                      <span className={`inline-block px-2 py-0.5 rounded-full text-xs font-medium ${statusColors[run.status] || ''}`}>
                        {run.status}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-zinc-300 font-mono text-xs">{run.temporal_id}</td>
                    <td className="px-4 py-3 text-zinc-400">{started.toLocaleString()}</td>
                    <td className="px-4 py-3 text-zinc-400">{duration}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
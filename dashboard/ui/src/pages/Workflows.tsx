// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useEffect, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { api, type WorkflowDefinition } from '../api/client'

export default function Workflows() {
  const [defs, setDefs] = useState<WorkflowDefinition[]>([])
  const [loading, setLoading] = useState(true)
  const [runningId, setRunningId] = useState<string | null>(null)
  // Surface fetch failures to the user instead of silently rendering "no
  // workflows yet" — the empty-state was indistinguishable from a 500 or
  // network error and made debugging harder.
  const [error, setError] = useState<string | null>(null)
  const navigate = useNavigate()

  useEffect(() => {
    let cancelled = false
    const ctrl = new AbortController()
    api.listDefinitions({ signal: ctrl.signal })
      .then(d => { if (!cancelled) { setDefs(d); setError(null) } })
      .catch(e => {
        if (cancelled) return
        if (e instanceof Error && e.name === 'AbortError') return
        setError(e instanceof Error ? e.message : 'Failed to load workflows')
      })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => {
      cancelled = true
      ctrl.abort()
    }
  }, [])

  const handleDelete = async (id: string) => {
    if (!confirm('Delete this workflow?')) return
    try {
      await api.deleteDefinition(id)
      setDefs(d => d.filter(x => x.id !== id))
    } catch (e: unknown) {
      alert(`Failed to delete: ${e instanceof Error ? e.message : 'unknown error'}`)
    }
  }

  const handleRun = async (id: string) => {
    setRunningId(id)
    try {
      const result = await api.runDefinition(id)
      navigate(`/runs?definition_id=${id}&highlight=${result.run.id}`)
    } catch (e: unknown) {
      alert(`Failed to start: ${e instanceof Error ? e.message : 'unknown error'}`)
    } finally {
      setRunningId(null)
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-semibold text-white">Workflows</h1>
          <p className="text-sm text-zinc-500 mt-1">Build and manage Temporal workflow definitions</p>
        </div>
        <Link
          to="/builder"
          className="px-4 py-2 bg-indigo-600 hover:bg-indigo-500 text-white text-sm font-medium rounded-lg transition-colors"
        >
          New Workflow
        </Link>
      </div>

      {error && (
        <div className="mb-4 bg-red-500/10 border border-red-500/40 text-red-300 rounded-lg px-4 py-3 text-sm">
          Failed to load workflows: {error}
        </div>
      )}

      {loading ? (
        <div className="text-zinc-400 text-sm">Loading...</div>
      ) : defs.length === 0 && !error ? (
        <div className="border border-dashed border-zinc-700 rounded-xl p-12 text-center">
          <p className="text-zinc-500 mb-4">No workflows yet</p>
          <Link
            to="/builder"
            className="text-indigo-400 hover:text-indigo-300 text-sm font-medium"
          >
            Create your first workflow
          </Link>
        </div>
      ) : (
        <div className="grid gap-4">
          {defs.map(def => (
            <div key={def.id} className="bg-zinc-900 border border-zinc-800 rounded-xl p-5 flex items-center justify-between">
              <div className="min-w-0 flex-1">
                <h3 className="text-white font-medium truncate">{def.name}</h3>
                {def.description && (
                  <p className="text-zinc-500 text-sm mt-0.5 truncate">{def.description}</p>
                )}
                <p className="text-zinc-600 text-xs mt-1">
                  Updated {new Date(def.updated_at).toLocaleDateString()}
                </p>
              </div>
              <div className="flex items-center gap-2 ml-4">
                <button
                  onClick={() => handleRun(def.id)}
                  disabled={runningId === def.id}
                  className="px-3 py-1.5 bg-emerald-600/20 text-emerald-400 hover:bg-emerald-600/30 disabled:opacity-50 text-sm rounded-md transition-colors"
                >
                  {runningId === def.id ? 'Running...' : 'Run'}
                </button>
                <Link
                  to={`/builder/${def.id}`}
                  className="px-3 py-1.5 bg-zinc-800 text-zinc-300 hover:bg-zinc-700 text-sm rounded-md transition-colors"
                >
                  Edit
                </Link>
                <button
                  onClick={() => handleDelete(def.id)}
                  className="px-3 py-1.5 bg-zinc-800 text-zinc-500 hover:bg-red-900/30 hover:text-red-400 text-sm rounded-md transition-colors"
                >
                  Delete
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useEffect, useState } from 'react'
import { delegatorApi, type AgentUsage, type DelegatorTask } from '../api/delegator'
import { HttpError } from '../api/client'
import AgentCard from '../components/delegator/AgentCard'
import StatsBar from '../components/delegator/StatsBar'
import TaskTable from '../components/delegator/TaskTable'

const AGENT_ORDER = ['claude', 'codex', 'gemini', 'opencode']

// Phase 3 polls /api/delegator/quota and /api/delegator/tasks every 30s.
// SSE wiring is deferred until Phase 2 (Temporal-backed tasks); polling is
// fine for a single-user dashboard.
const POLL_INTERVAL_MS = 30_000

export default function Delegator() {
  const [agents, setAgents] = useState<AgentUsage[]>([])
  const [tasks, setTasks] = useState<DelegatorTask[]>([])
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false

    const refresh = async () => {
      try {
        const [quotaRes, taskRes] = await Promise.all([
          delegatorApi.getQuota(1),
          delegatorApi.listTasks(),
        ])
        if (cancelled) return
        const sorted = [...quotaRes.agents].sort((a, b) => {
          const ai = AGENT_ORDER.indexOf(a.agent)
          const bi = AGENT_ORDER.indexOf(b.agent)
          return (ai === -1 ? 99 : ai) - (bi === -1 ? 99 : bi)
        })
        setAgents(sorted)
        setTasks(taskRes ?? [])
        setError(null)
      } catch (err) {
        if (cancelled) return
        if (err instanceof HttpError) {
          setError(`API ${err.status}: ${err.message}`)
        } else if (err instanceof Error) {
          setError(err.message)
        } else {
          setError(String(err))
        }
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    refresh()
    const interval = setInterval(refresh, POLL_INTERVAL_MS)
    return () => {
      cancelled = true
      clearInterval(interval)
    }
  }, [])

  return (
    <div className="space-y-6">
      <StatsBar agents={agents} tasks={tasks} />

      {error && (
        <div className="rounded-lg border border-red-900/50 bg-red-950/30 px-4 py-3 text-sm text-red-300">
          {error}
        </div>
      )}

      {loading && agents.length === 0 ? (
        <div className="text-zinc-500 text-sm">Loading…</div>
      ) : (
        <>
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
            {agents.map(a => (
              <AgentCard key={a.agent} agent={a} />
            ))}
          </div>
          <TaskTable tasks={tasks} />
        </>
      )}
    </div>
  )
}
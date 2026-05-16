// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import type { AgentUsage, DelegatorTask } from '../../api/delegator'

interface Props {
  agents: AgentUsage[]
  tasks: DelegatorTask[]
}

function formatTokens(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(n)
}

// Strip across the top of the Delegator page. Same four metrics the
// delegator's StatsBar.svelte showed.
export default function StatsBar({ agents, tasks }: Props) {
  const totalSessions = agents.reduce((sum, a) => sum + (a.sessions || 0), 0)
  const totalTokens = agents.reduce((sum, a) => sum + (a.inputTokens || 0) + (a.outputTokens || 0), 0)
  const activeTasks = tasks.filter(t => t.status === 'running' || t.status === 'pending').length
  const agentsUp = agents.length

  return (
    <div className="flex items-center justify-between bg-zinc-900 border border-zinc-800 rounded-lg px-6 py-3">
      <h2 className="text-base font-semibold text-white tracking-wide">Delegator</h2>
      <div className="flex items-center gap-8">
        <Metric label="Sessions" value={String(totalSessions)} color="text-emerald-400" />
        <Metric label="Tokens" value={formatTokens(totalTokens)} color="text-cyan-400" />
        <Metric label="Active" value={String(activeTasks)} color="text-blue-400" />
        <Metric label="Agents" value={String(agentsUp)} color="text-purple-400" />
      </div>
    </div>
  )
}

function Metric({ label, value, color }: { label: string; value: string; color: string }) {
  return (
    <div className="text-center">
      <div className="text-xs text-zinc-400 uppercase tracking-wider">{label}</div>
      <div className={`text-lg font-mono ${color}`}>{value}</div>
    </div>
  )
}
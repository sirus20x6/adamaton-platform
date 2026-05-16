// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import type { AgentUsage } from '../../api/delegator'

function barColor(util: number): string {
  if (util >= 0.8) return 'bg-red-500'
  if (util >= 0.5) return 'bg-yellow-500'
  return 'bg-green-500'
}

function pct(v: number | undefined): string {
  if (v === undefined || v === null) return '—'
  return `${Math.round(v * 100)}%`
}

interface Props {
  agent: AgentUsage
}

// Render quota bars for a single agent. Local agents (opencode) get the
// "unlimited" pill; Gemini's bar is daily (matches what the OAuth quota
// API surfaces). Everything else gets twin 5h/7d bars driven by the
// CCSAVER rate-limit headers.
export default function QuotaGauges({ agent }: Props) {
  const isUnlimited = agent.apiType === 'local'
  const isDaily = agent.agent === 'gemini'

  if (isUnlimited) {
    return (
      <div className="flex items-center justify-between text-sm">
        <span className="text-zinc-400">Quota</span>
        <span className="text-purple-400 font-mono">∞ Unlimited</span>
      </div>
    )
  }

  if (isDaily) {
    return (
      <div>
        <div className="flex items-center justify-between text-sm mb-1">
          <span className="text-zinc-400">Daily</span>
          <span className="text-zinc-300 font-mono">{pct(agent.utilization5h)}</span>
        </div>
        <div className="w-full bg-zinc-800 rounded-full h-2">
          <div
            className={`h-2 rounded-full transition-all duration-500 ${barColor(agent.utilization5h ?? 0)}`}
            style={{ width: `${Math.min((agent.utilization5h ?? 0) * 100, 100)}%` }}
          />
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-2">
      <div>
        <div className="flex items-center justify-between text-sm mb-1">
          <span className="text-zinc-400">5h</span>
          <span className="text-zinc-300 font-mono">{pct(agent.utilization5h)}</span>
        </div>
        <div className="w-full bg-zinc-800 rounded-full h-2">
          <div
            className={`h-2 rounded-full transition-all duration-500 ${barColor(agent.utilization5h ?? 0)}`}
            style={{ width: `${Math.min((agent.utilization5h ?? 0) * 100, 100)}%` }}
          />
        </div>
      </div>
      <div>
        <div className="flex items-center justify-between text-sm mb-1">
          <span className="text-zinc-400">7d</span>
          <span className="text-zinc-300 font-mono">{pct(agent.utilization7d)}</span>
        </div>
        <div className="w-full bg-zinc-800 rounded-full h-2">
          <div
            className={`h-2 rounded-full transition-all duration-500 ${barColor(agent.utilization7d ?? 0)}`}
            style={{ width: `${Math.min((agent.utilization7d ?? 0) * 100, 100)}%` }}
          />
        </div>
      </div>
    </div>
  )
}
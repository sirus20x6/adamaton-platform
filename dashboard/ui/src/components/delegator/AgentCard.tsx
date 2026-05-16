// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import type { AgentUsage } from '../../api/delegator'
import QuotaGauges from './QuotaGauges'

const labels: Record<string, string> = {
  claude: 'Claude',
  codex: 'Codex',
  gemini: 'Gemini',
  opencode: 'OpenCode',
}

// Top border accent color per agent. Matches the delegator's old palette
// roughly; tuned to read on the gogents zinc-950 background.
const borders: Record<string, string> = {
  claude: 'border-orange-500',
  codex: 'border-blue-500',
  gemini: 'border-cyan-500',
  opencode: 'border-emerald-500',
}

function formatTokens(n: number | undefined): string {
  if (!n) return '0'
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(n)
}

interface Props {
  agent: AgentUsage
}

export default function AgentCard({ agent }: Props) {
  const label = labels[agent.agent] ?? agent.agent
  const border = borders[agent.agent] ?? 'border-zinc-600'
  return (
    <div className={`bg-zinc-900 rounded-lg border-t-2 ${border} border border-zinc-800 p-4 min-w-[200px]`}>
      <div className="flex items-center justify-between mb-3">
        <h3 className="font-semibold text-white">{label}</h3>
        <span className="text-xs text-zinc-500 font-mono">{agent.model || agent.apiType}</span>
      </div>
      <QuotaGauges agent={agent} />
      <div className="mt-3 pt-3 border-t border-zinc-800 grid grid-cols-3 gap-2 text-sm">
        <div>
          <div className="text-zinc-400 text-xs">Sessions</div>
          <div className="text-zinc-300 font-mono">{agent.sessions ?? 0}</div>
        </div>
        <div>
          <div className="text-zinc-400 text-xs">In</div>
          <div className="text-blue-300 font-mono">{formatTokens(agent.inputTokens)}</div>
        </div>
        <div>
          <div className="text-zinc-400 text-xs">Out</div>
          <div className="text-emerald-300 font-mono">{formatTokens(agent.outputTokens)}</div>
        </div>
      </div>
    </div>
  )
}
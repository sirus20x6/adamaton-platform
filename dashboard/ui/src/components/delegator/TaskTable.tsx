// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import type { DelegatorTask } from '../../api/delegator'

const statusColor: Record<DelegatorTask['status'], string> = {
  pending: 'text-yellow-400',
  running: 'text-blue-400',
  completed: 'text-emerald-400',
  failed: 'text-red-400',
  cancelled: 'text-zinc-500',
  timed_out: 'text-orange-400',
}

const statusIcon: Record<DelegatorTask['status'], string> = {
  pending: '⏳',
  running: '⚡',
  completed: '✓',
  failed: '✗',
  cancelled: '⊘',
  timed_out: '⏱',
}

function elapsed(t: DelegatorTask): string {
  const s = t.elapsed_seconds || 0
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  return `${m}m ${s % 60}s`
}

interface Props {
  tasks: DelegatorTask[]
}

export default function TaskTable({ tasks }: Props) {
  return (
    <div className="bg-zinc-900 rounded-lg border border-zinc-800 p-4">
      <h3 className="text-white font-semibold mb-3">Recent Tasks</h3>
      {tasks.length === 0 ? (
        <p className="text-zinc-500 text-sm">No tasks delegated yet. Use the <code className="font-mono text-zinc-400">delegate_task</code> MCP tool to spawn one.</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-zinc-400 text-left border-b border-zinc-800">
                <th className="pb-2 pr-4 font-medium">ID</th>
                <th className="pb-2 pr-4 font-medium">Agent</th>
                <th className="pb-2 pr-4 font-medium">Diff/Prio</th>
                <th className="pb-2 pr-4 font-medium">Status</th>
                <th className="pb-2 pr-4 font-medium">Time</th>
                <th className="pb-2 font-medium">Prompt</th>
              </tr>
            </thead>
            <tbody>
              {tasks.map(t => (
                <tr key={t.id} className="border-b border-zinc-800/50 hover:bg-zinc-800/30">
                  <td className="py-2 pr-4 font-mono text-zinc-300">{t.id.slice(-8)}</td>
                  <td className="py-2 pr-4 text-zinc-300">{t.agent}</td>
                  <td className="py-2 pr-4 text-zinc-400 text-xs">
                    {t.difficulty || '—'} / {t.priority || 'normal'}
                  </td>
                  <td className={`py-2 pr-4 ${statusColor[t.status] ?? 'text-zinc-300'}`}>
                    <span className="mr-1">{statusIcon[t.status] ?? ''}</span>
                    {t.status}
                  </td>
                  <td className="py-2 pr-4 font-mono text-zinc-400">{elapsed(t)}</td>
                  <td className="py-2 text-zinc-300 truncate max-w-[400px]">{t.prompt_preview}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
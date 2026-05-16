// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

function narrowConditionData(raw: unknown): { expression: string } {
  const d = (raw && typeof raw === 'object' ? raw : {}) as { expression?: unknown }
  return { expression: typeof d.expression === 'string' ? d.expression : '' }
}

function ConditionNode({ data, selected }: NodeProps) {
  const d = narrowConditionData(data)

  return (
    <div
      className={`rounded-lg border border-orange-500/30 bg-orange-500/10 px-4 py-3 min-w-[160px] shadow-lg backdrop-blur-sm transition-all ${
        selected ? 'ring-2 ring-indigo-500 ring-offset-1 ring-offset-zinc-950' : ''
      }`}
    >
      <Handle type="target" position={Position.Top} className="!bg-zinc-400 !w-2.5 !h-2.5 !border-zinc-700" />
      <div className="flex items-center gap-2">
        <span className="text-base">🔀</span>
        <div className="text-sm font-medium text-zinc-200">Condition</div>
      </div>
      <div className="text-[10px] text-zinc-500 mt-0.5 font-mono truncate max-w-[160px]">
        {d.expression || 'Set expression...'}
      </div>
      <Handle
        type="source"
        position={Position.Bottom}
        id="true"
        style={{ left: '30%' }}
        className="!bg-emerald-400 !w-2.5 !h-2.5 !border-zinc-700"
      />
      <Handle
        type="source"
        position={Position.Bottom}
        id="false"
        style={{ left: '70%' }}
        className="!bg-red-400 !w-2.5 !h-2.5 !border-zinc-700"
      />
    </div>
  )
}

export default memo(ConditionNode)
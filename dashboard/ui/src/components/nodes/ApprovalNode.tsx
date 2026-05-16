// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { memo } from 'react'
import { Handle, Position, type NodeProps } from '@xyflow/react'

interface ApprovalNodeData {
  label?: string
  roleLabel?: string
}

function narrowApprovalData(raw: unknown): Required<ApprovalNodeData> {
  const d = (raw && typeof raw === 'object' ? raw : {}) as ApprovalNodeData
  return {
    label: typeof d.label === 'string' ? d.label : '',
    roleLabel: typeof d.roleLabel === 'string' ? d.roleLabel : '',
  }
}

function ApprovalNode({ data, selected }: NodeProps) {
  const d = narrowApprovalData(data)

  return (
    <div
      className={`rounded-lg border border-rose-500/30 bg-rose-500/10 px-4 py-3 min-w-[180px] max-w-[220px] shadow-lg backdrop-blur-sm transition-all ${
        selected ? 'ring-2 ring-indigo-500 ring-offset-1 ring-offset-zinc-950' : ''
      }`}
    >
      <Handle type="target" position={Position.Top} className="!bg-zinc-400 !w-2.5 !h-2.5 !border-zinc-700" />
      <div className="flex items-center gap-2">
        <span className="text-base flex-shrink-0">🛡️</span>
        <div className="min-w-0">
          <div className="text-sm font-medium text-zinc-200 truncate">{d.label}</div>
          {d.roleLabel ? (
            <div className="text-[10px] text-rose-400/80 truncate">{d.roleLabel}</div>
          ) : (
            <div className="text-[10px] text-zinc-500 truncate">Select a role...</div>
          )}
        </div>
      </div>
      {/* Approved path */}
      <Handle
        type="source"
        position={Position.Bottom}
        id="approved"
        style={{ left: '30%' }}
        className="!bg-emerald-400 !w-2.5 !h-2.5 !border-zinc-700"
      />
      {/* Rejected path */}
      <Handle
        type="source"
        position={Position.Bottom}
        id="rejected"
        style={{ left: '70%' }}
        className="!bg-red-400 !w-2.5 !h-2.5 !border-zinc-700"
      />
    </div>
  )
}

export default memo(ApprovalNode)
// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { memo } from 'react'
import { Handle, Position, type NodeProps, NodeResizer } from '@xyflow/react'

function ParallelNode({ selected }: NodeProps) {
  return (
    <div
      className={`rounded-xl border-2 border-dashed border-purple-500/40 bg-purple-500/5 min-w-[200px] min-h-[120px] w-full h-full transition-all ${
        selected ? 'border-purple-400/70' : ''
      }`}
    >
      <NodeResizer
        minWidth={200}
        minHeight={120}
        isVisible={selected}
        lineClassName="!border-purple-500/50"
        handleClassName="!bg-purple-500 !w-2 !h-2 !border-0"
      />
      {/*
       * Handle topology:
       *   Top    : external "in"   (target)  — visible
       *   Left   : internal "fan-out" (source) — hidden, used for synthetic
       *            edges to children. Distinct position avoids React Flow
       *            duplicate-position warnings and keeps the visible Top/Bottom
       *            handles unambiguous for normal connect interactions.
       *   Right  : internal "fan-in"  (target)  — hidden, paired with fan-out.
       *   Bottom : external "out"  (source) — visible
       */}
      <Handle type="target" position={Position.Top} id="in" className="!bg-purple-400 !w-3 !h-3 !border-zinc-700 !border-2" />
      <Handle type="source" position={Position.Left} id="fan-out" className="!bg-transparent !w-3 !h-3 !border-0 !min-w-0 !min-h-0 pointer-events-none" />

      <div className="px-3 py-1.5 flex items-center gap-2">
        <span className="text-sm">🔀</span>
        <span className="text-xs font-semibold text-purple-300/80 uppercase tracking-wider">Parallel</span>
        <span className="text-[9px] text-purple-400/50 ml-auto">runs concurrently</span>
      </div>

      <Handle type="target" position={Position.Right} id="fan-in" className="!bg-transparent !w-3 !h-3 !border-0 !min-w-0 !min-h-0 pointer-events-none" />
      <Handle type="source" position={Position.Bottom} id="out" className="!bg-purple-400 !w-3 !h-3 !border-zinc-700 !border-2" />
    </div>
  )
}

export default memo(ParallelNode)
// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { memo } from 'react'
import { type NodeProps } from '@xyflow/react'
import NodeHandles, { type HandleConfig } from './NodeHandles'

const categoryColors: Record<string, { bg: string; border: string; icon: string }> = {
  data:         { bg: 'bg-sky-500/10',     border: 'border-sky-500/30',     icon: '📥' },
  security:     { bg: 'bg-red-500/10',     border: 'border-red-500/30',     icon: '🔒' },
  cpp:          { bg: 'bg-blue-500/10',    border: 'border-blue-500/30',    icon: '⚙️' },
  build:        { bg: 'bg-orange-500/10',  border: 'border-orange-500/30',  icon: '🔨' },
  performance:  { bg: 'bg-yellow-500/10',  border: 'border-yellow-500/30',  icon: '⚡' },
  quality:      { bg: 'bg-violet-500/10',  border: 'border-violet-500/30',  icon: '✨' },
  testing:      { bg: 'bg-teal-500/10',    border: 'border-teal-500/30',    icon: '🧪' },
  architecture: { bg: 'bg-indigo-500/10',  border: 'border-indigo-500/30',  icon: '🏗️' },
  compliance:   { bg: 'bg-pink-500/10',    border: 'border-pink-500/30',    icon: '📋' },
  go:           { bg: 'bg-cyan-500/10',    border: 'border-cyan-500/30',    icon: '🐹' },
  python:       { bg: 'bg-green-500/10',   border: 'border-green-500/30',   icon: '🐍' },
  jsts:         { bg: 'bg-amber-500/10',   border: 'border-amber-500/30',   icon: '🟨' },
  rust:         { bg: 'bg-orange-600/10',  border: 'border-orange-600/30',  icon: '🦀' },
  devops:       { bg: 'bg-slate-500/10',   border: 'border-slate-500/30',   icon: '☁️' },
  docs:         { bg: 'bg-lime-500/10',    border: 'border-lime-500/30',    icon: '📖' },
  a11y:         { bg: 'bg-fuchsia-500/10', border: 'border-fuchsia-500/30', icon: '♿' },
  role:         { bg: 'bg-rose-500/10',    border: 'border-rose-500/30',    icon: '🛡️' },
  resource:     { bg: 'bg-cyan-600/10',    border: 'border-cyan-600/30',    icon: '🤖' },
  action:       { bg: 'bg-emerald-500/10', border: 'border-emerald-500/30', icon: '🎯' },
}

interface ActivityNodeData {
  label?: string
  activityName?: string
  category?: string
  description?: string
  icon?: string
  handleConfig?: HandleConfig
}

// narrowActivityData defaults missing fields so an unexpected payload never
// crashes the renderer with a TypeError.
function narrowActivityData(raw: unknown): Required<Pick<ActivityNodeData, 'label' | 'activityName' | 'category' | 'description'>> & ActivityNodeData {
  const d = (raw && typeof raw === 'object' ? raw : {}) as ActivityNodeData
  return {
    label: typeof d.label === 'string' ? d.label : '',
    activityName: typeof d.activityName === 'string' ? d.activityName : '',
    category: typeof d.category === 'string' ? d.category : '',
    description: typeof d.description === 'string' ? d.description : '',
    icon: typeof d.icon === 'string' ? d.icon : undefined,
    handleConfig: d.handleConfig,
  }
}

function ActivityNode({ data, selected }: NodeProps) {
  const d = narrowActivityData(data)
  const colors = categoryColors[d.category] || { bg: 'bg-zinc-500/10', border: 'border-zinc-500/30', icon: '📦' }
  const icon = d.icon || colors.icon

  return (
    <div
      className={`rounded-lg border ${colors.border} ${colors.bg} px-4 py-3 min-w-[180px] max-w-[220px] shadow-lg backdrop-blur-sm transition-all ${
        selected ? 'ring-2 ring-indigo-500 ring-offset-1 ring-offset-zinc-950' : ''
      }`}
    >
      <NodeHandles config={d.handleConfig} />
      <div className="flex items-center gap-2">
        <span className="text-base flex-shrink-0">{icon}</span>
        <div className="min-w-0">
          <div className="text-sm font-medium text-zinc-200 truncate">{d.label}</div>
          <div className="text-[10px] text-zinc-500 truncate">{d.activityName}</div>
        </div>
      </div>
    </div>
  )
}

export default memo(ActivityNode)
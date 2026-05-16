// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { Handle, Position } from '@xyflow/react'

export interface HandleConfig {
  inputs: string[]   // Position names: "top", "bottom", "left", "right"
  outputs: string[]
}

const positionMap: Record<string, Position> = {
  top: Position.Top,
  bottom: Position.Bottom,
  left: Position.Left,
  right: Position.Right,
}

interface Props {
  config?: HandleConfig
  inputClass?: string
  outputClass?: string
}

const defaultConfig: HandleConfig = {
  inputs: ['top'],
  outputs: ['bottom'],
}

// dedupe preserves first occurrence; React Flow rejects multiple handles with
// the same id, so we silently drop later duplicates from a config rather than
// crash-render.
function dedupe(items: string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const it of items) {
    if (seen.has(it)) continue
    seen.add(it)
    out.push(it)
  }
  return out
}

export default function NodeHandles({ config = defaultConfig, inputClass, outputClass }: Props) {
  const handleClass = '!bg-zinc-400 !w-2.5 !h-2.5 !border-zinc-700'

  // Handle id format compatibility note: when a node has only one handle of a
  // given type we emit `id={undefined}` (default handle) so existing edges
  // saved without a sourceHandle/targetHandle keep rendering. When a node
  // gains a second handle, ids switch to `in-${pos}-${i}` / `out-${pos}-${i}`.
  // Saved edges that referenced the legacy "default" handle still resolve to
  // the first generated handle on load because React Flow falls back to it
  // when the named handle isn't found. Builder.tsx's edge persistence does
  // not include sourceHandle/targetHandle for non-parallel nodes, so this
  // change is forward-compatible. If you change the id format, update
  // Builder.tsx's parallel fan-out edges too — they hard-code 'fan-out' /
  // 'fan-in' / 'out' / 'in' which match ParallelNode.tsx, not NodeHandles.

  const inputs = dedupe(config.inputs)
  const outputs = dedupe(config.outputs)

  return (
    <>
      {inputs.map((pos, i) => (
        <Handle
          key={`in-${pos}-${i}`}
          type="target"
          position={positionMap[pos] || Position.Top}
          // Handle ids must be unique per node. Including the index guarantees
          // uniqueness even if the same position appears more than once
          // (defensive — dedupe above should already handle it).
          id={inputs.length > 1 ? `in-${pos}-${i}` : undefined}
          className={inputClass || handleClass}
        />
      ))}
      {outputs.map((pos, i) => (
        <Handle
          key={`out-${pos}-${i}`}
          type="source"
          position={positionMap[pos] || Position.Bottom}
          id={outputs.length > 1 ? `out-${pos}-${i}` : undefined}
          className={outputClass || handleClass}
        />
      ))}
    </>
  )
}

export function HandlePositionMenu({ config, onChange }: { config: HandleConfig; onChange: (config: HandleConfig) => void }) {
  const positions = ['top', 'bottom', 'left', 'right']

  return (
    <div className="space-y-2">
      <div>
        <div className="text-[10px] text-zinc-500 mb-1">Input handles</div>
        <div className="flex gap-1">
          {positions.map(pos => (
            <button
              key={pos}
              onClick={() => onChange({ ...config, inputs: config.inputs.includes(pos) ? config.inputs.filter(p => p !== pos) : [...config.inputs, pos] })}
              className={`px-1.5 py-0.5 text-[9px] rounded transition-colors ${
                config.inputs.includes(pos) ? 'bg-indigo-600 text-white' : 'bg-zinc-800 text-zinc-500'
              }`}
            >
              {pos}
            </button>
          ))}
        </div>
      </div>
      <div>
        <div className="text-[10px] text-zinc-500 mb-1">Output handles</div>
        <div className="flex gap-1">
          {positions.map(pos => (
            <button
              key={pos}
              onClick={() => onChange({ ...config, outputs: config.outputs.includes(pos) ? config.outputs.filter(p => p !== pos) : [...config.outputs, pos] })}
              className={`px-1.5 py-0.5 text-[9px] rounded transition-colors ${
                config.outputs.includes(pos) ? 'bg-emerald-600 text-white' : 'bg-zinc-800 text-zinc-500'
              }`}
            >
              {pos}
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}
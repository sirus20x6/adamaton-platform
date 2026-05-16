// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useCallback, useEffect, useId, useState } from 'react'
import type { Node } from '@xyflow/react'
import { api, type Role, type RoleGroup } from '../api/client'
import ConditionalField from './properties/ConditionalField'
import PromptEditor from './properties/PromptEditor'
import { HandlePositionMenu } from './nodes/NodeHandles'

interface Props {
  node: Node | null
  onUpdate: (id: string, data: Record<string, unknown>) => void
  onDelete: (id: string) => void
}

type PropertyValueMap = Record<string, unknown>
type ExecutorData = { type?: string; agent?: string; function?: string }
type PropertyPanelData = Record<string, unknown> & {
  label?: string
  activityName?: string
  category?: string
  description?: string
  role?: string
  roleLabel?: string
  expression?: string
  trueBranch?: string
  falseBranch?: string
  prompt?: string
  source?: string
  executor?: ExecutorData
  properties?: import('./properties/DynamicField').NodeProperty[]
  inputSchema?: import('./properties/DynamicField').NodeProperty[]
  propertyValues?: PropertyValueMap
  inputMapping?: Record<string, string>
  config?: Record<string, unknown>
  handleConfig?: import('./nodes/NodeHandles').HandleConfig
}

export default function PropertyPanel({ node, onUpdate, onDelete }: Props) {
  const [roles, setRoles] = useState<Role[]>([])
  const [roleGroups, setRoleGroups] = useState<RoleGroup[]>([])
  const [activeTab, setActiveTab] = useState<'properties' | 'prompt' | 'advanced'>('properties')

  const roleId = useId()
  const expressionId = useId()
  const trueBranchId = useId()
  const falseBranchId = useId()
  const timeoutId = useId()
  const retryMaxId = useId()
  const retryInitialId = useId()
  const retryBackoffId = useId()

  useEffect(() => {
    const ctrl = new AbortController()
    api.listRoles({ signal: ctrl.signal }).then(data => {
      setRoles(data.roles)
      setRoleGroups(data.groups)
    }).catch(() => {})
    return () => ctrl.abort()
  }, [])

  if (!node) {
    return (
      <div className="w-64 bg-zinc-900 border-l border-zinc-800 p-4 flex-shrink-0">
        <div className="text-xs text-zinc-600 text-center mt-8">Select a node to edit</div>
      </div>
    )
  }

  const data = node.data as PropertyPanelData
  const isActivity = node.type === 'activity' || node.type === 'approval'
  const isCondition = node.type === 'condition'
  const isRoleBased = node.type === 'approval' || (node.type === 'activity' && data.category === 'role')
  const hasPrompt = isActivity && data.executor?.type === 'agent_prompt'
  const properties = data.properties || data.inputSchema || []

  // Memoize the three update closures so children that take them as props
  // don't re-render every time PropertyPanel re-renders for unrelated reasons
  // (e.g. a sibling node selection changing). The deps only change when the
  // selected node, its data, or the parent callback genuinely changes.
  const update = useCallback(<K extends keyof PropertyPanelData>(key: K, value: PropertyPanelData[K]) => {
    onUpdate(node.id, { ...data, [key]: value })
  }, [node.id, data, onUpdate])
  const updateConfig = useCallback((key: string, value: unknown) => {
    onUpdate(node.id, { ...data, config: { ...(data.config || {}), [key]: value } })
  }, [node.id, data, onUpdate])
  const updatePropValue = useCallback((propName: string, value: unknown) => {
    const propValues = { ...(data.propertyValues || {}), [propName]: value }
    onUpdate(node.id, { ...data, propertyValues: propValues })
  }, [node.id, data, onUpdate])

  const tabs = [
    { id: 'properties' as const, label: 'Properties' },
    ...(hasPrompt ? [{ id: 'prompt' as const, label: 'Prompt' }] : []),
    ...(isActivity ? [{ id: 'advanced' as const, label: 'Advanced' }] : []),
  ]
  const currentTab = tabs.some(tab => tab.id === activeTab) ? activeTab : 'properties'
  const configValue = (key: string, fallback: string | number) => {
    const value = data.config?.[key]
    return typeof value === 'string' || typeof value === 'number' ? value : fallback
  }

  return (
    <div className="w-64 bg-zinc-900 border-l border-zinc-800 overflow-y-auto flex-shrink-0 flex flex-col">
      {/* Header */}
      <div className="p-3 pb-0 flex-shrink-0">
        <div className="flex items-center justify-between mb-1">
          <div className="text-[10px] font-medium text-zinc-500 uppercase tracking-wider">Node</div>
          <button onClick={() => onDelete(node.id)} className="text-[10px] text-zinc-600 hover:text-red-400 transition-colors">Delete</button>
        </div>
        <div className="text-sm font-medium text-white truncate">{data.label || data.activityName || node.type}</div>
        {data.description && <div className="text-[10px] text-zinc-500 mt-0.5 line-clamp-2">{data.description}</div>}

        {/* Tabs */}
        {tabs.length > 1 && (
          <div className="flex gap-0.5 mt-2 -mx-1">
            {tabs.map(tab => (
              <button
                key={tab.id}
                onClick={() => setActiveTab(tab.id)}
                className={`px-2 py-1 text-[10px] rounded transition-colors ${
                  currentTab === tab.id ? 'bg-zinc-800 text-white' : 'text-zinc-500 hover:text-zinc-300'
                }`}
              >
                {tab.label}
              </button>
            ))}
          </div>
        )}
      </div>

      {/* Tab Content */}
      <div className="flex-1 overflow-y-auto p-3 pt-2">
        {currentTab === 'properties' && (
          <div className="space-y-3">
            {/* Role selector for role-based activities */}
            {isRoleBased && (
              <div>
                <label htmlFor={roleId} className="block text-[10px] text-zinc-500 mb-0.5">Assigned Role</label>
                <select
                  id={roleId}
                  value={data.role || ''}
                  onChange={e => {
                    const role = roles.find(r => r.id === e.target.value)
                    update('role', e.target.value)
                    update('roleLabel', role?.label || '')
                  }}
                  className="field-input"
                >
                  <option value="">Select role...</option>
                  {roleGroups.map(g => (
                    <optgroup key={g.id} label={`${g.icon} ${g.label}`}>
                      {roles.filter(r => r.group === g.id).map(r => (
                        <option key={r.id} value={r.id}>{r.label}</option>
                      ))}
                    </optgroup>
                  ))}
                </select>
              </div>
            )}

            {/* Dynamic properties with conditional visibility */}
            {properties.map(prop => (
              <ConditionalField
                key={prop.name}
                property={prop}
                value={(data.propertyValues || {})[prop.name] ?? (data.inputMapping || {})[prop.name]}
                onChange={v => updatePropValue(prop.name, v)}
                formValues={data.propertyValues || {}}
              />
            ))}

            {/* Input mapping section for wiring between nodes */}
            {isActivity && properties.length > 0 && (
              <details className="mt-3">
                <summary className="text-[10px] font-medium text-zinc-500 uppercase tracking-wider cursor-pointer hover:text-zinc-400">
                  Input Mapping
                </summary>
                <div className="mt-2 space-y-2">
                  {properties.map(field => (
                    <InputMappingRow
                      key={field.name}
                      fieldName={field.name}
                      displayName={field.displayName || field.name}
                      value={data.inputMapping?.[field.name] || ''}
                      onChange={val => {
                        const mapping = { ...(data.inputMapping || {}), [field.name]: val }
                        onUpdate(node.id, { ...data, inputMapping: mapping })
                      }}
                    />
                  ))}
                </div>
              </details>
            )}

            {/* Condition fields */}
            {isCondition && (
              <div className="space-y-2">
                <div>
                  <label htmlFor={expressionId} className="block text-[10px] text-zinc-500 mb-0.5">Expression</label>
                  <input
                    id={expressionId}
                    type="text"
                    value={data.expression || ''}
                    onChange={e => update('expression', e.target.value)}
                    placeholder="e.g. node-1.verdict == PASS"
                    className="field-input font-mono"
                  />
                </div>
                <div>
                  <label htmlFor={trueBranchId} className="block text-[10px] text-zinc-500 mb-0.5">True Branch</label>
                  <input id={trueBranchId} type="text" value={data.trueBranch || ''} onChange={e => update('trueBranch', e.target.value)} placeholder="node ID" className="field-input" />
                </div>
                <div>
                  <label htmlFor={falseBranchId} className="block text-[10px] text-zinc-500 mb-0.5">False Branch</label>
                  <input id={falseBranchId} type="text" value={data.falseBranch || ''} onChange={e => update('falseBranch', e.target.value)} placeholder="node ID" className="field-input" />
                </div>
              </div>
            )}
          </div>
        )}

        {currentTab === 'prompt' && hasPrompt && (
          <PromptEditor
            value={data.prompt || ''}
            onChange={v => update('prompt', v)}
            inputFields={properties.map(p => p.name)}
          />
        )}

        {currentTab === 'advanced' && isActivity && (
          <div className="space-y-2">
            <div className="text-[10px] font-medium text-zinc-500 uppercase tracking-wider mb-2">Timeout & Retry</div>
            <div>
              <label htmlFor={timeoutId} className="block text-[10px] text-zinc-500 mb-0.5">Timeout</label>
              <input id={timeoutId} type="text" value={configValue('timeout', '3m')} onChange={e => updateConfig('timeout', e.target.value)} placeholder="e.g. 3m, 48h" className="field-input" />
            </div>
            <div>
              <label htmlFor={retryMaxId} className="block text-[10px] text-zinc-500 mb-0.5">Max Retries</label>
              <input
                id={retryMaxId}
                type="number"
                value={configValue('retryMax', 3)}
                onChange={e => {
                  const v = e.target.value
                  if (v === '') { updateConfig('retryMax', 0); return }
                  const n = parseInt(v, 10)
                  if (!isNaN(n)) updateConfig('retryMax', n)
                }}
                className="field-input"
              />
            </div>
            <div>
              <label htmlFor={retryInitialId} className="block text-[10px] text-zinc-500 mb-0.5">Retry Interval</label>
              <input id={retryInitialId} type="text" value={configValue('retryInitial', '5s')} onChange={e => updateConfig('retryInitial', e.target.value)} className="field-input" />
            </div>
            <div>
              <label htmlFor={retryBackoffId} className="block text-[10px] text-zinc-500 mb-0.5">Backoff Multiplier</label>
              <input
                id={retryBackoffId}
                type="number"
                value={configValue('retryBackoff', 2.0)}
                onChange={e => {
                  const v = e.target.value
                  if (v === '') { updateConfig('retryBackoff', 2.0); return }
                  const n = Number(v)
                  if (!isNaN(n)) updateConfig('retryBackoff', n)
                }}
                step="0.1"
                className="field-input"
              />
            </div>

            <div className="text-[10px] font-medium text-zinc-500 uppercase tracking-wider mt-4 mb-2">Executor</div>
            <div className="text-[10px] text-zinc-400">
              <span className="text-zinc-500">Type:</span> {data.executor?.type || 'agent_prompt'}
            </div>
            {data.executor?.agent && (
              <div className="text-[10px] text-zinc-400">
                <span className="text-zinc-500">Agent:</span> {data.executor.agent}
              </div>
            )}
            {data.executor?.function && (
              <div className="text-[10px] text-zinc-400">
                <span className="text-zinc-500">Function:</span> {data.executor.function}
              </div>
            )}
            <div className="text-[10px] text-zinc-400">
              <span className="text-zinc-500">Source:</span> {data.source || 'builtin'}
            </div>

            <div className="text-[10px] font-medium text-zinc-500 uppercase tracking-wider mt-4 mb-2">Handle Positions</div>
            <HandlePositionMenu
              config={data.handleConfig || { inputs: ['top'], outputs: ['bottom'] }}
              onChange={cfg => update('handleConfig', cfg)}
            />
          </div>
        )}
      </div>
    </div>
  )
}

function InputMappingRow({
  fieldName,
  displayName,
  value,
  onChange,
}: {
  fieldName: string
  displayName: string
  value: string
  onChange: (value: string) => void
}) {
  const id = useId()
  return (
    <div>
      <label htmlFor={id} className="block text-[9px] text-zinc-600 mb-0.5">{displayName}</label>
      <input
        id={id}
        type="text"
        value={value}
        onChange={e => onChange(e.target.value)}
        placeholder={`e.g. params.${fieldName} or node-1.field`}
        className="field-input text-[10px] font-mono"
      />
    </div>
  )
}
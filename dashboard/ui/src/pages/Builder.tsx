// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
import { useCallback, useEffect, useRef, useState } from 'react'
import { useParams, useNavigate, Link } from 'react-router-dom'
import {
  ReactFlow,
  Background,
  Controls,
  MiniMap,
  addEdge,
  useNodesState,
  useEdgesState,
  type Connection,
  type Node,
  type Edge,
  ReactFlowProvider,
  useReactFlow,
} from '@xyflow/react'
import ActivityNode from '../components/nodes/ActivityNode'
import ParallelNode from '../components/nodes/ParallelNode'
import ConditionNode from '../components/nodes/ConditionNode'
import ApprovalNode from '../components/nodes/ApprovalNode'
import ActivityPalette from '../components/ActivityPalette'
import PropertyPanel from '../components/PropertyPanel'
import { api } from '../api/client'
import type { NodeProperty } from '../components/properties/DynamicField'

const nodeTypes = {
  activity: ActivityNode,
  parallel: ParallelNode,
  condition: ConditionNode,
  approval: ApprovalNode,
}

// Tightened ActivityMeta used for drag-drop payloads. The wire-format from the
// API may be looser; we runtime-validate a couple of required string fields
// before trusting the cast.
interface BuilderActivityMeta {
  name: string
  category: string
  description?: string
  input_schema?: NodeProperty[]
}

function isBuilderActivityMeta(v: unknown): v is BuilderActivityMeta {
  if (!v || typeof v !== 'object') return false
  const o = v as Record<string, unknown>
  return typeof o.name === 'string' && typeof o.category === 'string'
}

type GraphNode = {
  id: string
  type?: string
  activityName?: string
  category?: string
  description?: string
  position?: { x?: number; y?: number }
  config?: Record<string, unknown>
  inputMapping?: Record<string, string>
  inputSchema?: NodeProperty[]
  children?: string[]
  condition?: {
    expression?: string
    trueBranch?: string
    falseBranch?: string
  }
}

type GraphEdge = {
  source: string
  target: string
}

type WorkflowGraph = {
  nodes: GraphNode[]
  edges: GraphEdge[]
}

type BuilderNodeData = Record<string, unknown> & {
  children?: string[]
  inputSchema?: NodeProperty[]
  activityName?: string
  category?: string
  description?: string
  config?: Record<string, unknown>
  inputMapping?: Record<string, string>
  expression?: string
  trueBranch?: string
  falseBranch?: string
}

function BuilderInner() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const { screenToFlowPosition, fitView } = useReactFlow()
  const reactFlowRef = useRef<HTMLDivElement>(null)
  const [loaded, setLoaded] = useState(false)

  const [nodes, setNodes, onNodesChange] = useNodesState<Node>([])
  const [edges, setEdges, onEdgesChange] = useEdgesState<Edge>([])
  const [selectedNode, setSelectedNode] = useState<Node | null>(null)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [saving, setSaving] = useState(false)
  // When the existing definition JSON cannot be parsed, we surface a banner
  // and disable Save so the user does not silently overwrite their broken
  // (but possibly-recoverable) definition with an empty graph.
  const [parseError, setParseError] = useState<string | null>(null)
  // Raw JSON copy preserved on parse failure so the user can copy/edit the
  // broken definition. We re-parse on demand via the Re-parse button below.
  const [rawDefinition, setRawDefinition] = useState<string>('')

  // Per-component node-id counter — replaces module-level mutable global so
  // navigating between workflows doesn't cause id collisions.
  const nodeIdCounterRef = useRef<number>(0)
  const nextId = useCallback(() => `node-${++nodeIdCounterRef.current}`, [])

  // Load existing definition
  useEffect(() => {
    if (!id) {
      // New workflow — reset counter so a fresh page starts at 0.
      nodeIdCounterRef.current = 0
      setLoaded(true)
      return
    }
    let cancelled = false
    api.getDefinition(id).then(def => {
      if (cancelled) return
      setName(def.name)
      setDescription(def.description)

      let graph: Partial<WorkflowGraph> | null = null
      try {
        graph = JSON.parse(def.definition) as Partial<WorkflowGraph>
        setParseError(null)
        setRawDefinition(def.definition)
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err)
        console.error('Failed to parse workflow definition JSON:', msg, def.definition)
        // Surface the error in the UI and disable Save so the user does not
        // overwrite a recoverable broken definition with an empty graph.
        // Stash the raw JSON so the user can copy or repair it inline.
        setParseError(msg)
        setRawDefinition(def.definition)
        setLoaded(true)
        return
      }

      const graphNodes = graph?.nodes || []
      if (graphNodes.length > 0) {
        // First pass: identify which nodes are children of parallel nodes
        const childToParent = new Map<string, string>()
        for (const n of graphNodes) {
          if (n.type === 'parallel' && n.children) {
            for (const childId of n.children) {
              childToParent.set(childId, n.id)
            }
          }
        }

        // Build all nodes, setting parentId and relative position for children
        const allNodes: Node[] = []
        for (const n of graphNodes) {
          const parentId = childToParent.get(n.id)
          const isParallel = n.type === 'parallel'

          const node: Node = {
            id: n.id,
            type: isParallel ? 'parallel' : n.type === 'condition' ? 'condition' : n.type === 'approval' ? 'approval' : 'activity',
            position: { x: n.position?.x ?? 0, y: n.position?.y ?? 0 },
            data: {
              label: n.activityName?.replace('Activity', '') || n.type,
              activityName: n.activityName || '',
              category: n.category || '',
              description: n.description || '',
              config: n.config || {},
              inputMapping: n.inputMapping || {},
              inputSchema: n.inputSchema || [],
              expression: n.condition?.expression || '',
              trueBranch: n.condition?.trueBranch || '',
              falseBranch: n.condition?.falseBranch || '',
              children: n.children || [],
            },
          }

          if (isParallel) {
            // Parallel nodes are groups — size them to contain children.
            const childPositions = (n.children || [])
              .map((cid: string) => graphNodes.find(x => x.id === cid))
              .filter((child): child is GraphNode => Boolean(child))

            if (childPositions.length === 0) {
              // Default size for empty parallel group — avoid Math.min/max(...[])
              // which yields ±Infinity.
              node.style = { width: 250, height: 150 }
            } else {
              const minX = Math.min(...childPositions.map(c => c.position?.x ?? 0))
              const maxX = Math.max(...childPositions.map(c => (c.position?.x ?? 0) + 220))
              const maxY = Math.max(...childPositions.map(c => (c.position?.y ?? 0) + 80))
              node.style = {
                width: Math.max(maxX - minX + 60, 250),
                height: Math.max(maxY - (n.position?.y ?? 0) + 60, 150),
              }
            }
          }

          if (parentId) {
            node.parentId = parentId
            node.extent = 'parent' as const
            // Make position relative to parent
            const parent = graphNodes.find(x => x.id === parentId)
            if (parent?.position) {
              node.position = {
                x: (n.position?.x ?? 0) - (parent.position.x ?? 0),
                y: (n.position?.y ?? 0) - (parent.position.y ?? 0),
              }
            }
          }

          allNodes.push(node)
        }

        // Sort: parents before children (ReactFlow requirement)
        allNodes.sort((a, b) => {
          if (a.parentId && !b.parentId) return 1
          if (!a.parentId && b.parentId) return -1
          return 0
        })

        setNodes(allNodes)
        // Reset nodeIdCounterRef to the highest existing node-N suffix so the
        // next nextId() call does not collide with a loaded id. Without this,
        // loading nodes named node-1..node-7 leaves the counter at 0 and the
        // very next drop emits node-1, which ReactFlow may merge or crash on.
        // Math.max returns -Infinity on an empty array; we floor at 0 so the
        // counter never moves backwards from a fresh page.
        const maxId = Math.max(0, ...allNodes
          .map(n => parseInt(n.id.replace('node-', ''), 10))
          .filter(Number.isFinite))
        nodeIdCounterRef.current = maxId
      }

      if (graph?.edges) {
        const loadedEdges: Edge[] = graph.edges.map(e => ({
          id: `${e.source}-${e.target}`,
          source: e.source,
          target: e.target,
          animated: true,
          style: { stroke: '#52525b' },
        }))

        // Add internal fan-out/fan-in edges for parallel groups
        for (const n of graphNodes) {
          if (n.type === 'parallel' && n.children?.length) {
            for (const childId of n.children) {
              loadedEdges.push({
                id: `par-in-${n.id}-${childId}`,
                source: n.id,
                sourceHandle: 'fan-out',
                target: childId,
                animated: true,
                style: { stroke: '#a855f7', strokeDasharray: '4 4' },
              })
              loadedEdges.push({
                id: `par-out-${childId}-${n.id}`,
                source: childId,
                target: n.id,
                targetHandle: 'fan-in',
                animated: true,
                style: { stroke: '#a855f7', strokeDasharray: '4 4' },
              })
            }
          }
        }

        // Fix external edges that connect to/from parallel nodes to use correct handles
        for (const e of loadedEdges) {
          if (e.id.startsWith('par-')) continue
          const sourceNode = graphNodes.find(n => n.id === e.source)
          const targetNode = graphNodes.find(n => n.id === e.target)
          if (sourceNode?.type === 'parallel') e.sourceHandle = 'out'
          if (targetNode?.type === 'parallel') e.targetHandle = 'in'
        }

        setEdges(loadedEdges)
      }
      setLoaded(true)
    }).catch(err => {
      if (cancelled) return
      console.error('Failed to load workflow definition:', err)
      navigate('/')
    })

    return () => {
      cancelled = true
    }
  }, [id, navigate, setEdges, setNodes])

  // Fit view after nodes load
  useEffect(() => {
    if (loaded && nodes.length > 0) {
      const t = setTimeout(() => fitView({ padding: 0.2 }), 100)
      return () => clearTimeout(t)
    }
  }, [loaded, nodes.length, fitView])

  const onConnect = useCallback((conn: Connection) => {
    // If connecting to/from a parallel node via default handles, route to the named ones
    const sourceNode = nodes.find(n => n.id === conn.source)
    const targetNode = nodes.find(n => n.id === conn.target)
    const patched = { ...conn }
    if (sourceNode?.type === 'parallel' && !conn.sourceHandle) patched.sourceHandle = 'out'
    if (targetNode?.type === 'parallel' && !conn.targetHandle) patched.targetHandle = 'in'
    setEdges(eds => addEdge({ ...patched, animated: true, style: { stroke: '#52525b' } }, eds))
  }, [setEdges, nodes])

  const onDragOver = useCallback((e: React.DragEvent) => {
    e.preventDefault()
    e.dataTransfer.dropEffect = 'move'
  }, [])

  const onDrop = useCallback((e: React.DragEvent) => {
    e.preventDefault()
    const raw = e.dataTransfer.getData('application/json')
    if (!raw) return

    let activity: BuilderActivityMeta
    try {
      const parsed = JSON.parse(raw)
      if (!isBuilderActivityMeta(parsed)) {
        console.warn('Dropped payload missing required name/category strings:', parsed)
        return
      }
      activity = parsed
    } catch (err) {
      console.warn('Failed to parse drop payload:', err, raw)
      return
    }

    const position = screenToFlowPosition({ x: e.clientX, y: e.clientY })

    if (activity.name === '__parallel') {
      const newNode: Node = {
        id: nextId(),
        type: 'parallel',
        position,
        style: { width: 500, height: 180 },
        data: { label: 'Parallel', children: [] },
      }
      setNodes(nds => [...nds, newNode])
      return
    }

    if (activity.name === '__condition') {
      const newNode: Node = {
        id: nextId(),
        type: 'condition',
        position,
        data: { label: 'Condition', expression: '', trueBranch: '', falseBranch: '' },
      }
      setNodes(nds => [...nds, newNode])
      return
    }

    // Approval-gate substring detection used to live here, but plugin renames
    // (Pass-10) make activity names a moving target. Approval is also not yet
    // implemented at the runtime level, so role-category activities are
    // dropped as plain `activity` nodes for now. A "wait for human" gate is a
    // backend concern — when the runtime supports it we can reintroduce a
    // dedicated palette entry instead of name-sniffing.

    // Check if dropping onto a parallel group node.
    // Style values can be numbers or CSS strings ("500px"); coerce safely.
    const styleNumber = (v: unknown, fallback: number): number => {
      if (typeof v === 'number' && Number.isFinite(v)) return v
      if (typeof v === 'string') {
        const parsed = parseFloat(v)
        if (Number.isFinite(parsed)) return parsed
      }
      return fallback
    }
    const parentNode = nodes.find(n =>
      n.type === 'parallel' &&
      position.x >= n.position.x &&
      position.y >= n.position.y &&
      position.x <= n.position.x + styleNumber(n.style?.width, 500) &&
      position.y <= n.position.y + styleNumber(n.style?.height, 180)
    )

    const newNode: Node = {
      id: nextId(),
      type: 'activity',
      position: parentNode
        ? { x: position.x - parentNode.position.x, y: position.y - parentNode.position.y }
        : position,
      data: {
        label: activity.name.replace('Activity', ''),
        activityName: activity.name,
        category: activity.category,
        description: activity.description,
        config: { timeout: '3m', retryMax: 3, retryInitial: '5s', retryBackoff: 2.0 },
        inputMapping: {},
        inputSchema: activity.input_schema || [],
      },
    }

    if (parentNode) {
      newNode.parentId = parentNode.id
      newNode.extent = 'parent' as const
      // Track the child in the parallel node's data
      setNodes(nds => {
        const updated = nds.map(n =>
          n.id === parentNode.id
            ? { ...n, data: { ...n.data, children: [...(((n.data as BuilderNodeData).children) || []), newNode.id] } }
            : n
        )
        return [...updated, newNode]
      })
      // Add internal fan-out/fan-in edges
      setEdges(eds => [
        ...eds,
        {
          id: `par-in-${parentNode.id}-${newNode.id}`,
          source: parentNode.id,
          sourceHandle: 'fan-out',
          target: newNode.id,
          animated: true,
          style: { stroke: '#a855f7', strokeDasharray: '4 4' },
        },
        {
          id: `par-out-${newNode.id}-${parentNode.id}`,
          source: newNode.id,
          target: parentNode.id,
          targetHandle: 'fan-in',
          animated: true,
          style: { stroke: '#a855f7', strokeDasharray: '4 4' },
        },
      ])
      return
    }

    setNodes(nds => [...nds, newNode])
  }, [screenToFlowPosition, setEdges, setNodes, nodes, nextId])

  const onNodeClick = useCallback((_: React.MouseEvent, node: Node) => {
    setSelectedNode(node)
  }, [])

  const onPaneClick = useCallback(() => {
    setSelectedNode(null)
  }, [])

  const handleNodeUpdate = useCallback((nodeId: string, data: Record<string, unknown>) => {
    setNodes(nds => nds.map(n => n.id === nodeId ? { ...n, data } : n))
    setSelectedNode(prev => prev?.id === nodeId ? { ...prev, data } : prev)
  }, [setNodes])

  const handleNodeDelete = useCallback((nodeId: string) => {
    // Also remove from parent's children list
    setNodes(nds => {
      const node = nds.find(n => n.id === nodeId)
      let filtered = nds.filter(n => n.id !== nodeId && n.parentId !== nodeId)
      if (node?.parentId) {
        filtered = filtered.map(n =>
          n.id === node.parentId
            ? { ...n, data: { ...n.data, children: (((n.data as BuilderNodeData).children) || []).filter(c => c !== nodeId) } }
            : n
        )
      }
      return filtered
    })
    // Edge filtering by structural (source/target) match — string-suffix
    // heuristics matched unrelated edges (e.g. node-12 would match edges
    // ending in -2). source/target check covers all edges including the
    // par-in-/par-out- internals because those still reference nodeId on
    // one side.
    setEdges(eds => eds.filter(e => e.source !== nodeId && e.target !== nodeId))
    setSelectedNode(null)
  }, [setNodes, setEdges])

  // Build the graph definition for saving
  const buildDefinition = () => {
    const graphNodes = nodes.map(n => {
      const d = n.data as BuilderNodeData
      const base: GraphNode = {
        id: n.id,
        type: n.type,
        position: n.parentId
          ? { x: n.position.x + (nodes.find(p => p.id === n.parentId)?.position.x || 0), y: n.position.y + (nodes.find(p => p.id === n.parentId)?.position.y || 0) }
          : n.position,
      }
      if (n.type === 'activity' || n.type === 'approval') {
        base.activityName = String(d.activityName || '')
        base.config = (d.config as Record<string, unknown>) || {}
        base.inputMapping = (d.inputMapping as Record<string, string>) || {}
        base.description = String(d.description || '')
        // Preserve inputSchema across save/load. Without this, the schema is
        // dropped on first save and the property panel renders an empty form
        // the next time the workflow is opened.
        const inputSchema = d.inputSchema
        if (Array.isArray(inputSchema) && inputSchema.length > 0) {
          base.inputSchema = inputSchema as NodeProperty[]
        }
      }
      if (n.type === 'parallel') {
        base.children = d.children || []
        base.description = String(d.description || '')
      }
      if (n.type === 'condition') {
        base.condition = {
          expression: String(d.expression || ''),
          trueBranch: String(d.trueBranch || ''),
          falseBranch: String(d.falseBranch || ''),
        }
      }
      return base
    })

    const graphEdges = edges
      .filter(e => !e.id.startsWith('par-in-') && !e.id.startsWith('par-out-'))
      .map(e => ({
        source: e.source,
        target: e.target,
      }))

    return { nodes: graphNodes, edges: graphEdges, parameters: [] }
  }

  // Try to re-parse the rawDefinition the user has been editing. On success,
  // clear the error so the canvas re-renders with the patched graph.
  const tryReparse = useCallback(() => {
    try {
      const parsed = JSON.parse(rawDefinition) as Partial<WorkflowGraph>
      setParseError(null)
      // The full reload pipeline lives in the loader effect; the simplest way
      // to re-run it is to navigate to the same id so the effect fires again.
      // But we also have all the data in `parsed`, so we can apply it inline:
      const graphNodes = parsed?.nodes || []
      const allNodes: Node[] = graphNodes.map(n => ({
        id: n.id,
        type: n.type === 'parallel' ? 'parallel' : n.type === 'condition' ? 'condition' : n.type === 'approval' ? 'approval' : 'activity',
        position: { x: n.position?.x ?? 0, y: n.position?.y ?? 0 },
        data: {
          label: n.activityName?.replace('Activity', '') || n.type,
          activityName: n.activityName || '',
          category: n.category || '',
          description: n.description || '',
          config: n.config || {},
          inputMapping: n.inputMapping || {},
          inputSchema: n.inputSchema || [],
          expression: n.condition?.expression || '',
          trueBranch: n.condition?.trueBranch || '',
          falseBranch: n.condition?.falseBranch || '',
          children: n.children || [],
        },
      }))
      setNodes(allNodes)
      // Same reset logic as the loader effect — the user may have edited the
      // raw JSON to re-add node-12, and we don't want the next drop to emit
      // node-1 when the highest existing id is now 12.
      const maxId = Math.max(0, ...allNodes
        .map(n => parseInt(n.id.replace('node-', ''), 10))
        .filter(Number.isFinite))
      nodeIdCounterRef.current = maxId
      const loadedEdges: Edge[] = (parsed?.edges || []).map(e => ({
        id: `${e.source}-${e.target}`,
        source: e.source,
        target: e.target,
        animated: true,
        style: { stroke: '#52525b' },
      }))
      setEdges(loadedEdges)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      setParseError(msg)
    }
  }, [rawDefinition, setNodes, setEdges])

  // Discard whatever broken state we loaded and reset to a fresh empty graph.
  // The user keeps the workflow's id and metadata; only the graph is wiped.
  const discardAndReset = useCallback(() => {
    if (!confirm('Discard the broken definition and start over with an empty graph?')) return
    setNodes([])
    setEdges([])
    setParseError(null)
    setRawDefinition('')
    nodeIdCounterRef.current = 0
  }, [setNodes, setEdges])

  const handleSave = async () => {
    if (!name.trim()) {
      alert('Please enter a workflow name')
      return
    }
    setSaving(true)
    try {
      const definition = buildDefinition()
      if (id) {
        await api.updateDefinition(id, { name, description, definition })
      } else {
        const created = await api.createDefinition({ name, description, definition })
        navigate(`/builder/${created.id}`, { replace: true })
      }
    } catch (e: unknown) {
      alert(`Save failed: ${e instanceof Error ? e.message : 'unknown error'}`)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="h-screen flex flex-col bg-zinc-950 text-zinc-100">
      {/* Top bar */}
      <div className="h-12 border-b border-zinc-800 bg-zinc-950/80 backdrop-blur-sm flex items-center px-4 gap-4 flex-shrink-0">
        <Link to="/" className="text-zinc-500 hover:text-zinc-300 text-sm transition-colors">
          &larr; Back
        </Link>
        <div className="h-5 w-px bg-zinc-800" />
        <input
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder="Workflow name..."
          className="bg-transparent text-sm text-white font-medium placeholder:text-zinc-600 focus:outline-none flex-1 min-w-0"
        />
        <input
          value={description}
          onChange={e => setDescription(e.target.value)}
          placeholder="Description..."
          className="bg-transparent text-xs text-zinc-400 placeholder:text-zinc-700 focus:outline-none w-64"
        />
        <button
          onClick={handleSave}
          disabled={saving || parseError !== null}
          title={parseError ? 'Save disabled: existing definition JSON failed to parse' : undefined}
          className="px-4 py-1.5 bg-indigo-600 hover:bg-indigo-500 disabled:opacity-50 text-white text-sm font-medium rounded-md transition-colors"
        >
          {saving ? 'Saving...' : 'Save'}
        </button>
      </div>

      {parseError && (
        <div className="bg-red-500/10 border-b border-red-500/30 px-4 py-3 text-xs text-red-300">
          <div className="flex items-start gap-2 mb-2">
            <span className="font-medium">Definition parse error:</span>
            <span className="font-mono break-all">{parseError}</span>
            <span className="ml-auto text-red-400/70 whitespace-nowrap">
              Save is disabled. Repair the JSON below or discard.
            </span>
          </div>
          <textarea
            value={rawDefinition}
            onChange={e => setRawDefinition(e.target.value)}
            spellCheck={false}
            rows={8}
            aria-label="Raw workflow definition JSON"
            className="w-full bg-zinc-900 border border-red-500/30 rounded px-2 py-1.5 text-[11px] font-mono text-zinc-200 focus:outline-none focus:border-red-400 resize-y"
          />
          <div className="flex gap-2 mt-2">
            <button
              type="button"
              onClick={tryReparse}
              className="px-3 py-1 bg-indigo-600 hover:bg-indigo-500 text-white text-[11px] rounded transition-colors"
            >
              Re-parse
            </button>
            <button
              type="button"
              onClick={discardAndReset}
              className="px-3 py-1 bg-zinc-800 hover:bg-zinc-700 text-zinc-300 text-[11px] rounded transition-colors"
            >
              Discard and start over
            </button>
          </div>
        </div>
      )}

      {/* Main area */}
      <div className="flex flex-1 min-h-0">
        <ActivityPalette />

        <div className="flex-1 relative" ref={reactFlowRef}>
          <ReactFlow
            nodes={nodes}
            edges={edges}
            onNodesChange={onNodesChange}
            onEdgesChange={onEdgesChange}
            onConnect={onConnect}
            onDrop={onDrop}
            onDragOver={onDragOver}
            onNodeClick={onNodeClick}
            onPaneClick={onPaneClick}
            nodeTypes={nodeTypes}
            fitView
            proOptions={{ hideAttribution: true }}
            defaultEdgeOptions={{ animated: true, style: { stroke: '#52525b' } }}
            className="bg-zinc-950"
          >
            <Background color="#27272a" gap={20} />
            <Controls
              className="!bg-zinc-800 !border-zinc-700 !rounded-lg [&>button]:!bg-zinc-800 [&>button]:!border-zinc-700 [&>button]:!text-zinc-400 [&>button:hover]:!bg-zinc-700"
            />
            <MiniMap
              nodeColor={(n) => n.type === 'parallel' ? '#7c3aed33' : '#3f3f46'}
              maskColor="rgba(0,0,0,0.7)"
              className="!bg-zinc-900 !border-zinc-800 !rounded-lg"
            />
          </ReactFlow>

          {nodes.length === 0 && (
            <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
              <div className="text-center">
                <p className="text-zinc-600 text-sm">Drag activities from the left panel onto the canvas</p>
                <p className="text-zinc-700 text-xs mt-1">Drop activities onto a Parallel group to run them concurrently</p>
              </div>
            </div>
          )}
        </div>

        <PropertyPanel
          node={selectedNode}
          onUpdate={handleNodeUpdate}
          onDelete={handleNodeDelete}
        />
      </div>
    </div>
  )
}

export default function Builder() {
  return (
    <ReactFlowProvider>
      <BuilderInner />
    </ReactFlowProvider>
  )
}
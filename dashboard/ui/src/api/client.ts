// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api/v1'

// Default fetch deadline. 30s is comfortably above the slowest synchronous
// API call (workflow trigger waits on Temporal client.SignalWithStart) and
// below most upstream load-balancer idle limits, so we hang up before the
// network does. Override via opts.signal if a caller needs a different bound.
const DEFAULT_TIMEOUT_MS = 30000

// Where the API token lives. sessionStorage (NOT localStorage) so the token
// is dropped when the tab closes — defense in depth against an XSS that
// would otherwise persist across browser restarts. The export is so
// auth-flow code (login/logout) doesn't have to repeat the key string.
const TOKEN_STORAGE_KEY = 'gogents_api_token'

export function getAuthToken(): string | null {
  return window.sessionStorage.getItem(TOKEN_STORAGE_KEY)
}

export function setAuthToken(token: string): void {
  window.sessionStorage.setItem(TOKEN_STORAGE_KEY, token)
}

export function clearAuthToken(): void {
  window.sessionStorage.removeItem(TOKEN_STORAGE_KEY)
}

/**
 * HttpError surfaces the response status alongside the message so callers
 * can branch on it (e.g. show a login prompt on 401, retry on 503) without
 * fragile message-string parsing. Generic Error was the previous behavior;
 * existing catch sites that only read `.message` continue to work because
 * HttpError extends Error.
 */
export class HttpError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = 'HttpError'
    this.status = status
  }
}

export interface RequestOptions extends Omit<RequestInit, 'signal'> {
  signal?: AbortSignal
}

/**
 * Build a timeout AbortSignal. AbortSignal.timeout (ES2024) is preferred when
 * available; older browsers (Safari < 17.4, older Chromium) lack it and
 * would throw a TypeError on direct call. The setTimeout fallback returns an
 * equivalent abortable signal so callers don't have to care which path ran.
 */
function timeoutSignal(ms: number): AbortSignal {
  if (
    typeof AbortSignal !== 'undefined' &&
    typeof (AbortSignal as unknown as { timeout?: (ms: number) => AbortSignal }).timeout === 'function'
  ) {
    return (AbortSignal as unknown as { timeout: (ms: number) => AbortSignal }).timeout(ms)
  }
  const ctl = new AbortController()
  setTimeout(
    () => ctl.abort(new DOMException(`Timeout after ${ms}ms`, 'TimeoutError')),
    ms,
  )
  return ctl.signal
}

/**
 * Combine a caller-supplied AbortSignal (if any) with the default 30s
 * timeout. AbortSignal.any and AbortSignal.timeout are BOTH ES2024 and BOTH
 * absent on older browsers, so each is runtime-checked separately — the
 * earlier version of this code only gated `.any` and would throw on
 * `.timeout()` before the `.any` check was reached. Both fallbacks are
 * functionally equivalent to the native primitives.
 */
function combinedSignal(userSignal: AbortSignal | undefined): AbortSignal {
  const timeout = timeoutSignal(DEFAULT_TIMEOUT_MS)
  if (!userSignal) return timeout
  // Cast through unknown because some lib.dom variants miss `any`.
  const anyFn = (AbortSignal as unknown as { any?: (signals: AbortSignal[]) => AbortSignal }).any
  if (typeof anyFn === 'function') {
    return anyFn([userSignal, timeout])
  }
  // Manual fallback: forward whichever fires first to a fresh controller.
  const ctl = new AbortController()
  const onAbort = (s: AbortSignal) => () => ctl.abort(s.reason)
  if (userSignal.aborted) ctl.abort(userSignal.reason)
  else userSignal.addEventListener('abort', onAbort(userSignal), { once: true })
  if (timeout.aborted) ctl.abort(timeout.reason)
  else timeout.addEventListener('abort', onAbort(timeout), { once: true })
  return ctl.signal
}

async function request<T>(path: string, opts?: RequestOptions): Promise<T> {
  const token = getAuthToken()
  const res = await fetch(`${BASE}${path}`, {
    ...opts,
    signal: combinedSignal(opts?.signal),
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...opts?.headers,
    },
  })

  if (!res.ok) {
    let message = `HTTP ${res.status}`
    try {
      const errJson = await res.json()
      if (errJson && typeof errJson === 'object' && typeof errJson.error === 'string') {
        message = errJson.error
      }
    } catch {
      // body wasn't JSON, use status
    }
    throw new HttpError(res.status, message)
  }

  // 204 No Content responses have no body to parse. Return undefined cast as T
  // so callers that expect e.g. {id: string} from a DELETE still type-check; the
  // alternative is forcing every DELETE caller to switch on the absence of data.
  if (res.status === 204) return undefined as T

  const contentType = res.headers.get('Content-Type') || ''
  if (!contentType.includes('application/json')) {
    throw new Error(`expected JSON, got ${contentType || 'no content-type'}`)
  }

  const json = await res.json()
  if (!json || typeof json !== 'object') {
    throw new Error('Invalid response: expected JSON object')
  }
  if (!json.success) throw new Error(json.error || 'Request failed')
  return json.data as T
}

// --- Types ---

export interface ActivityMeta {
  name: string
  description: string
  category: string
  input_schema: FieldMeta[]
  output_type: string
}

export interface FieldMeta {
  name: string
  type: string
  required: boolean
  description?: string
  default?: string
}

export interface WorkflowDefinition {
  id: string
  name: string
  description: string
  definition: string
  created_at: string
  updated_at: string
}

export interface WorkflowRun {
  id: string
  definition_id: string
  temporal_id: string
  temporal_run: string
  status: string
  input: string
  output?: string
  started_at: string
  finished_at?: string
}

export interface Role {
  id: string
  label: string
  group: string
  description: string
}

export interface RoleGroup {
  id: string
  label: string
  icon: string
}

// --- API ---

// All path segments built from caller-supplied identifiers go through
// encodeURIComponent so a slash, hash, or '?' in an `id` cannot escape into
// the URL grammar (path traversal, accidental query split, server-side
// confusion). Type literals and static path components are NOT encoded —
// that would be both pointless and corrupt the URL.

export const api = {
  listActivities: (opts?: RequestOptions) =>
    request<ActivityMeta[]>('/workflows/activities', opts),

  listRoles: (opts?: RequestOptions) =>
    request<{ roles: Role[]; groups: RoleGroup[] }>('/workflows/roles', opts),

  listDefinitions: (opts?: RequestOptions) =>
    request<WorkflowDefinition[]>('/workflows/definitions', opts),

  getDefinition: (id: string, opts?: RequestOptions) =>
    request<WorkflowDefinition>(`/workflows/definitions/${encodeURIComponent(id)}`, opts),

  createDefinition: (data: { name: string; description: string; definition: object }, opts?: RequestOptions) =>
    request<WorkflowDefinition>('/workflows/definitions', {
      ...opts,
      method: 'POST',
      body: JSON.stringify(data),
    }),

  updateDefinition: (id: string, data: { name: string; description: string; definition: object }, opts?: RequestOptions) =>
    request<WorkflowDefinition>(`/workflows/definitions/${encodeURIComponent(id)}`, {
      ...opts,
      method: 'PUT',
      body: JSON.stringify(data),
    }),

  deleteDefinition: (id: string, opts?: RequestOptions) =>
    request<{ id: string }>(`/workflows/definitions/${encodeURIComponent(id)}`, { ...opts, method: 'DELETE' }),

  runDefinition: (id: string, params: Record<string, unknown> = {}, opts?: RequestOptions) =>
    request<{ run: WorkflowRun; workflowID: string; runID: string }>(
      `/workflows/definitions/${encodeURIComponent(id)}/run`,
      { ...opts, method: 'POST', body: JSON.stringify(params) },
    ),

  listRuns: (definitionId?: string, opts?: RequestOptions) =>
    request<WorkflowRun[]>(
      `/workflows/runs${definitionId ? `?definition_id=${encodeURIComponent(definitionId)}` : ''}`,
      opts,
    ),

  getRun: (id: string, opts?: RequestOptions) =>
    request<WorkflowRun>(`/workflows/runs/${encodeURIComponent(id)}`, opts),

  // --- Delegator (read-only quota + tasks; new delegations go through MCP) ---

  delegatorQuota: (days = 1, opts?: RequestOptions) =>
    request<{ agents: AgentUsage[] }>(`/delegator/quota?days=${encodeURIComponent(days)}`, opts),

  delegatorTasks: (filters?: { status?: string; agent?: string }, opts?: RequestOptions) => {
    const qs = new URLSearchParams()
    if (filters?.status) qs.set('status', filters.status)
    if (filters?.agent) qs.set('agent', filters.agent)
    const path = `/delegator/tasks${qs.toString() ? `?${qs}` : ''}`
    return request<DelegatorTask[]>(path, opts)
  },
}

export interface AgentUsage {
  agent: string
  apiType: string
  sessions: number
  inputTokens: number
  outputTokens: number
  model: string
  utilization5h?: number
  utilization7d?: number
  resetTime5h?: string
  resetTime7d?: string
}

export interface DelegatorTask {
  id: string
  agent: string
  provider: string
  difficulty: string
  priority: string
  status: 'pending' | 'running' | 'completed' | 'failed' | 'cancelled' | 'timed_out'
  prompt_preview: string
  created_at: string
  started_at: string | null
  completed_at: string | null
  exit_code: number
  elapsed_seconds: number
  error: string
}
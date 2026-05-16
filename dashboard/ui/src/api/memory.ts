// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
// memory.ts — typed fetch helpers for /api/v1/memory/*. The other api
// modules in this folder route every request through the API envelope
// in client.ts (which expects {success, data}). The memory endpoints
// instead return raw JSON the way the evo/skills endpoints do, so we
// fetch directly here and handle the few status codes ourselves.

const BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api/v1'

const TOKEN_STORAGE_KEY = 'gogents_api_token'

function authToken(): string | null {
  return window.sessionStorage.getItem(TOKEN_STORAGE_KEY)
}

async function rawFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const token = authToken()
  const res = await fetch(`${BASE}${path}`, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init?.headers ?? {}),
    },
  })
  if (res.status === 204) return undefined as T
  const ct = res.headers.get('Content-Type') ?? ''
  const parsed = ct.includes('application/json') ? await res.json() : null
  if (!res.ok) {
    // Surface the server's error message when present, fall back to
    // the status line so the caller never sees a bare "TypeError".
    const msg = parsed && typeof parsed === 'object' && 'error' in parsed && typeof parsed.error === 'string'
      ? parsed.error
      : `HTTP ${res.status}`
    throw new Error(msg)
  }
  return parsed as T
}

export interface MemorySource {
  key: string
  label: string
  kind: 'files' | 'file' | 'postgres'
  count: number
  last_modified?: string
  path?: string
  available: boolean
  note?: string
}

export interface MemoryItem {
  id: string
  agent: string
  scope?: string
  name: string
  description: string
  type?: string
  body?: string
  path: string
  last_modified: string
  has_matter: boolean
}

export interface MemoryInsight {
  id: number
  domain: string
  title: string
  body: string
  tags: string[]
  has_embedding: boolean
  source_program_id?: number
  created_at: string
}

export interface MemoryEntity {
  id: string
  parent_id: string
  name: string
  category: string
  description: string
  created_at: string
  updated_at: string
}

export interface MemoryRelationship {
  id: string
  parent_id: string
  subject: string
  predicate: string
  object: string
  description: string
  weight: number
  created_at: string
  updated_at: string
}

export interface MemoryWriteInput {
  name?: string
  description?: string
  type?: string
  scope?: string
  body?: string
}

export const memoryApi = {
  listSources: () => rawFetch<MemorySource[]>('/memory/sources'),

  listAgent: (agent: string) =>
    rawFetch<MemoryItem[]>(`/memory/agents/${encodeURIComponent(agent)}/items`),

  getItem: (agent: string, id: string) =>
    rawFetch<MemoryItem>(`/memory/agents/${encodeURIComponent(agent)}/items/${encodeURIComponent(id)}`),

  createItem: (agent: string, input: MemoryWriteInput) =>
    rawFetch<MemoryItem>(`/memory/agents/${encodeURIComponent(agent)}/items`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),

  updateItem: (agent: string, id: string, input: MemoryWriteInput) =>
    rawFetch<MemoryItem>(`/memory/agents/${encodeURIComponent(agent)}/items/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(input),
    }),

  deleteItem: (agent: string, id: string) =>
    rawFetch<void>(`/memory/agents/${encodeURIComponent(agent)}/items/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    }),

  listInsights: (q = '', limit = 200) => {
    const qs = new URLSearchParams()
    if (q) qs.set('q', q)
    qs.set('limit', String(limit))
    return rawFetch<MemoryInsight[]>(`/memory/insights?${qs}`)
  },

  createInsight: (input: { domain: string; title: string; body: string; tags?: string[] }) =>
    rawFetch<MemoryInsight>('/memory/insights', { method: 'POST', body: JSON.stringify(input) }),

  updateInsight: (id: number, input: { domain?: string; title?: string; body?: string; tags?: string[] }) =>
    rawFetch<MemoryInsight>(`/memory/insights/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(input),
    }),

  deleteInsight: (id: number) =>
    rawFetch<void>(`/memory/insights/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  listEntities: (q = '', limit = 200) => {
    const qs = new URLSearchParams()
    if (q) qs.set('q', q)
    qs.set('limit', String(limit))
    return rawFetch<MemoryEntity[]>(`/memory/entities?${qs}`)
  },

  updateEntity: (id: string, input: { description?: string; category?: string }) =>
    rawFetch<MemoryEntity>(`/memory/entities/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(input),
    }),

  deleteEntity: (id: string) =>
    rawFetch<void>(`/memory/entities/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  listRelationships: (q = '', limit = 200) => {
    const qs = new URLSearchParams()
    if (q) qs.set('q', q)
    qs.set('limit', String(limit))
    return rawFetch<MemoryRelationship[]>(`/memory/relationships?${qs}`)
  },

  updateRelationship: (id: string, input: { predicate?: string; description?: string; weight?: number }) =>
    rawFetch<MemoryRelationship>(`/memory/relationships/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(input),
    }),

  deleteRelationship: (id: string) =>
    rawFetch<void>(`/memory/relationships/${encodeURIComponent(id)}`, { method: 'DELETE' }),
}
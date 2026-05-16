// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
// Re-export the delegator types and a thin facade over the central api
// object. Components import from here so the import path is "./api/delegator"
// rather than the catch-all "./api/client".
import { api, type AgentUsage, type DelegatorTask } from './client'

export type { AgentUsage, DelegatorTask }

export const delegatorApi = {
  getQuota: api.delegatorQuota,
  listTasks: api.delegatorTasks,
}
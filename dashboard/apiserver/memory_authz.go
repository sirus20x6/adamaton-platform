package apiserver

// Row-level authorization for the memory_db endpoints.
//
// Caller identity rides in the X-Agent-ID header (defaulting to "dashboard"
// for the SPA, which doesn't send one). Two enforcement layers:
//
//   - evo.insights rows carry an `owner` column (added best-effort at boot;
//     see ensureInsightsOwnerColumn). Creates stamp the caller as owner.
//     Updates/deletes only touch rows whose owner is NULL (legacy rows,
//     writable by anyone for compatibility) or equals the caller. Agents in
//     EVO_MEMORY_ADMIN_AGENTS bypass the owner check.
//
//   - deepresearch documents_entities / documents_relationships live in an
//     external schema we can't add columns to, so mutations are gated by an
//     optional writer allowlist: when EVO_MEMORY_DB_WRITERS is set (comma-
//     separated agent ids), only listed callers (or admins) may mutate.
//     Unset = allow all authenticated callers, preserving existing behaviour.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	agentIDHeader         = "X-Agent-ID"
	defaultCallerAgentID  = "dashboard"
	memoryAdminAgentsEnv  = "EVO_MEMORY_ADMIN_AGENTS"
	memoryGraphWritersEnv = "EVO_MEMORY_DB_WRITERS"
	maxAgentIDLen         = 128
)

// callerAgentID extracts the caller identity for row-level authz. Falls back
// to "dashboard" when absent so the deployed SPA keeps working unchanged.
func callerAgentID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get(agentIDHeader))
	if id == "" {
		return defaultCallerAgentID
	}
	if len(id) > maxAgentIDLen {
		id = id[:maxAgentIDLen]
	}
	return id
}

// inCSVEnv reports whether id appears in the comma-separated env list.
func inCSVEnv(envKey, id string) bool {
	raw := os.Getenv(envKey)
	if raw == "" {
		return false
	}
	for _, e := range strings.Split(raw, ",") {
		if strings.TrimSpace(e) == id {
			return true
		}
	}
	return false
}

// isMemoryAdmin reports whether the caller bypasses row ownership.
func isMemoryAdmin(agentID string) bool {
	return inCSVEnv(memoryAdminAgentsEnv, agentID)
}

// memoryGraphWriteAllowed gates entity/relationship mutations. Open when the
// allowlist env is unset (compatibility); closed to non-listed, non-admin
// callers when set.
func memoryGraphWriteAllowed(agentID string) bool {
	if os.Getenv(memoryGraphWritersEnv) == "" {
		return true
	}
	return inCSVEnv(memoryGraphWritersEnv, agentID) || isMemoryAdmin(agentID)
}

// ensureInsightsOwnerColumn adds the owner column to evo.insights, enabling
// row-level scoping on insights. Best-effort at boot (mirrors the
// runExperimentsMigrations pattern): on failure the endpoints keep the
// legacy no-owner behaviour rather than 500ing.
func (s *APIServer) ensureInsightsOwnerColumn(ctx context.Context) {
	if s.evoPool == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := s.evoPool.Exec(cctx,
		`ALTER TABLE evo.insights ADD COLUMN IF NOT EXISTS owner TEXT`); err != nil {
		s.logger.WithError(err).
			Warn("memory authz: could not ensure evo.insights.owner column; row-level scoping disabled")
		return
	}
	s.insightsOwnerCol.Store(true)
}

// insightsOwnerEnabled reports whether the owner column is available.
func (s *APIServer) insightsOwnerEnabled() bool {
	return s.insightsOwnerCol.Load()
}

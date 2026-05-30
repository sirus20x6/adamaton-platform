package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/sirus20x6/adamaton-platform/dashboard/apiserver/health"
)

// fleetHealth is a 1:1 forwarder type so the apiserver package doesn't
// alias every health.* type inline in every handler. Stored on the
// APIServer struct as `fleetHealth`.
type fleetHealth = *health.Aggregator

// registerHealthEndpoints wires the fleet-health surface. Called from
// setupRoutes() after the existing /system/status registration.
//
// Paths:
//
//	GET    /api/v1/health/fleet         — compact rollup (capabilities only)
//	GET    /api/v1/health/roles         — per-role rollup
//	GET    /api/v1/health/instances     — flat instance list
//	GET    /api/v1/health/topology      — parsed topology.yml
//	DELETE /api/v1/health/cache         — force-refresh the cache
//
// Existing /api/v1/health (liveness) and /api/v1/health/ready are NOT
// touched — those stay as they were.
func (s *APIServer) registerHealthEndpoints(api *mux.Router) {
	api.HandleFunc("/health/fleet", s.getFleetHealth).Methods("GET")
	api.HandleFunc("/health/roles", s.getFleetRoles).Methods("GET")
	api.HandleFunc("/health/instances", s.getFleetInstances).Methods("GET")
	api.HandleFunc("/health/workflows", s.getFleetWorkflows).Methods("GET")
	api.HandleFunc("/health/topology", s.getFleetTopology).Methods("GET")
	api.HandleFunc("/health/cache", s.refreshFleetHealth).Methods("DELETE")
}

// registerPlatformHealthCompat wires /platform/health/ready + /live as
// thin adapters over the new aggregator. Kept at the TOP level (NOT
// under /api/v1) because the SPA's existing axios client pollers were
// pointed at /platform/health/ready (left over from the retired Python
// R2R backend's URL space).
//
// Once the SPA stops calling these, drop the routes.
func (s *APIServer) registerPlatformHealthCompat() {
	s.router.HandleFunc("/platform/health/ready", s.getPlatformHealthReady).Methods("GET")
	s.router.HandleFunc("/platform/health/live", s.getPlatformHealthLive).Methods("GET")
}

func (s *APIServer) getFleetHealth(w http.ResponseWriter, r *http.Request) {
	if s.fleetHealth == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error:   "fleet health unavailable: topology not loaded",
			Success: false,
		})
		return
	}
	snap := s.fleetHealth.Get()
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]any{
			"generated_at": snap.GeneratedAt,
			"stale_for":    snap.StaleFor,
			"capabilities": snap.Capabilities,
			// workflows is the failure gauge; omitted (nil) when no
			// workflow store is wired. Included in the compact rollup so
			// the SPA's top-level health pill can fold a "workflow
			// failure storm" into the same view as capability health.
			"workflows": snap.Workflows,
		},
		Success: true,
	})
}

// getFleetWorkflows returns just the workflow-failure gauge —
// workflows_failed_last_hour and the alert-friendly counters operators
// use to spot a "workflow failure storm". 503s when topology isn't
// loaded; returns the gauge with a 200 even when no workflow source is
// wired (workflows == null) so the SPA can distinguish "health system
// down" from "no workflow telemetry".
func (s *APIServer) getFleetWorkflows(w http.ResponseWriter, r *http.Request) {
	if s.fleetHealth == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error:   "fleet health unavailable: topology not loaded",
			Success: false,
		})
		return
	}
	snap := s.fleetHealth.Get()
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]any{
			"generated_at": snap.GeneratedAt,
			"stale_for":    snap.StaleFor,
			"workflows":    snap.Workflows,
		},
		Success: true,
	})
}

func (s *APIServer) getFleetRoles(w http.ResponseWriter, r *http.Request) {
	if s.fleetHealth == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error:   "fleet health unavailable: topology not loaded",
			Success: false,
		})
		return
	}
	snap := s.fleetHealth.Get()
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]any{
			"generated_at": snap.GeneratedAt,
			"stale_for":    snap.StaleFor,
			"roles":        snap.Roles,
		},
		Success: true,
	})
}

func (s *APIServer) getFleetInstances(w http.ResponseWriter, r *http.Request) {
	if s.fleetHealth == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error:   "fleet health unavailable: topology not loaded",
			Success: false,
		})
		return
	}
	snap := s.fleetHealth.Get()
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]any{
			"generated_at": snap.GeneratedAt,
			"stale_for":    snap.StaleFor,
			"instances":    snap.Instances,
		},
		Success: true,
	})
}

// getFleetTopology returns the parsed topology.yml. Used by the SPA to
// know the static graph (capabilities -> roles) without re-fetching the
// dynamic rollup every render.
func (s *APIServer) getFleetTopology(w http.ResponseWriter, r *http.Request) {
	if s.fleetTopology == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error:   "topology not loaded",
			Success: false,
		})
		return
	}
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]any{
			"roles":        s.fleetTopology.Roles,
			"capabilities": s.fleetTopology.Capabilities,
			"role_order":   s.fleetTopology.RoleOrder,
			"cap_order":    s.fleetTopology.CapabilityOrder,
		},
		Success: true,
	})
}

// refreshFleetHealth force-refreshes the cache. Gated by the
// inflightSem so a misclick can't pile up fanouts.
func (s *APIServer) refreshFleetHealth(w http.ResponseWriter, r *http.Request) {
	if s.fleetHealth == nil {
		s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
			Error:   "fleet health unavailable",
			Success: false,
		})
		return
	}
	select {
	case s.inflightSem <- struct{}{}:
		defer func() { <-s.inflightSem }()
	default:
		s.sendJSON(w, http.StatusTooManyRequests, APIResponse{
			Error:   "too many in-flight mutations; try again shortly",
			Success: false,
		})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.fleetHealth.Refresh(ctx)
	w.WriteHeader(http.StatusOK)
}

// getPlatformHealthReady returns the SystemHealth shape the SPA's
// useSystemHealth() hook expects: {"ok": bool, "deps": {...}}. The deps
// map is derived from the new aggregator's roles — we map
// postgres/redis/(bge-embed → sidecar) onto the historical names the SPA
// stores in its ConnectionStore. This shim lets the SPA keep working
// during the cutover; it can be deleted once the widget moves to the
// new typed hooks.
func (s *APIServer) getPlatformHealthReady(w http.ResponseWriter, r *http.Request) {
	if s.fleetHealth == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "deps": map[string]bool{
				"postgres": false, "redis": false, "sidecar": false,
			},
		})
		return
	}
	snap := s.fleetHealth.Get()
	postgresOK := lookupRoleStatus(snap.Roles, "postgres") == health.StatusOK
	redisOK := lookupRoleStatus(snap.Roles, "redis") == health.StatusOK
	// "sidecar" historically meant the BGE embedding sidecar — map to
	// the bge-embed role. If the topology doesn't include bge-embed
	// (e.g. on a host without RAG), report sidecar as "true" since
	// there's nothing to be sad about.
	sidecarOK := true
	for _, role := range snap.Roles {
		if role.Name == "bge-embed" {
			sidecarOK = role.Status == health.StatusOK
			break
		}
	}
	ok := postgresOK && redisOK && sidecarOK
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": ok,
		"deps": map[string]bool{
			"postgres": postgresOK,
			"redis":    redisOK,
			"sidecar":  sidecarOK,
		},
	})
}

// getPlatformHealthLive is the legacy liveness counterpart — the SPA
// has a useSystemHealth() that may call /platform/health/live in
// addition to /ready. Returns 200 with {"ok": true} whenever the
// apiserver process is up.
func (s *APIServer) getPlatformHealthLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func lookupRoleStatus(roles []health.RoleStatus, name string) health.Status {
	for _, role := range roles {
		if role.Name == name {
			return role.Status
		}
	}
	return health.StatusUnknown
}

// inferLocalHost figures out which host this apiserver process is
// running on, used by the aggregator to decide which instances to
// actively probe vs which to trust the deploy-agent's view of. Source
// preference: HEALTH_LOCAL_HOST env > os.Hostname inference > "pi5"
// fallback (the dominant deploy target).
func inferLocalHost() string {
	if v := osGetenv("HEALTH_LOCAL_HOST"); v != "" {
		return v
	}
	// Match host fragments to likely Adamaton host names.
	for _, candidate := range []string{"pi5-speaker", "pi5", "blackwell", "workstation"} {
		if hostnameContains(candidate) {
			return candidate
		}
	}
	return "pi5"
}

func hostnameContains(needle string) bool {
	hn := osGetenv("HOSTNAME")
	return strings.Contains(strings.ToLower(hn), needle)
}

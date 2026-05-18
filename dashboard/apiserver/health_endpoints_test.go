package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-platform/dashboard/apiserver/health"
)

// newTestAPIServerWithHealth wires a minimal APIServer with a loaded
// topology + an aggregator backed by no-op probers. Aggregator runs
// one initial refresh in-band (no goroutine) so tests are deterministic.
func newTestAPIServerWithHealth(t *testing.T, topologyYAML string) *APIServer {
	t.Helper()
	topo, err := health.LoadTopology(strings.NewReader(topologyYAML))
	if err != nil {
		t.Fatalf("load topology: %v", err)
	}
	s := &APIServer{
		logger:        logrus.New(),
		router:        mux.NewRouter(),
		inflightSem:   make(chan struct{}, 4),
		fleetTopology: topo,
	}
	s.fleetHealth = health.NewAggregator(topo, health.NewFleetClient(), health.Probers{
		HTTP:  health.NewHTTPProber(false),
		TCP:   health.TCPProber{},
		Redis: health.RedisProber{},
	}, time.Hour /* never auto-refresh */, "testhost")
	// Seed an initial refresh in-band so Get() returns real data.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.fleetHealth.Refresh(ctx)
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.registerHealthEndpoints(api)
	s.registerPlatformHealthCompat()
	return s
}

func TestFleetHealth_compactRollup(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)
	req := httptest.NewRequest("GET", "/api/v1/health/fleet", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Data struct {
			Capabilities []map[string]any `json:"capabilities"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if !body.Success {
		t.Fatal("success=false")
	}
	if len(body.Data.Capabilities) != 1 {
		t.Fatalf("capabilities len=%d body=%s", len(body.Data.Capabilities), rr.Body.String())
	}
}

func TestFleetHealth_rolesEndpoint(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)
	req := httptest.NewRequest("GET", "/api/v1/health/roles", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	var body struct {
		Data struct {
			Roles []map[string]any `json:"roles"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data.Roles) == 0 {
		t.Fatal("no roles")
	}
}

func TestFleetHealth_topologyEndpoint(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)
	req := httptest.NewRequest("GET", "/api/v1/health/topology", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "test-cap") {
		t.Fatalf("topology body missing capability: %s", rr.Body.String())
	}
}

func TestFleetHealth_503WhenTopologyMissing(t *testing.T) {
	s := &APIServer{
		logger:      logrus.New(),
		router:      mux.NewRouter(),
		inflightSem: make(chan struct{}, 4),
		// fleetHealth + fleetTopology both nil
	}
	api := s.router.PathPrefix("/api/v1").Subrouter()
	s.registerHealthEndpoints(api)

	for _, path := range []string{
		"/api/v1/health/fleet",
		"/api/v1/health/roles",
		"/api/v1/health/instances",
		"/api/v1/health/topology",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rr := httptest.NewRecorder()
		s.router.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d want 503", path, rr.Code)
		}
	}
}

func TestPlatformHealthReady_compatShape(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)
	req := httptest.NewRequest("GET", "/platform/health/ready", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		OK   bool            `json:"ok"`
		Deps map[string]bool `json:"deps"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	for _, key := range []string{"postgres", "redis", "sidecar"} {
		if _, ok := body.Deps[key]; !ok {
			t.Fatalf("deps missing %q: %+v", key, body.Deps)
		}
	}
}

func TestPlatformHealthLive_alwaysOK(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)
	req := httptest.NewRequest("GET", "/platform/health/live", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"ok":true`) {
		t.Fatalf("body=%s", rr.Body.String())
	}
}

func TestFleetHealth_cacheBust(t *testing.T) {
	srv := newTestAPIServerWithHealth(t, miniTopology)
	req := httptest.NewRequest("DELETE", "/api/v1/health/cache", nil)
	rr := httptest.NewRecorder()
	srv.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// miniTopology is a small, valid topology used by the endpoint tests.
// Uses tcp roles pointed at port 1 so probes deterministically fail (no
// listener), which is fine — we're testing the HTTP surface, not probe
// truth. The /platform/health/ready compat shim is tolerant of all-bad
// roles and reports back the same shape regardless.
const miniTopology = `
roles:
  postgres:
    kind: postgres
    min_healthy: 1
  redis:
    kind: redis
    probe: { port: 1, timeout: 100ms }
    min_healthy: 1
  bge-embed:
    kind: http
    probe: { port: 1, path: /healthz, timeout: 100ms }
    min_healthy: 1
capabilities:
  test-cap:
    label: Test
    roles: [postgres, redis, bge-embed]
`

package health

import (
	"strings"
	"testing"
	"time"
)

func TestLoadTopology_minimalValid(t *testing.T) {
	body := `
roles:
  r2g:
    kind: http
    probe: { port: 7373, path: /health, timeout: 2s }
    min_healthy: 1
  postgres:
    kind: postgres
    min_healthy: 1
capabilities:
  data-plane:
    label: "Data plane"
    roles: [postgres]
  rag:
    label: "RAG plane"
    roles: [r2g, postgres]
`
	got, err := LoadTopology(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}
	if len(got.Roles) != 2 {
		t.Fatalf("want 2 roles, got %d", len(got.Roles))
	}
	if got.Roles["r2g"].Probe.Timeout != 2*time.Second {
		t.Fatalf("timeout mismatch: %v", got.Roles["r2g"].Probe.Timeout)
	}
	if got.Roles["r2g"].Name != "r2g" {
		t.Fatalf("role name not back-populated: %q", got.Roles["r2g"].Name)
	}
	if got.Capabilities["rag"].Name != "rag" {
		t.Fatalf("capability name not back-populated: %q", got.Capabilities["rag"].Name)
	}
	// Order is deterministic + sorted.
	wantOrder := []string{"postgres", "r2g"}
	for i, name := range wantOrder {
		if got.RoleOrder[i] != name {
			t.Fatalf("RoleOrder[%d]=%q want %q", i, got.RoleOrder[i], name)
		}
	}
}

func TestLoadTopology_temporalQueue(t *testing.T) {
	body := `
roles:
  skills-worker:
    kind: temporal_queue
    queue: skills
    min_healthy: 1
    heartbeat_max_age: 90s
capabilities:
  ops:
    label: Ops
    roles: [skills-worker]
`
	got, err := LoadTopology(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}
	r := got.Roles["skills-worker"]
	if r.Queue != "skills" {
		t.Fatalf("queue=%q", r.Queue)
	}
	if r.HeartbeatMaxAge != 90*time.Second {
		t.Fatalf("heartbeat_max_age=%v", r.HeartbeatMaxAge)
	}
}

func TestLoadTopology_rejectsUnknownKind(t *testing.T) {
	body := `
roles:
  weird:
    kind: lolwhat
    min_healthy: 1
capabilities:
  any:
    label: Any
    roles: [weird]
`
	_, err := LoadTopology(strings.NewReader(body))
	if err == nil {
		t.Fatal("want error on unknown kind, got nil")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Fatalf("error not about kind: %v", err)
	}
}

func TestLoadTopology_rejectsDanglingCapability(t *testing.T) {
	body := `
roles:
  r2g:
    kind: http
    probe: { port: 7373, path: /health, timeout: 2s }
    min_healthy: 1
capabilities:
  ghost:
    label: Ghost
    roles: [doesnt-exist]
`
	_, err := LoadTopology(strings.NewReader(body))
	if err == nil {
		t.Fatal("want error on dangling capability role, got nil")
	}
	if !strings.Contains(err.Error(), "undefined role") {
		t.Fatalf("error not about undefined role: %v", err)
	}
}

func TestLoadTopology_rejectsHTTPWithoutPath(t *testing.T) {
	body := `
roles:
  bad:
    kind: http
    probe: { port: 7373, timeout: 2s }
    min_healthy: 1
capabilities:
  any:
    label: Any
    roles: [bad]
`
	_, err := LoadTopology(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "needs path") {
		t.Fatalf("want error about missing path, got %v", err)
	}
}

func TestLoadTopology_rejectsTemporalQueueWithoutQueue(t *testing.T) {
	body := `
roles:
  bad:
    kind: temporal_queue
    min_healthy: 1
    heartbeat_max_age: 90s
capabilities:
  any:
    label: Any
    roles: [bad]
`
	_, err := LoadTopology(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "queue name") {
		t.Fatalf("want error about missing queue, got %v", err)
	}
}

func TestLoadTopology_rejectsNegativeMinHealthy(t *testing.T) {
	body := `
roles:
  bad:
    kind: tcp
    probe: { port: 6379, timeout: 2s }
    min_healthy: -1
capabilities:
  any:
    label: Any
    roles: [bad]
`
	_, err := LoadTopology(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "min_healthy") {
		t.Fatalf("want error about min_healthy, got %v", err)
	}
}

func TestLoadTopology_rejectsEmptyRoles(t *testing.T) {
	body := `capabilities: { rag: { label: x, roles: [r] } }`
	_, err := LoadTopology(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "no roles") {
		t.Fatalf("want error about empty roles, got %v", err)
	}
}

func TestLoadTopology_optionalRoleMinZero(t *testing.T) {
	body := `
roles:
  vllm:
    kind: http
    probe: { port: 9080, path: /v1/models, timeout: 2s }
    min_healthy: 0
    optional: true
capabilities:
  llm:
    label: LLM
    roles: [vllm]
`
	got, err := LoadTopology(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}
	if !got.Roles["vllm"].Optional {
		t.Fatal("optional flag not preserved")
	}
}

func TestTopology_TemporalQueues(t *testing.T) {
	body := `
roles:
  skills-worker:
    kind: temporal_queue
    queue: skills
    min_healthy: 1
    heartbeat_max_age: 90s
  skills-worker-b:
    kind: temporal_queue
    queue: skills
    min_healthy: 0
    heartbeat_max_age: 90s
  dispatch-worker:
    kind: temporal_queue
    queue: dispatch
    min_healthy: 1
    heartbeat_max_age: 90s
  r2g:
    kind: http
    min_healthy: 1
    probe: {port: 7373, path: /healthz}
capabilities:
  ops:
    label: Ops
    roles: [skills-worker]
`
	topo, err := LoadTopology(strings.NewReader(body))
	if err != nil {
		t.Fatalf("LoadTopology: %v", err)
	}
	got := topo.TemporalQueues()
	want := []string{"dispatch", "skills"}
	if len(got) != len(want) {
		t.Fatalf("TemporalQueues() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TemporalQueues()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

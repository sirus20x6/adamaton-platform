package health

import (
	"strings"
	"testing"
)

func TestComputeRoleStatus(t *testing.T) {
	cases := []struct {
		name    string
		role    Role
		running int
		healthy int
		want    Status
	}{
		{"all healthy meets quorum", Role{MinHealthy: 1}, 1, 1, StatusOK},
		{"two healthy, one missing", Role{MinHealthy: 1}, 2, 1, StatusDegraded},
		{"none running, quorum 0", Role{MinHealthy: 0}, 0, 0, StatusOK},
		{"none running, quorum 1", Role{MinHealthy: 1}, 0, 0, StatusOffline},
		{"running but none healthy", Role{MinHealthy: 1}, 2, 0, StatusOffline},
		{"running, one healthy, quorum 2", Role{MinHealthy: 2}, 2, 1, StatusDegraded},
		{"all healthy, quorum 2", Role{MinHealthy: 2}, 2, 2, StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeRoleStatus(tc.role, tc.running, tc.healthy)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestWorseStatus(t *testing.T) {
	if worseStatus(StatusOK, StatusOffline) != StatusOffline {
		t.Fatal("offline > ok")
	}
	if worseStatus(StatusDegraded, StatusUnknown) != StatusDegraded {
		t.Fatal("degraded > unknown")
	}
	if worseStatus(StatusOK, StatusOK) != StatusOK {
		t.Fatal("ok = ok")
	}
}

func TestRollupCapabilities_optionalOfflineDoesntDrag(t *testing.T) {
	topo := mustParse(t, `
roles:
  vllm:
    kind: http
    probe: { port: 9080, path: /v1/models, timeout: 2s }
    min_healthy: 0
    optional: true
  r2g:
    kind: http
    probe: { port: 7373, path: /health, timeout: 2s }
    min_healthy: 1
capabilities:
  rag:
    label: RAG
    roles: [r2g, vllm]
`)
	a := NewAggregator(topo, NewFleetClient(), Probers{}, 0, "workstation")
	roles := []RoleStatus{
		{Name: "r2g", Status: StatusOK, MinHealthy: 1, Running: 1, Healthy: 1},
		{Name: "vllm", Status: StatusOffline, Optional: true, MinHealthy: 0, Running: 0, Healthy: 0},
	}
	caps := a.rollupCapabilities(roles)
	if caps[0].Status != StatusOK {
		t.Fatalf("optional offline dragged: %q (down=%v)", caps[0].Status, caps[0].Down)
	}
}

func TestRollupCapabilities_worstOfWins(t *testing.T) {
	topo := mustParse(t, `
roles:
  a:
    kind: tcp
    probe: { port: 1, timeout: 1s }
    min_healthy: 1
  b:
    kind: tcp
    probe: { port: 1, timeout: 1s }
    min_healthy: 1
capabilities:
  bundle:
    label: Bundle
    roles: [a, b]
`)
	a := NewAggregator(topo, NewFleetClient(), Probers{}, 0, "workstation")
	roles := []RoleStatus{
		{Name: "a", Status: StatusDegraded, MinHealthy: 1, Running: 1, Healthy: 0},
		{Name: "b", Status: StatusOffline, MinHealthy: 1, Running: 0, Healthy: 0},
	}
	caps := a.rollupCapabilities(roles)
	if caps[0].Status != StatusOffline {
		t.Fatalf("worst-of failed: %q", caps[0].Status)
	}
	if len(caps[0].Down) != 2 {
		t.Fatalf("down list incomplete: %v", caps[0].Down)
	}
}

func TestParseAgentURLs(t *testing.T) {
	raw := "pi5=http://deploy-agent:9128,blackwell=http://10.0.4.37:9128,bad,host=,pi5-speaker=http://10.0.4.20:9128/"
	got := parseAgentURLs(raw)
	if got["pi5"] != "http://deploy-agent:9128" {
		t.Fatalf("pi5=%q", got["pi5"])
	}
	if got["blackwell"] != "http://10.0.4.37:9128" {
		t.Fatalf("blackwell=%q", got["blackwell"])
	}
	if got["pi5-speaker"] != "http://10.0.4.20:9128" {
		t.Fatalf("pi5-speaker=%q (trailing slash should be stripped)", got["pi5-speaker"])
	}
	if _, ok := got["bad"]; ok {
		t.Fatal("invalid token slipped through")
	}
	if _, ok := got["host"]; ok {
		t.Fatal("empty url accepted")
	}
}

func TestParseComposeStatus_singleObject(t *testing.T) {
	body := `{"Name":"deepresearch-r2g-1","Image":"adamaton-r2g:sha","State":"running","Health":"healthy","Service":"r2g"}`
	got, err := parseComposeStatus(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	if got[0].Name != "deepresearch-r2g-1" || got[0].State != "running" {
		t.Fatalf("got %+v", got[0])
	}
}

func TestParseComposeStatus_array(t *testing.T) {
	body := `[
		{"Name":"r2g-1","State":"running"},
		{"Name":"r2g-2","State":"exited"}
	]`
	got, err := parseComposeStatus(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d", len(got))
	}
}

func TestParseComposeStatus_newlineDelimited(t *testing.T) {
	body := `{"Name":"r2g-1"}` + "\n" + `{"Name":"r2g-2"}` + "\n"
	got, err := parseComposeStatus(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d (got %+v)", len(got), got)
	}
}

func mustParse(t *testing.T, body string) *Topology {
	t.Helper()
	topo, err := LoadTopology(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return topo
}

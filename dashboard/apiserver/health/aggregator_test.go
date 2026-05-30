package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// fakeWorkflowSource is an in-memory WorkflowFailureSource for the gauge
// tests — it returns canned counts (or a canned error) without a DB.
type fakeWorkflowSource struct {
	counts WorkflowFailureCounts
	err    error
}

func (f fakeWorkflowSource) WorkflowFailureCounts(context.Context) (WorkflowFailureCounts, error) {
	return f.counts, f.err
}

func TestDeriveWorkflowHealth_statusLadder(t *testing.T) {
	cases := []struct {
		name     string
		counts   WorkflowFailureCounts
		wantStat Status
		wantRate float64
	}{
		{
			name:     "no failures -> ok",
			counts:   WorkflowFailureCounts{FailedLastHour: 0, CompletedLastHour: 8},
			wantStat: StatusOK,
			wantRate: 0,
		},
		{
			name:     "some failures below threshold -> degraded",
			counts:   WorkflowFailureCounts{FailedLastHour: 3, CompletedLastHour: 1},
			wantStat: StatusDegraded,
			wantRate: 0.75,
		},
		{
			name:     "at storm threshold -> offline",
			counts:   WorkflowFailureCounts{FailedLastHour: FailureStormThreshold, CompletedLastHour: 0},
			wantStat: StatusOffline,
			wantRate: 1,
		},
		{
			name:     "above storm threshold -> offline",
			counts:   WorkflowFailureCounts{FailedLastHour: 25, CompletedLastHour: 5},
			wantStat: StatusOffline,
			wantRate: 25.0 / 30.0,
		},
		{
			name:     "empty window -> ok, rate 0 (no divide-by-zero)",
			counts:   WorkflowFailureCounts{},
			wantStat: StatusOK,
			wantRate: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := deriveWorkflowHealth(tc.counts)
			if wf.Status != tc.wantStat {
				t.Fatalf("status = %q, want %q", wf.Status, tc.wantStat)
			}
			if wf.FailedLastHour != tc.counts.FailedLastHour {
				t.Fatalf("FailedLastHour = %d, want %d", wf.FailedLastHour, tc.counts.FailedLastHour)
			}
			if diff := wf.FailureRate1h - tc.wantRate; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("FailureRate1h = %v, want %v", wf.FailureRate1h, tc.wantRate)
			}
			if wf.Detail == "" {
				t.Fatal("Detail should never be empty")
			}
		})
	}
}

// TestAggregator_workflowGauge_inSnapshot proves the gauge flows through a
// refresh into the published snapshot when a source is wired, and stays
// nil when it isn't.
func TestAggregator_workflowGauge_inSnapshot(t *testing.T) {
	topo := mustParse(t, `
roles:
  r2g:
    kind: http
    probe: { port: 7373, path: /health, timeout: 100ms }
    min_healthy: 0
    optional: true
capabilities:
  rag:
    label: RAG
    roles: [r2g]
`)

	// No source wired -> snapshot omits the gauge.
	bare := NewAggregator(topo, NewFleetClient(), Probers{}, time.Hour, "testhost")
	bare.Refresh(context.Background())
	if got := bare.Get().Workflows; got != nil {
		t.Fatalf("expected nil Workflows without a source, got %+v", got)
	}

	// Source wired with a failure storm -> gauge present + offline.
	a := NewAggregator(topo, NewFleetClient(), Probers{}, time.Hour, "testhost")
	a.SetWorkflowSource(fakeWorkflowSource{counts: WorkflowFailureCounts{
		FailedLastHour: 12, CompletedLastHour: 3, RunningNow: 2, FailedLast24h: 40,
	}})
	a.Refresh(context.Background())
	wf := a.Get().Workflows
	if wf == nil {
		t.Fatal("expected Workflows gauge in snapshot")
	}
	if wf.Status != StatusOffline {
		t.Fatalf("storm should be offline, got %q", wf.Status)
	}
	if wf.FailedLastHour != 12 || wf.RunningNow != 2 || wf.FailedLast24h != 40 {
		t.Fatalf("counts not propagated: %+v", wf)
	}
}

// TestAggregator_workflowGauge_sourceError keeps the snapshot publishing
// (with an "unknown" advisory gauge) when the source query fails.
func TestAggregator_workflowGauge_sourceError(t *testing.T) {
	topo := mustParse(t, `
roles:
  r2g:
    kind: http
    probe: { port: 7373, path: /health, timeout: 100ms }
    min_healthy: 0
    optional: true
capabilities:
  rag:
    label: RAG
    roles: [r2g]
`)
	a := NewAggregator(topo, NewFleetClient(), Probers{}, time.Hour, "testhost")
	a.SetWorkflowSource(fakeWorkflowSource{err: errors.New("boom: relation does not exist")})
	a.Refresh(context.Background())
	wf := a.Get().Workflows
	if wf == nil {
		t.Fatal("expected an advisory gauge even on source error")
	}
	if wf.Status != StatusUnknown {
		t.Fatalf("source error should yield unknown, got %q", wf.Status)
	}
	if wf.Error == "" {
		t.Fatal("source error should be surfaced in Error")
	}
	// The rest of the snapshot must still be present.
	if len(a.Get().Roles) == 0 {
		t.Fatal("roles rollup should still publish alongside a failed workflow gauge")
	}
}

func TestDiscoverInstances_synthesisCoversInfraRoles(t *testing.T) {
	// Regression: redis (kind:tcp) + temporal (kind:tcp) + postgres
	// must all be synthesized as cluster-singleton instances even when
	// they don't appear in any host's deploy-agent /services list,
	// because infra services aren't in MANIFEST.yaml.services.
	topo := mustParse(t, `
roles:
  postgres:
    kind: postgres
    min_healthy: 1
  redis:
    kind: tcp
    probe: { port: 6379, timeout: 100ms }
    min_healthy: 1
  temporal:
    kind: tcp
    probe: { port: 7233, timeout: 100ms }
    min_healthy: 1
  r2g:
    kind: http
    probe: { port: 7373, path: /health, timeout: 100ms }
    min_healthy: 1
capabilities:
  data:
    label: Data
    roles: [postgres, redis, temporal]
  rag:
    label: RAG
    roles: [r2g]
`)
	// No-fleet path (FleetClient with empty ADAMATON_DEPLOY_AGENTS).
	// We want every declared role synthesized.
	a := NewAggregator(topo, NewFleetClient(), Probers{}, time.Hour, "testhost")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	insts := a.discoverInstances(ctx)
	roles := map[string]bool{}
	for _, inst := range insts {
		roles[inst.Role] = true
	}
	for _, want := range []string{"postgres", "redis", "temporal", "r2g"} {
		if !roles[want] {
			t.Fatalf("role %q not synthesized (got %v)", want, roles)
		}
	}
}

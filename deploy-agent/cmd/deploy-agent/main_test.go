package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidTag(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"sha-abc1234", true},
		{"main", true},
		{"v1.2.3", true},
		{"abc_DEF", true},
		{"", false},
		{"foo bar", false},
		{"$(rm -rf /)", false},
		{"a;b", false},
		{strings.Repeat("a", 129), false},
		{strings.Repeat("a", 128), true},
	}
	for _, c := range cases {
		got := validTag.MatchString(c.tag)
		if got != c.want {
			t.Errorf("validTag(%q) = %v, want %v", c.tag, got, c.want)
		}
	}
}

func TestValidService(t *testing.T) {
	for _, c := range []struct {
		svc  string
		want bool
	}{
		{"dashboard", true},
		{"nano-research-worker", true},
		{"deploy_agent", true},
		{"", false},
		{"; rm -rf /", false},
		{"foo bar", false},
	} {
		got := validService.MatchString(c.svc)
		if got != c.want {
			t.Errorf("validService(%q) = %v, want %v", c.svc, got, c.want)
		}
	}
}

func TestTagEnvKey(t *testing.T) {
	cases := map[string]string{
		"dashboard":            "ADAMATON_DASHBOARD_TAG",
		"nano-research-worker": "ADAMATON_NANO_RESEARCH_WORKER_TAG",
		"skills-rae-worker":    "ADAMATON_SKILLS_RAE_WORKER_TAG",
		// adamaton- prefix is stripped so the unified worker service
		// "adamaton-worker" maps to ADAMATON_WORKER_TAG, not the
		// doubled-up ADAMATON_ADAMATON_WORKER_TAG.
		"adamaton-worker": "ADAMATON_WORKER_TAG",
	}
	for svc, want := range cases {
		if got := tagEnvKey(svc); got != want {
			t.Errorf("tagEnvKey(%q) = %q, want %q", svc, got, want)
		}
	}
}

func TestUpsertTagCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-tags.env")
	if err := upsertTag(path, "dashboard", "sha-abc"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "ADAMATON_DASHBOARD_TAG=sha-abc\n" {
		t.Errorf("unexpected file contents: %q", string(b))
	}
}

func TestUpsertTagReplacesLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-tags.env")
	initial := "ADAMATON_DASHBOARD_TAG=main\nADAMATON_FRONTEND_TAG=main\nOTHER_VAR=keep\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertTag(path, "dashboard", "sha-new"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.Contains(got, "ADAMATON_DASHBOARD_TAG=sha-new\n") {
		t.Errorf("dashboard line not updated: %q", got)
	}
	if !strings.Contains(got, "ADAMATON_FRONTEND_TAG=main\n") {
		t.Errorf("frontend line clobbered: %q", got)
	}
	if !strings.Contains(got, "OTHER_VAR=keep\n") {
		t.Errorf("other var clobbered: %q", got)
	}
	if strings.Count(got, "ADAMATON_DASHBOARD_TAG=") != 1 {
		t.Errorf("duplicated dashboard line: %q", got)
	}
}

func TestUpsertTagAppendsNewService(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-tags.env")
	initial := "ADAMATON_DASHBOARD_TAG=main\n"
	_ = os.WriteFile(path, []byte(initial), 0o644)
	if err := upsertTag(path, "frontend", "sha-xyz"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "ADAMATON_FRONTEND_TAG=sha-xyz\n") {
		t.Errorf("frontend not appended: %q", string(b))
	}
}

func TestLoadManifest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST.yaml")
	body := `host: pi5
image_tag: main
services:
  - dashboard
  - skills-rae-worker
`
	_ = os.WriteFile(path, []byte(body), 0o644)
	m, err := loadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Host != "pi5" {
		t.Errorf("host = %q, want pi5", m.Host)
	}
	if len(m.Services) != 2 {
		t.Errorf("got %d services, want 2", len(m.Services))
	}
}

func TestLoadManifestRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST.yaml")
	_ = os.WriteFile(path, []byte("host: pi5\nservices: []\n"), 0o644)
	if _, err := loadManifest(path); err == nil {
		t.Errorf("expected error for empty services list")
	}
}

func TestTail(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	if got := tail(in, 2); got != "d\ne\n" {
		t.Errorf("tail(2) = %q, want %q", got, "d\ne\n")
	}
	if got := tail(in, 10); got != in {
		t.Errorf("tail(10) = %q, want %q", got, in)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"adamaton-dashboard:sha-abc1234":                "sha-abc1234",
		"registry.example/adamaton-dashboard:main":      "main",
		"registry:5000/adamaton-dashboard:sha-deadbeef": "sha-deadbeef",
		"registry:5000/adamaton-dashboard":              "", // no tag, only host:port colon
		"adamaton-dashboard":                            "", // bare name, no tag
		"adamaton-dashboard:v1.2.3@sha256:abcdef":       "v1.2.3",
		"": "",
	}
	for img, want := range cases {
		if got := imageTag(img); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", img, got, want)
		}
	}
}

func TestParseComposePS(t *testing.T) {
	// NDJSON (one object per line) — the modern compose default.
	ndjson := `{"Name":"adamaton-dashboard-1","Service":"dashboard","Image":"reg/adamaton-dashboard:sha-new","State":"running"}
{"Name":"adamaton-worker-1","Service":"adamaton-worker","Image":"reg/adamaton-worker:main","State":"running"}`
	entries, err := parseComposePS([]byte(ndjson))
	if err != nil {
		t.Fatalf("parseComposePS NDJSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Service != "dashboard" || entries[0].Image != "reg/adamaton-dashboard:sha-new" {
		t.Errorf("unexpected first entry: %+v", entries[0])
	}

	// JSON array shape (older compose).
	arr := `[{"Name":"c1","Service":"dashboard","Image":"reg/adamaton-dashboard:sha-x","State":"running"}]`
	entries, err = parseComposePS([]byte(arr))
	if err != nil {
		t.Fatalf("parseComposePS array: %v", err)
	}
	if len(entries) != 1 || entries[0].Image != "reg/adamaton-dashboard:sha-x" {
		t.Errorf("array parse wrong: %+v", entries)
	}

	// Single object shape.
	single := `{"Name":"c1","Service":"dashboard","Image":"reg/adamaton-dashboard:sha-y","State":"running"}`
	entries, err = parseComposePS([]byte(single))
	if err != nil {
		t.Fatalf("parseComposePS single: %v", err)
	}
	if len(entries) != 1 || entries[0].Image != "reg/adamaton-dashboard:sha-y" {
		t.Errorf("single parse wrong: %+v", entries)
	}

	// Empty output -> no entries, no error.
	entries, err = parseComposePS([]byte("  \n"))
	if err != nil {
		t.Fatalf("parseComposePS empty: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("empty should yield 0 entries, got %d", len(entries))
	}

	// Malformed JSON -> error.
	if _, err := parseComposePS([]byte("{not json")); err == nil {
		t.Errorf("expected error on malformed JSON")
	}
}

func TestMetricsPath(t *testing.T) {
	cases := map[string]string{
		"/metrics":                 "/metrics",
		"/restart":                 "/restart",
		"/status":                  "/status",
		"/projects":                "/projects/*",
		"/projects/abc/terminal":   "/projects/*",
		"/some-unknown-probe-path": "other",
	}
	for path, want := range cases {
		r := httptest.NewRequest("GET", path, nil)
		if got := metricsPath(r); got != want {
			t.Errorf("metricsPath(%q) = %q, want %q", path, got, want)
		}
	}
}

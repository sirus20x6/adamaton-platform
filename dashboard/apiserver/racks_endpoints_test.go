package apiserver

import (
	"testing"
)

func TestLoadRackManifests(t *testing.T) {
	manifests, err := loadRackManifests()
	if err != nil {
		t.Fatalf("loadRackManifests: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatalf("expected at least one rack manifest")
	}
	want := map[string]bool{"pi5": false, "pi5-speaker": false}
	for _, m := range manifests {
		if _, ok := want[m.Host]; ok {
			want[m.Host] = true
		}
	}
	for h, found := range want {
		if !found {
			t.Errorf("rack manifest missing host: %s", h)
		}
	}
}

func TestMatchWorkerByAliasSuffix(t *testing.T) {
	workers := []Worker{
		{ID: "nano-research-worker@pi5", Identity: "nano-research-worker"},
		{ID: "nano-research-worker@pi5-speaker", Identity: "nano-research-worker"},
		{ID: "skills-worker@pi", Identity: "skills-worker"},
		{ID: "evo-worker@blackwell", Identity: "evo-worker"},
	}
	cases := []struct {
		name    string
		svc     string
		aliases []string
		wantID  string // "" means nil expected
	}{
		{"pi5 nano", "nano-research-worker", []string{"pi5", "pi"}, "nano-research-worker@pi5"},
		{"pi5-speaker nano", "nano-research-worker", []string{"pi5-speaker"}, "nano-research-worker@pi5-speaker"},
		{"pi5 skills via legacy alias", "skills-worker", []string{"pi5", "pi"}, "skills-worker@pi"},
		{"blackwell evo", "evo-worker", []string{"blackwell"}, "evo-worker@blackwell"},
		{"no match — different host", "nano-research-worker", []string{"workstation"}, ""},
		{"no match — different identity", "skills-rae-worker", []string{"pi"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := matchWorker(c.svc, c.aliases, workers)
			if c.wantID == "" {
				if got != nil {
					t.Fatalf("expected nil match, got %s", got.ID)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected match %s, got nil", c.wantID)
			}
			if got.ID != c.wantID {
				t.Errorf("expected match ID %s, got %s", c.wantID, got.ID)
			}
		})
	}
}

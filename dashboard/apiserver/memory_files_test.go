// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	raw := []byte("---\nname: my-note\ndescription: A short one\nmetadata:\n  type: project\n---\n\nBody goes here.\n")
	fm, body := parseFrontmatter(raw)
	if !fm.present {
		t.Fatalf("expected frontmatter present")
	}
	if fm.Name != "my-note" {
		t.Errorf("name: got %q want my-note", fm.Name)
	}
	if fm.Description != "A short one" {
		t.Errorf("description: got %q", fm.Description)
	}
	if fm.Type != "project" {
		t.Errorf("type pulled from metadata: got %q want project", fm.Type)
	}
	if !strings.HasPrefix(body, "\nBody goes here.") {
		t.Errorf("body lost leading content: %q", body)
	}
}

func TestParseFrontmatter_NoMatter(t *testing.T) {
	raw := []byte("# Hello\n\nNo frontmatter here.\n")
	fm, body := parseFrontmatter(raw)
	if fm.present {
		t.Errorf("did not expect frontmatter to be marked present")
	}
	if body != string(raw) {
		t.Errorf("body should equal raw when no frontmatter")
	}
}

func TestRenderFrontmatter_RoundTrip(t *testing.T) {
	in := frontmatter{
		Name:        "round-trip",
		Description: "round-trip test",
		Type:        "user",
		Metadata:    map[string]interface{}{"originSessionId": "abc"},
		present:     true,
	}
	out := renderFrontmatter(in)
	if !strings.HasPrefix(out, "---\n") || !strings.HasSuffix(out, "---\n") {
		t.Errorf("missing matter delimiters: %q", out)
	}
	fm, _ := parseFrontmatter([]byte(out + "body\n"))
	if fm.Name != "round-trip" || fm.Description != "round-trip test" || fm.Type != "user" {
		t.Errorf("round-trip lost fields: %+v", fm)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Hello World":          "hello-world",
		"Codex Sticky Notes!":  "codex-sticky-notes",
		"  spaces   matter ":   "spaces-matter",
		"under_score-and dash": "under-score-and-dash",
		"":                     "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeJoin_Traversal(t *testing.T) {
	root := t.TempDir()
	for _, bad := range []string{
		encodeMemoryID("../escape.md"),
		encodeMemoryID("/etc/passwd"),
		"",
		encodeMemoryID("subdir/../../escape.md"),
	} {
		if _, err := safeJoin(root, bad); err == nil {
			t.Errorf("expected rejection for %q", bad)
		}
	}
}

func TestSafeJoin_Symlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "evil.md")
	if err := os.Symlink(filepath.Join(outside, "x.md"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeJoin(root, encodeMemoryID("evil.md")); err == nil {
		t.Error("expected symlink escape to be rejected")
	}
}

func TestUpdateMemoryIndex_AddAndRemove(t *testing.T) {
	dir := t.TempDir()
	if err := updateMemoryIndex(dir, "alpha.md", "First note", true); err != nil {
		t.Fatalf("add: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if !strings.Contains(string(body), "[alpha](alpha.md) — First note") {
		t.Errorf("missing pointer line: %s", body)
	}
	if err := updateMemoryIndex(dir, "alpha.md", "First note", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if strings.Contains(string(body), "alpha.md") {
		t.Errorf("pointer line not removed: %s", body)
	}
}

func TestEncodeDecodeMemoryID(t *testing.T) {
	for _, p := range []string{"a.md", "scope/memory/file.md", "deep/nest/file.md"} {
		enc := encodeMemoryID(p)
		dec, err := decodeMemoryID(enc)
		if err != nil {
			t.Errorf("decode(%q): %v", enc, err)
		}
		if dec != p {
			t.Errorf("round-trip %q -> %q -> %q", p, enc, dec)
		}
		if strings.Contains(enc, "/") {
			t.Errorf("encoded id %q still contains /", enc)
		}
	}
}
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

func TestParseSkillMarkdownWithFrontmatter(t *testing.T) {
	raw := []byte(`---
name: extract-method
description: pull a block of code into a named function
tags: [refactor, any-language]
community: code-refactoring
when_to_use: when a function does more than its name implies
example: split a 30-line render() into render() + draw_overlay()
---

# Extract Method

When a function has accumulated too much responsibility, pull a
self-contained chunk of its body into a separate, named function.
`)
	got, err := parseSkillMarkdown(raw, "fallback", "file:///tmp/x.md", "manual")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Name != "extract-method" {
		t.Errorf("name: got %q", got.Name)
	}
	if got.Description == "" || !strings.Contains(got.Description, "pull a block") {
		t.Errorf("description: got %q", got.Description)
	}
	if got.WhenToUse == "" {
		t.Errorf("when_to_use empty")
	}
	if got.Example == "" {
		t.Errorf("example empty")
	}
	if got.Community != "code-refactoring" {
		t.Errorf("community: got %q", got.Community)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "refactor" {
		t.Errorf("tags: got %v", got.Tags)
	}
	if got.SourceSHA == "" {
		t.Errorf("source_sha empty")
	}
	if !strings.Contains(got.Body, "# Extract Method") {
		t.Errorf("body missing markdown heading: %q", got.Body)
	}
}

func TestParseSkillMarkdownWithoutFrontmatter(t *testing.T) {
	raw := []byte("# Hello\n\nA short skill description that becomes the description field.\n")
	got, err := parseSkillMarkdown(raw, "hello", "file:///tmp/hello.md", "manual")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Name != "hello" {
		t.Errorf("name: got %q", got.Name)
	}
	if !strings.Contains(got.Description, "becomes the description") {
		t.Errorf("description: got %q", got.Description)
	}
}

func TestParseSkillMarkdownMissingFields(t *testing.T) {
	cases := map[string][]byte{
		"empty file":              []byte(""),
		"frontmatter only":        []byte("---\nname: x\n---\n"),
		"no name and no fallback": []byte("body only with no fallback name"),
	}
	for label, raw := range cases {
		t.Run(label, func(t *testing.T) {
			fallback := ""
			if label == "no name and no fallback" {
				fallback = ""
			}
			if _, err := parseSkillMarkdown(raw, fallback, "x", "manual"); err == nil {
				t.Errorf("expected error for %q", label)
			}
		})
	}
}

func TestImportLocalDirWalksMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct {
		name, body string
	}{
		{"alpha.md", "---\nname: alpha\ndescription: the alpha skill\n---\n\nbody A.\n"},
		{"beta.md", "---\nname: beta\ndescription: the beta skill\n---\n\nbody B.\n"},
		{"ignored.txt", "not markdown"},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	results, err := importLocalDir(dir, "claude-files")
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("file %q: unexpected err: %v", r.Path, r.Err)
		}
		if r.Skill.Origin != "claude-files" {
			t.Errorf("origin not propagated: %q", r.Skill.Origin)
		}
		if !strings.HasPrefix(r.Skill.SourceURL, "file://") {
			t.Errorf("source_url wrong: %q", r.Skill.SourceURL)
		}
	}
}

func TestNormaliseGithubRawURL(t *testing.T) {
	cases := []struct {
		in, want string
		hasErr   bool
	}{
		{
			in:   "https://raw.githubusercontent.com/owner/repo/main/skills/x.md",
			want: "https://raw.githubusercontent.com/owner/repo/main/skills/x.md",
		},
		{
			in:   "https://github.com/owner/repo/blob/main/skills/x.md",
			want: "https://raw.githubusercontent.com/owner/repo/main/skills/x.md",
		},
		{
			in:   "https://gitlab.example.com/foo.md",
			want: "https://gitlab.example.com/foo.md",
		},
		{
			in:     "not a url at all",
			hasErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := normaliseGithubRawURL(c.in)
			if c.hasErr {
				if err == nil {
					t.Errorf("expected error for %q", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseGithubRepoURL(t *testing.T) {
	cases := []struct {
		in         string
		owner, rep string
		hasErr     bool
	}{
		{"https://github.com/foo/bar", "foo", "bar", false},
		{"https://github.com/foo/bar.git", "foo", "bar", false},
		{"https://github.com/foo/bar/tree/main/skills", "foo", "bar", false},
		{"https://gitlab.com/foo/bar", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			o, r, err := parseGithubRepoURL(c.in)
			if c.hasErr {
				if err == nil {
					t.Errorf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if o != c.owner || r != c.rep {
				t.Errorf("got %s/%s, want %s/%s", o, r, c.owner, c.rep)
			}
		})
	}
}

func TestMergeTagsDedupes(t *testing.T) {
	got := mergeTags([]string{"py", "ast"}, []string{"ast", "refactor", ""})
	want := []string{"py", "ast", "refactor"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %q want %q", i, got[i], want[i])
		}
	}
}
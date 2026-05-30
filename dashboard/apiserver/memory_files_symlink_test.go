// Hardening regression tests for safeJoin()'s symlink-traversal boundary.
// The old implementation only os.Readlink'd the FINAL path component and
// merely checked resolved != root, so (a) an intermediate directory
// symlink pointing outside root was never resolved, and (b) a leaf
// symlink pointing anywhere outside root that wasn't literally root
// slipped through. These tests pin the resolved-parent prefix check.
package apiserver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSafeJoin_LeafSymlinkOutsideRoot: a final-component symlink whose
// target is outside root (but isn't root) must be rejected.
func TestSafeJoin_LeafSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "leak.md")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeJoin(root, encodeMemoryID("leak.md")); err == nil {
		t.Error("leaf symlink pointing outside root must be rejected")
	}
}

// TestSafeJoin_IntermediateDirSymlinkOutsideRoot is the vector the card
// calls out: a DIRECTORY component in the path is a symlink that points
// outside root. The old final-component-only readlink never resolved it.
func TestSafeJoin_IntermediateDirSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// outside/realdir/secret.md exists on disk.
	realDir := filepath.Join(outside, "realdir")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "secret.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// root/escape -> outside/realdir (a directory symlink escaping root).
	if err := os.Symlink(realDir, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// id "escape/secret.md" is lexically under root, but escape/ resolves
	// outside — must be rejected.
	if _, err := safeJoin(root, encodeMemoryID("escape/secret.md")); err == nil {
		t.Error("path through an intermediate dir symlink escaping root must be rejected")
	}
}

// TestSafeJoin_SymlinkWithinRootAllowed: a symlink that stays inside root
// is fine — we must not over-reject legitimate internal links.
func TestSafeJoin_SymlinkWithinRootAllowed(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.md")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := safeJoin(root, encodeMemoryID("alias.md"))
	if err != nil {
		t.Errorf("symlink staying within root should be allowed, got error: %v", err)
	}
	if got == "" {
		t.Error("expected a resolved path for an in-root symlink")
	}
}

// TestSafeJoin_NonexistentLeafUnderRealDirAllowed: creating a NEW memory
// file (leaf doesn't exist yet) under a real, in-root directory must still
// succeed — the longest-existing-prefix resolver handles the missing tail.
func TestSafeJoin_NonexistentLeafUnderRealDirAllowed(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "memory")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(root, encodeMemoryID("memory/newnote.md")); err != nil {
		t.Errorf("new file under an in-root real dir should be allowed, got: %v", err)
	}
}

// TestSafeJoin_SiblingPrefixNotWithin: a sibling dir sharing a name prefix
// with root ("/tmp/rootX" vs "/tmp/root") must not be treated as within.
func TestSafeJoin_SiblingPrefixNotWithin(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sibling := filepath.Join(base, "root-evil")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "x.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink in root pointing at the sibling must be rejected even
	// though the sibling path has root's text as a byte prefix.
	if err := os.Symlink(filepath.Join(sibling, "x.md"), filepath.Join(root, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := safeJoin(root, encodeMemoryID("link.md")); err == nil {
		t.Error("symlink into a sibling dir sharing a name prefix must be rejected")
	}
}

// TestPathWithin exercises the separator-anchored prefix helper directly.
func TestPathWithin(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b/c", "/a/b", true},
		{"/a/bc", "/a/b", false},
		{"/a", "/a/b", false},
		{"/x", "/a/b", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.p, c.root); got != c.want {
			t.Errorf("pathWithin(%q,%q)=%v want %v", c.p, c.root, got, c.want)
		}
	}
}

// Package stage hands out writable paths under /var/lib/dr-uploads for
// plugins that share the volume with the host. The plugin writes the
// bytes directly; the host never sees them in memory. Path() does the
// filename sanitisation so plugin code can't escape the root.
package stage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Stager is bound to a single root. Caller usually passes
// PH_DR_UPLOADS_DIR.
type Stager struct{ root string }

// New does not stat root -- the dir may not exist yet on a fresh boot;
// Path() creates parents as needed.
func New(root string) *Stager { return &Stager{root: root} }

// Root returns the configured root directory; useful for tests + callers
// (e.g. hostserver) that need to compose plugin-specific subpaths
// without exporting the field.
func (s *Stager) Root() string { return s.root }

// PluginPath returns an absolute writable path scoped to a specific
// plugin: <root>/plugins/<pluginID>/<runID>/<safe_filename>. Same
// sanitisation rules as Path; pluginID is itself sanitised so plugin
// authors can't escape into a sibling plugin's tree.
func (s *Stager) PluginPath(pluginID, runID, filename, contentType string) (string, error) {
	if s.root == "" {
		return "", errors.New("stager root is empty")
	}
	cleanPlugin, err := sanitiseSegment(pluginID)
	if err != nil {
		return "", fmt.Errorf("plugin id: %w", err)
	}
	cleanRun, err := sanitiseSegment(runID)
	if err != nil {
		// run_id may be empty in tests / dev — fall back to a sentinel.
		cleanRun = "_no_run"
	}
	cleanFile, err := sanitiseFilename(filename)
	if err != nil {
		return "", err
	}
	_ = contentType // reserved; see godoc on Path
	dir := filepath.Join(s.root, "plugins", cleanPlugin, cleanRun)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir staging: %w", err)
	}
	return filepath.Join(dir, cleanFile), nil
}

// LegacyZoteroPath matches the pre-plugin layout where Zotero PDFs lived
// at /var/lib/dr-uploads/zotero/<key>.pdf. We keep emitting that exact
// shape for the zotero plugin so the ingest worker keeps finding files
// without a config change. filename is sanitised the usual way.
func (s *Stager) LegacyZoteroPath(filename string) (string, error) {
	if s.root == "" {
		return "", errors.New("stager root is empty")
	}
	clean, err := sanitiseFilename(filename)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, "zotero")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir zotero staging: %w", err)
	}
	return filepath.Join(dir, clean), nil
}

// sanitiseSegment is the same rules as sanitiseFilename but rejects
// empty input outright; used for path-segment identifiers (plugin id,
// run id) where there is no extension-preservation concern.
func sanitiseSegment(s string) (string, error) {
	if s == "" {
		return "", errors.New("segment is empty")
	}
	if strings.ContainsAny(s, `/\`) {
		return "", fmt.Errorf("segment %q contains path separator", s)
	}
	s = strings.TrimLeft(s, ".")
	if s == "" {
		return "", errors.New("segment was all dots")
	}
	if len(s) > 128 {
		s = s[:128]
	}
	return s, nil
}

// Path returns an absolute, writable path under root. runID becomes the
// containing dir so plugin_runs cleanup can remove an entire run's
// staged files atomically. contentType is hashed into a subdir prefix
// today only as a hint (no MIME-based routing yet) but the parameter is
// here so future MIME-aware sharding doesn't change the signature.
func (s *Stager) Path(runID, filename, contentType string) (string, error) {
	if s.root == "" {
		return "", errors.New("stager root is empty")
	}
	clean, err := sanitiseFilename(filename)
	if err != nil {
		return "", err
	}
	_ = contentType // reserved; see godoc
	dir := filepath.Join(s.root, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir staging: %w", err)
	}
	return filepath.Join(dir, clean), nil
}

// Write is the no-shared-volume fallback. Bytes arrive over gRPC, the
// host writes them itself. Mode 0644 matches what the manifest-shared
// path advertises so plugins don't observe perm differences.
func (s *Stager) Write(runID, filename, contentType string, body []byte) (string, error) {
	p, err := s.Path(runID, filename, contentType)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", p, err)
	}
	return p, nil
}

// sanitiseFilename strips separators + leading dots and caps length. We
// don't try to enforce a charset beyond that -- plugin filenames are
// already adversary-controlled and the FS handles whatever survives.
func sanitiseFilename(name string) (string, error) {
	if name == "" {
		return "", errors.New("filename is empty")
	}
	// Reject path separators outright; we never want a plugin to climb
	// out of the run dir even if MkdirAll would handle it.
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("filename %q contains path separator", name)
	}
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "", errors.New("filename was all dots")
	}
	if len(name) > 200 {
		// Preserve the trailing extension so MIME sniffing downstream
		// still works after truncation.
		ext := filepath.Ext(name)
		if len(ext) > 16 { // pathological extensions get clipped too
			ext = ext[:16]
		}
		name = name[:200-len(ext)] + ext
	}
	return name, nil
}

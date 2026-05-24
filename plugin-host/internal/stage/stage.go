// Package stage hands out writable paths under a container-local
// ephemeral directory (PH_STAGE_DIR, default /run/ph-stage) for plugins
// that share the host's filesystem. The plugin writes the bytes directly
// to that path; the host never sees them in memory while they're being
// written.
//
// The local path is only a staging buffer: once the file is known
// complete the host COMMITS it to the shared Garage / S3 blob store
// (bucket "dr-uploads") via CommitFile/CommitBytes and deletes the local
// copy. Readers (this host's compat routes, knowledge/r2g's ingest) pull
// the bytes back out of the blob store by object key — nothing downstream
// reads the local filesystem anymore. This is the "local stage + host
// commit" design that gets document blobs off the NFS-backed
// /var/lib/dr-uploads volume.
//
// Object keys mirror the on-disk layout so the migration is a 1:1 remap:
//
//	zotero PDFs           -> zotero/<key>.pdf            (preserved legacy layout)
//	general plugin files  -> plugin/<pluginID>/<runID>/<file>
//
// Path() does the filename sanitisation so plugin code can't escape the
// root, and the same sanitisation feeds the object-key builders so a
// hostile filename can't produce a surprising key.
package stage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirus20x6/adamaton-core/blobstore"
)

// Stager is bound to a single local root plus an optional blob store. The
// root is a container-local ephemeral dir (PH_STAGE_DIR); the store is the
// durable destination. A nil store means "no blob store configured" — the
// stager still hands out local paths but the commit methods fail soft with
// ErrNoBlobStore so the caller can surface a 503 rather than silently
// losing the blob.
type Stager struct {
	root  string
	blobs blobstore.Backend
}

// New does not stat root -- the dir may not exist yet on a fresh boot;
// Path() creates parents as needed. blobs may be nil (blob store not
// configured); CommitFile/CommitBytes/Get then return ErrNoBlobStore.
func New(root string, blobs blobstore.Backend) *Stager {
	return &Stager{root: root, blobs: blobs}
}

// ErrNoBlobStore is returned by the commit/get methods when the stager was
// built without a blob store (BLOBSTORE_ENDPOINT unset). Callers translate
// this into a 503 / FailedPrecondition so the operation fails soft rather
// than dropping the staged bytes on the floor.
var ErrNoBlobStore = errors.New("stage: no blob store configured")

// Root returns the configured local root directory; useful for tests +
// callers (e.g. hostserver) that need to compose plugin-specific subpaths
// without exporting the field.
func (s *Stager) Root() string { return s.root }

// HasBlobStore reports whether a durable blob store is wired. Handlers use
// it to fail soft (503) up front instead of staging bytes they can't commit.
func (s *Stager) HasBlobStore() bool { return s.blobs != nil }

// PluginPath returns an absolute writable local path scoped to a specific
// plugin: <root>/plugins/<pluginID>/<runID>/<safe_filename>. Same
// sanitisation rules as Path; pluginID is itself sanitised so plugin
// authors can't escape into a sibling plugin's tree.
func (s *Stager) PluginPath(pluginID, runID, filename, contentType string) (string, error) {
	if s.root == "" {
		return "", errors.New("stager root is empty")
	}
	cleanPlugin, cleanRun, cleanFile, err := s.pluginParts(pluginID, runID, filename)
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

// PluginKey returns the blob-store object key matching PluginPath:
// plugin/<pluginID>/<runID>/<safe_filename>. The local layout uses a
// "plugins/" dir; the durable key uses the singular "plugin/" prefix to
// match the contract knowledge/r2g shares (general plugin staging lives
// under a "plugin/<...>" prefix in the dr-uploads bucket).
func (s *Stager) PluginKey(pluginID, runID, filename string) (string, error) {
	cleanPlugin, cleanRun, cleanFile, err := s.pluginParts(pluginID, runID, filename)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{"plugin", cleanPlugin, cleanRun, cleanFile}, "/"), nil
}

// pluginParts runs the shared sanitisation for the plugin path + key
// builders so the local file and the object key always agree.
func (s *Stager) pluginParts(pluginID, runID, filename string) (string, string, string, error) {
	cleanPlugin, err := sanitiseSegment(pluginID)
	if err != nil {
		return "", "", "", fmt.Errorf("plugin id: %w", err)
	}
	cleanRun, err := sanitiseSegment(runID)
	if err != nil {
		// run_id may be empty in tests / dev — fall back to a sentinel.
		cleanRun = "_no_run"
	}
	cleanFile, err := sanitiseFilename(filename)
	if err != nil {
		return "", "", "", err
	}
	return cleanPlugin, cleanRun, cleanFile, nil
}

// LegacyZoteroPath matches the pre-plugin layout where Zotero PDFs lived
// at <root>/zotero/<key>.pdf. We keep emitting that exact shape so the
// zotero plugin writes its PDF where the host expects to find it for the
// commit step; the durable object key (see ZoteroKey) preserves the same
// zotero/<key>.pdf layout in the dr-uploads bucket. filename is sanitised
// the usual way.
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

// ZoteroKey returns the blob-store object key for a zotero PDF:
// zotero/<safe_filename>. Matches the legacy on-disk layout and the
// contract knowledge/r2g reads back (s3://dr-uploads/zotero/<key>.pdf).
func (s *Stager) ZoteroKey(filename string) (string, error) {
	clean, err := sanitiseFilename(filename)
	if err != nil {
		return "", err
	}
	return "zotero/" + clean, nil
}

// CommitFile uploads the bytes at localPath into the blob store under key,
// then deletes the local copy on success. The local file is the source of
// truth until the Put succeeds; a failed Put leaves it in place so the
// caller can retry or surface an error without data loss. A failed local
// cleanup after a successful Put is NOT fatal -- the durable copy is what
// matters. Returns ErrNoBlobStore when no store is configured.
func (s *Stager) CommitFile(ctx context.Context, localPath, key string) (blobstore.ObjectRef, error) {
	if s.blobs == nil {
		return blobstore.ObjectRef{}, ErrNoBlobStore
	}
	f, err := os.Open(localPath)
	if err != nil {
		return blobstore.ObjectRef{}, fmt.Errorf("open staged file %s: %w", localPath, err)
	}
	var size int64 = -1
	if fi, statErr := f.Stat(); statErr == nil {
		size = fi.Size()
	}
	ref, putErr := s.blobs.Put(ctx, key, f, size)
	_ = f.Close()
	if putErr != nil {
		return blobstore.ObjectRef{}, fmt.Errorf("put %q: %w", key, putErr)
	}
	// Best-effort local cleanup -- the durable copy is what matters now.
	if rmErr := os.Remove(localPath); rmErr != nil && !os.IsNotExist(rmErr) {
		// Not fatal: the bytes are durable. The caller may log; we return
		// the successful ref so the commit is still considered done.
		return ref, nil
	}
	return ref, nil
}

// CommitBytes uploads body directly into the blob store under key. Used by
// the no-shared-volume / host-writes-bytes paths where there is no local
// file to read back. Returns ErrNoBlobStore when no store is configured.
func (s *Stager) CommitBytes(ctx context.Context, key string, body []byte) (blobstore.ObjectRef, error) {
	if s.blobs == nil {
		return blobstore.ObjectRef{}, ErrNoBlobStore
	}
	ref, err := s.blobs.Put(ctx, key, strings.NewReader(string(body)), int64(len(body)))
	if err != nil {
		return blobstore.ObjectRef{}, fmt.Errorf("put %q: %w", key, err)
	}
	return ref, nil
}

// CommitReader streams r into the blob store under key WITHOUT touching
// any local file. Used to mirror a staged file into durable storage while
// keeping the on-disk copy for an in-container consumer (e.g. the zotero
// plugin reading sqlite_path on its next sync). Size is unknown so the
// upload always goes through the multipart manager. Returns ErrNoBlobStore
// when no store is configured.
func (s *Stager) CommitReader(ctx context.Context, key string, r io.Reader) (blobstore.ObjectRef, error) {
	if s.blobs == nil {
		return blobstore.ObjectRef{}, ErrNoBlobStore
	}
	ref, err := s.blobs.Put(ctx, key, r, -1)
	if err != nil {
		return blobstore.ObjectRef{}, fmt.Errorf("put %q: %w", key, err)
	}
	return ref, nil
}

// Get streams an object back out of the blob store by key. Callers must
// Close the returned reader. Returns ErrNoBlobStore when no store is
// configured and blobstore.ErrNotFound when the key is absent.
func (s *Stager) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.blobs == nil {
		return nil, ErrNoBlobStore
	}
	return s.blobs.Get(ctx, key)
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

// Path returns an absolute, writable local path under root. runID becomes
// the containing dir so plugin_runs cleanup can remove an entire run's
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

package stage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirus20x6/adamaton-core/blobstore"
)

// memStore is an in-memory blobstore.Backend for tests. Mirrors the shape
// knowledge/r2g's test fake uses so the contract stays recognisable.
type memStore struct {
	objs map[string][]byte
}

func newMemStore() *memStore { return &memStore{objs: map[string][]byte{}} }

func (m *memStore) EnsureBucket(context.Context) error { return nil }

func (m *memStore) Put(_ context.Context, key string, r io.Reader, _ int64) (blobstore.ObjectRef, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return blobstore.ObjectRef{}, err
	}
	m.objs[key] = b
	return blobstore.ObjectRef{Bucket: "dr-uploads", Key: key, Size: int64(len(b))}, nil
}

func (m *memStore) PutMultipart(ctx context.Context, key string, r io.Reader) (blobstore.ObjectRef, error) {
	return m.Put(ctx, key, r, -1)
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := m.objs[key]
	if !ok {
		return nil, blobstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) Stat(_ context.Context, key string) (blobstore.ObjectRef, error) {
	b, ok := m.objs[key]
	if !ok {
		return blobstore.ObjectRef{}, blobstore.ErrNotFound
	}
	return blobstore.ObjectRef{Bucket: "dr-uploads", Key: key, Size: int64(len(b))}, nil
}

func (m *memStore) List(_ context.Context, prefix string, _ int) ([]blobstore.ObjectRef, error) {
	var refs []blobstore.ObjectRef
	for k, v := range m.objs {
		if len(prefix) == 0 || (len(k) >= len(prefix) && k[:len(prefix)] == prefix) {
			refs = append(refs, blobstore.ObjectRef{Bucket: "dr-uploads", Key: k, Size: int64(len(v))})
		}
	}
	return refs, nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	delete(m.objs, key)
	return nil
}

// ----- key scheme matches the knowledge#15 contract ------------------

func TestZoteroKeyMatchesContract(t *testing.T) {
	s := New(t.TempDir(), nil)
	key, err := s.ZoteroKey("ABCD1234.pdf")
	if err != nil {
		t.Fatalf("ZoteroKey: %v", err)
	}
	if key != "zotero/ABCD1234.pdf" {
		t.Errorf("key = %q, want zotero/ABCD1234.pdf", key)
	}
}

func TestPluginKeyUsesPluginPrefix(t *testing.T) {
	s := New(t.TempDir(), nil)
	key, err := s.PluginKey("zotero", "run-1", "zotero.sqlite")
	if err != nil {
		t.Fatalf("PluginKey: %v", err)
	}
	if key != "plugin/zotero/run-1/zotero.sqlite" {
		t.Errorf("key = %q, want plugin/zotero/run-1/zotero.sqlite", key)
	}
}

// ----- CommitFile uploads + removes the local copy --------------------

func TestCommitFileUploadsAndRemovesLocal(t *testing.T) {
	root := t.TempDir()
	store := newMemStore()
	s := New(root, store)

	local, err := s.LegacyZoteroPath("ABCD.pdf")
	if err != nil {
		t.Fatalf("LegacyZoteroPath: %v", err)
	}
	if err := os.WriteFile(local, []byte("%PDF-1.7 hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	key, _ := s.ZoteroKey("ABCD.pdf")

	ref, err := s.CommitFile(context.Background(), local, key)
	if err != nil {
		t.Fatalf("CommitFile: %v", err)
	}
	if ref.Key != "zotero/ABCD.pdf" {
		t.Errorf("ref.Key = %q", ref.Key)
	}
	if got := string(store.objs["zotero/ABCD.pdf"]); got != "%PDF-1.7 hello" {
		t.Errorf("stored bytes = %q", got)
	}
	if _, statErr := os.Stat(local); !os.IsNotExist(statErr) {
		t.Errorf("local copy still present after commit: %v", statErr)
	}
}

func TestCommitFileNoStoreFailsSoft(t *testing.T) {
	s := New(t.TempDir(), nil)
	_, err := s.CommitFile(context.Background(), filepath.Join(t.TempDir(), "x"), "k")
	if err != ErrNoBlobStore {
		t.Errorf("err = %v, want ErrNoBlobStore", err)
	}
}

func TestCommitReaderKeepsLocalCopy(t *testing.T) {
	store := newMemStore()
	s := New(t.TempDir(), store)
	_, err := s.CommitReader(context.Background(), "plugin/zotero/r/zotero.sqlite",
		bytes.NewReader([]byte("sqlite-bytes")))
	if err != nil {
		t.Fatalf("CommitReader: %v", err)
	}
	if got := string(store.objs["plugin/zotero/r/zotero.sqlite"]); got != "sqlite-bytes" {
		t.Errorf("stored = %q", got)
	}
}

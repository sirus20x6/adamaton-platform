// Unit tests for the per-RPC plumbing in hostserver. We don't stand up a
// real Postgres / secrets pair; the dbConn + secretsAPI interfaces let
// us swap in fakes that exercise the wire shape (Unauthenticated when
// ctx has no plugin id, allowlist enforcement on UpsertImportRow, etc.).
package hostserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/sirus20x6/adamaton-core/blobstore"
	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/stage"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/supervisor"
)

// ----- fakes ----------------------------------------------------------

// fakeRow implements pgx.Row. err is the error Scan returns (typically
// pgx.ErrNoRows for the "miss" path); values are copied into dest pointers
// in order. Untyped nils pass through unchanged so *string fields land as
// NULL when the test omits them.
type fakeRow struct {
	values []any
	err    error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i >= len(r.values) {
			break
		}
		v := r.values[i]
		switch ptr := d.(type) {
		case *string:
			if v == nil {
				*ptr = ""
			} else if s, ok := v.(string); ok {
				*ptr = s
			}
		case **string:
			if v == nil {
				*ptr = nil
			} else if s, ok := v.(string); ok {
				ss := s
				*ptr = &ss
			}
		}
	}
	return nil
}

// fakeDB is the dbConn the tests pass. Records the last SQL + args so
// assertions can inspect them; returns the rows + errors the test set.
type fakeDB struct {
	nextRow *fakeRow
	lastSQL string
	lastArg []any
	execErr error
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.lastSQL = sql
	f.lastArg = args
	if f.nextRow == nil {
		return &fakeRow{err: pgx.ErrNoRows}
	}
	return f.nextRow
}
func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgxConnTag, error) {
	f.lastSQL = sql
	f.lastArg = args
	return nil, f.execErr
}

// fakeSecrets satisfies secretsAPI; the store is a map keyed by plugin id
// so a round-trip test (Set then Get) reads back the same shape.
type fakeSecrets struct {
	store  map[string]map[string]any
	getErr error
	setErr error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{store: map[string]map[string]any{}}
}

func (f *fakeSecrets) Get(_ context.Context, _, pluginID string) (map[string]any, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.store[pluginID], nil
}
func (f *fakeSecrets) Set(_ context.Context, _, pluginID string, cfg map[string]any) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.store[pluginID] = cfg
	return nil
}

// ctxWithPlugin returns a context with the supervisor's plugin-id stamp.
// The hostserver pulls it out via supervisor.PluginIDFromContext.
func ctxWithPlugin(id string) context.Context {
	return context.WithValue(context.Background(), supervisor.PluginCtxKey{}, id)
}

// ----- Identity gating ------------------------------------------------

func TestRPCsRefuseMissingPluginIdentity(t *testing.T) {
	s := &Server{db: &fakeDB{}, sec: newFakeSecrets()}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"IsKnown", func() error {
			_, err := s.IsKnown(ctx, &pluginv1.IsKnownRequest{ExternalId: "x"})
			return err
		}},
		{"UpsertImportRow", func() error {
			_, err := s.UpsertImportRow(ctx, &pluginv1.UpsertImportRowRequest{Table: "zotero_imports"})
			return err
		}},
		{"StagePath", func() error {
			_, err := s.StagePath(ctx, &pluginv1.StagePathRequest{Filename: "x.pdf"})
			return err
		}},
		{"GetConfig", func() error {
			_, err := s.GetConfig(ctx, &pluginv1.GetConfigRequest{})
			return err
		}},
		{"SetConfig", func() error {
			_, err := s.SetConfig(ctx, &pluginv1.SetConfigRequest{})
			return err
		}},
	}
	for _, tc := range cases {
		err := tc.call()
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: code = %v, want Unauthenticated", tc.name, status.Code(err))
		}
	}
}

// ----- IsKnown --------------------------------------------------------

func TestIsKnownReturnsKnownWhenRowExists(t *testing.T) {
	docID := "00000000-0000-0000-0000-000000000001"
	db := &fakeDB{nextRow: &fakeRow{values: []any{docID}}}
	s := &Server{db: db}
	resp, err := s.IsKnown(ctxWithPlugin("zotero"), &pluginv1.IsKnownRequest{ExternalId: "ABCD1234"})
	if err != nil {
		t.Fatalf("IsKnown: %v", err)
	}
	if !resp.Known {
		t.Errorf("Known = false, want true")
	}
	if resp.DocumentId != docID {
		t.Errorf("DocumentId = %q, want %q", resp.DocumentId, docID)
	}
	if !strings.Contains(db.lastSQL, "platform.zotero_imports") {
		t.Errorf("SQL did not target zotero_imports: %s", db.lastSQL)
	}
}

func TestIsKnownReturnsFalseOnNoRow(t *testing.T) {
	db := &fakeDB{nextRow: &fakeRow{err: pgx.ErrNoRows}}
	s := &Server{db: db}
	resp, err := s.IsKnown(ctxWithPlugin("zotero"), &pluginv1.IsKnownRequest{ExternalId: "Z"})
	if err != nil {
		t.Fatalf("IsKnown: %v", err)
	}
	if resp.Known {
		t.Errorf("Known = true, want false")
	}
}

func TestIsKnownNonZoteroPluginGetsMiss(t *testing.T) {
	// Generic plugin_items dedup hasn't landed; non-zotero plugins should
	// not error -- they should get a clean "no" so the sync can continue.
	s := &Server{db: &fakeDB{}}
	resp, err := s.IsKnown(ctxWithPlugin("some_other"), &pluginv1.IsKnownRequest{ExternalId: "x"})
	if err != nil {
		t.Fatalf("IsKnown: %v", err)
	}
	if resp.Known {
		t.Errorf("non-zotero plugin returned Known=true unexpectedly")
	}
}

// ----- UpsertImportRow ------------------------------------------------

func TestUpsertImportRowRejectsUnknownTable(t *testing.T) {
	s := &Server{db: &fakeDB{}}
	_, err := s.UpsertImportRow(ctxWithPlugin("zotero"), &pluginv1.UpsertImportRowRequest{
		Table: "platform_users",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestUpsertImportRowRejectsForeignPlugin(t *testing.T) {
	// zotero_imports is owned by the zotero plugin; another plugin asking
	// to write there is a privilege violation.
	s := &Server{db: &fakeDB{}}
	_, err := s.UpsertImportRow(ctxWithPlugin("search_arxiv"), &pluginv1.UpsertImportRowRequest{
		Table: "zotero_imports",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestUpsertImportRowRejectsUnknownColumn(t *testing.T) {
	id := "row-id"
	db := &fakeDB{nextRow: &fakeRow{values: []any{id}}}
	row, err := structpb.NewStruct(map[string]any{
		"zotero_user_id": "users/1",
		"zotero_key":     "ABCD1234",
		"canonical_id":   "10.1/abc",
		"canonical_kind": "doi",
		"forbidden":      "value",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	_, err = s.UpsertImportRow(ctxWithPlugin("zotero"), &pluginv1.UpsertImportRowRequest{
		Table: "zotero_imports",
		Row:   row,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestUpsertImportRowHappyPath(t *testing.T) {
	id := "row-id"
	db := &fakeDB{nextRow: &fakeRow{values: []any{id}}}
	row, err := structpb.NewStruct(map[string]any{
		"zotero_user_id": "users/1",
		"zotero_key":     "ABCD1234",
		"zotero_version": float64(42),
		"canonical_id":   "10.1/abc",
		"canonical_kind": "doi",
		"doi":            "10.1/abc",
		"title":          "A Paper",
		"ingest_status":  "queued",
		"metadata":       map[string]any{"key": "ABCD1234"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	resp, err := s.UpsertImportRow(ctxWithPlugin("zotero"), &pluginv1.UpsertImportRowRequest{
		Table: "zotero_imports",
		Row:   row,
	})
	if err != nil {
		t.Fatalf("UpsertImportRow: %v", err)
	}
	if resp.Id != id {
		t.Errorf("Id = %q, want %q", resp.Id, id)
	}
}

// fakeBlobs is a minimal in-memory blobstore.Backend for the commit-trigger
// test. Only Put + Stat are exercised by the zotero PDF commit path.
type fakeBlobs struct{ objs map[string][]byte }

func (f *fakeBlobs) EnsureBucket(context.Context) error { return nil }
func (f *fakeBlobs) Put(_ context.Context, key string, r io.Reader, _ int64) (blobstore.ObjectRef, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return blobstore.ObjectRef{}, err
	}
	f.objs[key] = b
	return blobstore.ObjectRef{Bucket: "dr-uploads", Key: key, Size: int64(len(b))}, nil
}
func (f *fakeBlobs) PutMultipart(ctx context.Context, key string, r io.Reader) (blobstore.ObjectRef, error) {
	return f.Put(ctx, key, r, -1)
}
func (f *fakeBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, blobstore.ErrNotFound
}
func (f *fakeBlobs) Stat(_ context.Context, key string) (blobstore.ObjectRef, error) {
	b, ok := f.objs[key]
	if !ok {
		return blobstore.ObjectRef{}, blobstore.ErrNotFound
	}
	return blobstore.ObjectRef{Bucket: "dr-uploads", Key: key, Size: int64(len(b))}, nil
}
func (f *fakeBlobs) List(context.Context, string, int) ([]blobstore.ObjectRef, error) {
	return nil, nil
}
func (f *fakeBlobs) Delete(_ context.Context, key string) error { delete(f.objs, key); return nil }

// TestUpsertImportRowCommitsStagedZoteroPDF verifies the commit trigger:
// once the zotero plugin has staged a PDF and calls UpsertImportRow, the
// host pushes the bytes to the blob store at zotero/<key>.pdf and removes
// the local copy.
func TestUpsertImportRowCommitsStagedZoteroPDF(t *testing.T) {
	root := t.TempDir()
	blobs := &fakeBlobs{objs: map[string][]byte{}}
	stg := stage.New(root, blobs)

	// Simulate the plugin having written its PDF to the staged path.
	local, err := stg.LegacyZoteroPath("ABCD1234.pdf")
	if err != nil {
		t.Fatalf("LegacyZoteroPath: %v", err)
	}
	if err := os.WriteFile(local, []byte("%PDF-1.7 body"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{nextRow: &fakeRow{values: []any{"row-id"}}}
	row, err := structpb.NewStruct(map[string]any{
		"zotero_user_id": "users/1",
		"zotero_key":     "ABCD1234",
		"canonical_id":   "10.1/abc",
		"canonical_kind": "doi",
		"ingest_status":  "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, Stage: stg}
	if _, err := s.UpsertImportRow(ctxWithPlugin("zotero"), &pluginv1.UpsertImportRowRequest{
		Table: "zotero_imports",
		Row:   row,
	}); err != nil {
		t.Fatalf("UpsertImportRow: %v", err)
	}

	if got := string(blobs.objs["zotero/ABCD1234.pdf"]); got != "%PDF-1.7 body" {
		t.Errorf("blob at zotero/ABCD1234.pdf = %q, want committed PDF bytes", got)
	}
	if _, statErr := os.Stat(local); !os.IsNotExist(statErr) {
		t.Errorf("local staged PDF still present after commit: %v", statErr)
	}
}

// TestUpsertImportRowNoStagedPDFIsNoop verifies the common web_api / DOI
// path where no PDF was staged: the upsert succeeds and the commit step is
// a silent no-op (no error, nothing in the blob store).
func TestUpsertImportRowNoStagedPDFIsNoop(t *testing.T) {
	blobs := &fakeBlobs{objs: map[string][]byte{}}
	stg := stage.New(t.TempDir(), blobs)
	db := &fakeDB{nextRow: &fakeRow{values: []any{"row-id"}}}
	row, err := structpb.NewStruct(map[string]any{
		"zotero_user_id": "users/1",
		"zotero_key":     "NOPDF999",
		"canonical_id":   "10.1/x",
		"canonical_kind": "doi",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, Stage: stg}
	if _, err := s.UpsertImportRow(ctxWithPlugin("zotero"), &pluginv1.UpsertImportRowRequest{
		Table: "zotero_imports",
		Row:   row,
	}); err != nil {
		t.Fatalf("UpsertImportRow: %v", err)
	}
	if len(blobs.objs) != 0 {
		t.Errorf("expected no blobs committed, got %v", blobs.objs)
	}
}

// ----- StagePath ------------------------------------------------------

func TestStagePathSanitisesPathTraversal(t *testing.T) {
	stg := stage.New(t.TempDir(), nil)
	s := &Server{Stage: stg}
	_, err := s.StagePath(ctxWithPlugin("zotero"), &pluginv1.StagePathRequest{
		Filename: "../etc/passwd",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument for path traversal", status.Code(err))
	}
}

func TestStagePathZoteroPDFUsesLegacyLayout(t *testing.T) {
	root := t.TempDir()
	stg := stage.New(root, nil)
	s := &Server{Stage: stg}
	resp, err := s.StagePath(ctxWithPlugin("zotero"), &pluginv1.StagePathRequest{
		Filename:    "ABCD.pdf",
		ContentType: "application/pdf",
		RunId:       "r1",
	})
	if err != nil {
		t.Fatalf("StagePath: %v", err)
	}
	want := filepath.Join(root, "zotero", "ABCD.pdf")
	if resp.Path != want {
		t.Errorf("path = %q, want %q", resp.Path, want)
	}
}

func TestStagePathGenericUsesPluginLayout(t *testing.T) {
	root := t.TempDir()
	stg := stage.New(root, nil)
	s := &Server{Stage: stg}
	resp, err := s.StagePath(ctxWithPlugin("search_arxiv"), &pluginv1.StagePathRequest{
		Filename:    "doc.json",
		ContentType: "application/json",
		RunId:       "run-1",
	})
	if err != nil {
		t.Fatalf("StagePath: %v", err)
	}
	want := filepath.Join(root, "plugins", "search_arxiv", "run-1", "doc.json")
	if resp.Path != want {
		t.Errorf("path = %q, want %q", resp.Path, want)
	}
}

// ----- GetConfig / SetConfig round-trip --------------------------------

func TestSetGetConfigRoundTrip(t *testing.T) {
	sec := newFakeSecrets()
	s := &Server{sec: sec}

	cfg, _ := structpb.NewStruct(map[string]any{
		"source":     "web_api",
		"api_key":    "abcd1234",
		"library_id": "12345",
	})
	if _, err := s.SetConfig(ctxWithPlugin("zotero"), &pluginv1.SetConfigRequest{Config: cfg}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	resp, err := s.GetConfig(ctxWithPlugin("zotero"), &pluginv1.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	got := resp.Config.AsMap()
	if got["source"] != "web_api" {
		t.Errorf("config[source] = %v, want web_api", got["source"])
	}
	if got["api_key"] != "abcd1234" {
		t.Errorf("config[api_key] = %v, want abcd1234", got["api_key"])
	}
}

func TestGetConfigMissingRowReturnsEmpty(t *testing.T) {
	// secrets.Manager.Get returns (nil, nil) when the row is absent; the
	// hostserver should turn that into an empty Struct, not an error.
	sec := newFakeSecrets()
	s := &Server{sec: sec}
	resp, err := s.GetConfig(ctxWithPlugin("zotero"), &pluginv1.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if len(resp.Config.AsMap()) != 0 {
		t.Errorf("expected empty config, got %v", resp.Config.AsMap())
	}
}

func TestSetConfigPropagatesSecretsError(t *testing.T) {
	sec := newFakeSecrets()
	sec.setErr = errors.New("boom")
	s := &Server{sec: sec}
	cfg, _ := structpb.NewStruct(map[string]any{"k": "v"})
	_, err := s.SetConfig(ctxWithPlugin("zotero"), &pluginv1.SetConfigRequest{Config: cfg})
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

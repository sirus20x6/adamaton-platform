// Package hostserver implements the gRPC Host service plugins call back
// into. Every plugin connects to <socket_dir>/<plugin_id>.<pid>.host.sock
// and the supervisor wires its identity through. The methods below trust
// the plugin id stamped into ctx by the supervisor's per-conn interceptor:
// every RPC fails closed with Unauthenticated if that's missing.
package hostserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/persist"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/secrets"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/stage"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/supervisor"
)

// dbConn is the slice of pgxpool.Pool we actually use. Carved out as an
// interface so the unit tests can substitute a fake without standing up
// a real pgx pool.
type dbConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgxConnTag, error)
}

// pgxConnTag is the bit of pgconn.CommandTag we rely on. We don't use
// the methods; the interface exists so dbConn.Exec's signature is
// implementable by a test fake without pulling in pgconn.
type pgxConnTag interface{}

// poolAdapter lets us pass a *pgxpool.Pool through the dbConn interface;
// pgconn.CommandTag satisfies pgxConnTag trivially (any).
type poolAdapter struct{ p *pgxpool.Pool }

func (a poolAdapter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return a.p.QueryRow(ctx, sql, args...)
}
func (a poolAdapter) Exec(ctx context.Context, sql string, args ...any) (pgxConnTag, error) {
	tag, err := a.p.Exec(ctx, sql, args...)
	return tag, err
}

// secretsAPI is the bit of secrets.Manager we depend on. Same rationale
// as dbConn: tests pass a fake.
type secretsAPI interface {
	Get(ctx context.Context, userID, pluginID string) (map[string]any, error)
	Set(ctx context.Context, userID, pluginID string, cfg map[string]any) error
}

// Server holds the dependencies the Host RPCs need. The fields stay
// exported for the test seam -- a fake secretsAPI / dbConn in unit tests
// beats a constructor-injection ceremony.
type Server struct {
	pluginv1.UnimplementedHostServer

	Pool    *pgxpool.Pool
	Logger  *logrus.Logger
	Store   *persist.Store
	Secrets *secrets.Manager
	Stage   *stage.Stager

	// db / sec are the test-seam wrappers. nil in production -> we lazily
	// wrap Pool / Secrets the first time we need them.
	db  dbConn
	sec secretsAPI
}

// New wires the deps. Real peer-identity plumbing happens in the
// supervisor's per-conn interceptor; this constructor just builds the
// methods' shared state.
func New(pool *pgxpool.Pool, logger *logrus.Logger, store *persist.Store, sec *secrets.Manager, stg *stage.Stager) *Server {
	s := &Server{
		Pool:    pool,
		Logger:  logger,
		Store:   store,
		Secrets: sec,
		Stage:   stg,
	}
	if pool != nil {
		s.db = poolAdapter{p: pool}
	}
	if sec != nil {
		s.sec = sec
	}
	return s
}

// pluginIDFromContext reads the plugin id the supervisor's per-conn
// gRPC interceptor stamps into ctx. Returns Unauthenticated for calls
// that reached the host server through a non-supervisor route.
func pluginIDFromContext(ctx context.Context) (string, error) {
	id := supervisor.PluginIDFromContext(ctx)
	if id == "" {
		return "", errors.New("hostserver: no plugin identity in context")
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// IsKnown
// ---------------------------------------------------------------------------

func (s *Server) IsKnown(ctx context.Context, req *pluginv1.IsKnownRequest) (*pluginv1.IsKnownResponse, error) {
	pluginID, err := pluginIDFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	// The on-wire plugin_id field is a hint from the plugin; we trust the
	// supervisor-stamped one. The request's value is allowed but ignored
	// when it disagrees -- this matches the principle that a plugin can
	// only ask about its own namespace.
	_ = req.GetPluginId()

	external := req.GetExternalId()
	if external == "" {
		return nil, status.Error(codes.InvalidArgument, "external_id is empty")
	}
	if s.db == nil {
		return nil, status.Error(codes.FailedPrecondition, "no database pool")
	}

	switch pluginID {
	case "zotero":
		// canonical_id OR zotero_key both legitimately identify a prior
		// import. The plugin doesn't know which form it has at probe time
		// (the canonical_id may be a doi / arxiv id / content hash).
		var docID *string
		row := s.db.QueryRow(ctx, `
			SELECT document_id::text
			FROM platform.zotero_imports
			WHERE (canonical_id = $1 OR zotero_key = $1)
			  AND ingest_status IN ('ingested','queued','duplicate')
			ORDER BY created_at ASC
			LIMIT 1
		`, external)
		if err := row.Scan(&docID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return &pluginv1.IsKnownResponse{Known: false}, nil
			}
			return nil, status.Errorf(codes.Internal, "scan zotero_imports: %v", err)
		}
		resp := &pluginv1.IsKnownResponse{Known: true}
		if docID != nil {
			resp.DocumentId = *docID
		}
		return resp, nil
	default:
		// Generic plugin_items dedup is a Phase B follow-up; until that
		// table is wired, every non-zotero plugin gets a clean miss.
		return &pluginv1.IsKnownResponse{Known: false}, nil
	}
}

// ---------------------------------------------------------------------------
// UpsertImportRow
// ---------------------------------------------------------------------------

// zoteroAllowedColumns is the allowlist for what a plugin may write into
// platform.zotero_imports. Keep in sync with alembic 0004; values omitted
// from the row map fall back to whatever default the column declared.
var zoteroAllowedColumns = map[string]struct{}{
	"zotero_user_id": {},
	"zotero_key":     {},
	"zotero_version": {},
	"canonical_id":   {},
	"canonical_kind": {},
	"doi":            {},
	"arxiv_id":       {},
	"isbn":           {},
	"content_hash":   {},
	"title":          {},
	"document_id":    {},
	"ingest_status":  {},
	"ingest_error":   {},
	"metadata":       {},
}

func (s *Server) UpsertImportRow(ctx context.Context, req *pluginv1.UpsertImportRowRequest) (*pluginv1.UpsertImportRowResponse, error) {
	pluginID, err := pluginIDFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	if req.GetTable() != "zotero_imports" {
		return nil, status.Errorf(codes.PermissionDenied, "table %q not in allowlist", req.GetTable())
	}
	if pluginID != "zotero" {
		// Today only the zotero plugin owns the zotero_imports table.
		// Once generic importers land they'll come in through
		// InsertPluginItem (host-driven from a Sync stream item), not
		// this path.
		return nil, status.Errorf(codes.PermissionDenied, "plugin %q may not write to zotero_imports", pluginID)
	}
	if s.db == nil {
		return nil, status.Error(codes.FailedPrecondition, "no database pool")
	}

	row := req.GetRow().AsMap()
	for k := range row {
		if _, ok := zoteroAllowedColumns[k]; !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown column %q", k)
		}
	}

	// Column extraction. JSON numbers come through as float64; coerce.
	zUser, _ := row["zotero_user_id"].(string)
	zKey, _ := row["zotero_key"].(string)
	if zUser == "" || zKey == "" {
		return nil, status.Error(codes.InvalidArgument, "zotero_user_id and zotero_key are required")
	}
	zVersion := int64Ptr(row["zotero_version"])
	canonicalID, _ := row["canonical_id"].(string)
	canonicalKind, _ := row["canonical_kind"].(string)
	if canonicalID == "" || canonicalKind == "" {
		return nil, status.Error(codes.InvalidArgument, "canonical_id and canonical_kind are required")
	}
	doi := stringPtr(row["doi"])
	arxivID := stringPtr(row["arxiv_id"])
	isbn := stringPtr(row["isbn"])
	contentHash := bytesPtr(row["content_hash"])
	title := stringPtr(row["title"])
	documentID := stringPtr(row["document_id"])
	ingestStatus, _ := row["ingest_status"].(string)
	if ingestStatus == "" {
		ingestStatus = "pending"
	}
	ingestError := stringPtr(row["ingest_error"])

	// metadata: JSON-encode whatever the plugin put there. Plugins may
	// pass either a structured object or a raw string; we accept both
	// because pgx wants a string for jsonb-via-cast.
	var metadataJSON string
	if mv, ok := row["metadata"]; ok && mv != nil {
		b, err := json.Marshal(mv)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "marshal metadata: %v", err)
		}
		metadataJSON = string(b)
	} else {
		metadataJSON = "{}"
	}

	// Matches the SQL in app/zotero/sync.py:_upsert_import_row almost
	// verbatim -- the column set and ON CONFLICT key are part of the
	// table contract; we don't get to evolve them here.
	const sqlText = `
		INSERT INTO platform.zotero_imports (
			zotero_user_id, zotero_key, zotero_version,
			canonical_id, canonical_kind,
			doi, arxiv_id, isbn, content_hash,
			title, document_id, ingest_status, ingest_error, metadata,
			created_at, updated_at
		) VALUES (
			$1, $2, $3,
			$4, $5,
			$6, $7, $8, $9,
			$10, $11, $12, $13,
			CAST($14 AS jsonb), now(), now()
		)
		ON CONFLICT (zotero_user_id, zotero_key) DO UPDATE
		SET zotero_version = EXCLUDED.zotero_version,
		    canonical_id   = EXCLUDED.canonical_id,
		    canonical_kind = EXCLUDED.canonical_kind,
		    doi            = EXCLUDED.doi,
		    arxiv_id       = EXCLUDED.arxiv_id,
		    isbn           = EXCLUDED.isbn,
		    content_hash   = EXCLUDED.content_hash,
		    title          = EXCLUDED.title,
		    document_id    = COALESCE(EXCLUDED.document_id, platform.zotero_imports.document_id),
		    ingest_status  = EXCLUDED.ingest_status,
		    ingest_error   = EXCLUDED.ingest_error,
		    metadata       = EXCLUDED.metadata,
		    updated_at     = now()
		RETURNING id::text
	`
	var id string
	scanRow := s.db.QueryRow(ctx, sqlText,
		zUser, zKey, zVersion,
		canonicalID, canonicalKind,
		doi, arxivID, isbn, contentHash,
		title, documentID, ingestStatus, ingestError,
		metadataJSON,
	)
	if err := scanRow.Scan(&id); err != nil {
		return nil, status.Errorf(codes.Internal, "upsert zotero_imports: %v", err)
	}
	return &pluginv1.UpsertImportRowResponse{Id: id}, nil
}

// ---------------------------------------------------------------------------
// StagePath
// ---------------------------------------------------------------------------

func (s *Server) StagePath(ctx context.Context, req *pluginv1.StagePathRequest) (*pluginv1.StagePathResponse, error) {
	pluginID, err := pluginIDFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	if s.Stage == nil {
		return nil, status.Error(codes.FailedPrecondition, "no stager configured")
	}
	filename := req.GetFilename()
	if filename == "" {
		return nil, status.Error(codes.InvalidArgument, "filename is empty")
	}
	contentType := req.GetContentType()

	// Forward-compat shim: the legacy ingest worker reads PDFs out of
	// /var/lib/dr-uploads/zotero/<key>.pdf. As long as that worker still
	// exists, the zotero plugin's PDFs need to land there.
	if pluginID == "zotero" && strings.Contains(strings.ToLower(contentType), "pdf") {
		path, err := s.Stage.LegacyZoteroPath(filename)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "stage path: %v", err)
		}
		return &pluginv1.StagePathResponse{Path: path}, nil
	}

	path, err := s.Stage.PluginPath(pluginID, req.GetRunId(), filename, contentType)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "stage path: %v", err)
	}
	return &pluginv1.StagePathResponse{Path: path}, nil
}

func (s *Server) WriteAttachment(ctx context.Context, req *pluginv1.WriteAttachmentRequest) (*pluginv1.WriteAttachmentResponse, error) {
	if _, err := pluginIDFromContext(ctx); err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	_ = req
	return nil, status.Error(codes.Unimplemented, "WriteAttachment not yet implemented")
}

// ---------------------------------------------------------------------------
// Config (Get / Set)
// ---------------------------------------------------------------------------

func (s *Server) GetConfig(ctx context.Context, _ *pluginv1.GetConfigRequest) (*pluginv1.GetConfigResponse, error) {
	pluginID, err := pluginIDFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	if s.sec == nil {
		return nil, status.Error(codes.FailedPrecondition, "no secrets manager")
	}
	cfg, err := s.sec.Get(ctx, "singleton", pluginID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get config: %v", err)
	}
	// Missing row -> empty struct (NOT an error). Plugin code reads
	// config_dict() and treats {} as "no saved config".
	str, err := structpb.NewStruct(cfg)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode config: %v", err)
	}
	return &pluginv1.GetConfigResponse{Config: str}, nil
}

func (s *Server) SetConfig(ctx context.Context, req *pluginv1.SetConfigRequest) (*pluginv1.SetConfigResponse, error) {
	pluginID, err := pluginIDFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	if s.sec == nil {
		return nil, status.Error(codes.FailedPrecondition, "no secrets manager")
	}
	cfg := req.GetConfig().AsMap()
	if err := s.sec.Set(ctx, "singleton", pluginID, cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "set config: %v", err)
	}
	return &pluginv1.SetConfigResponse{}, nil
}

// ---------------------------------------------------------------------------
// Observability (still stubs; Phase D)
// ---------------------------------------------------------------------------

func (s *Server) EmitMetric(ctx context.Context, req *pluginv1.EmitMetricRequest) (*pluginv1.EmitMetricResponse, error) {
	if _, err := pluginIDFromContext(ctx); err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	_ = req
	return nil, status.Error(codes.Unimplemented, "EmitMetric not yet implemented")
}

func (s *Server) EmitLog(ctx context.Context, req *pluginv1.EmitLogRequest) (*pluginv1.EmitLogResponse, error) {
	if _, err := pluginIDFromContext(ctx); err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	_ = req
	return nil, status.Error(codes.Unimplemented, "EmitLog not yet implemented")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// stringPtr returns a *string suitable for pgx parameter binding. nil
// for absent / empty values so they land as SQL NULL.
func stringPtr(v any) *string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return &x
	case fmt.Stringer:
		s := x.String()
		if s == "" {
			return nil
		}
		return &s
	default:
		return nil
	}
}

// int64Ptr coerces JSON-y numerics (float64) into *int64. JSON has no
// int type so plugin payloads always arrive as float64 here.
func int64Ptr(v any) *int64 {
	switch x := v.(type) {
	case nil:
		return nil
	case float64:
		n := int64(x)
		return &n
	case float32:
		n := int64(x)
		return &n
	case int:
		n := int64(x)
		return &n
	case int64:
		n := x
		return &n
	default:
		return nil
	}
}

// bytesPtr extracts a byte slice from a row map value. JSON's only
// string-like type for binary is base64 strings, but Struct doesn't
// carry bytes natively -- if a plugin needs content_hash to be exact
// bytes it should hex-encode and pass a string, which we'll store as
// the same bytea via hex decode below.
func bytesPtr(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		if len(x) == 0 {
			return nil
		}
		return x
	case string:
		if x == "" {
			return nil
		}
		// We can't reliably distinguish hex from raw text here. Store the
		// raw bytes of the string; plugins that want a specific encoding
		// should canonicalise on their side.
		return []byte(x)
	default:
		return nil
	}
}

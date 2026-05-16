// Package persist is the seam where plugin-host writes to the shared
// platform.* tables. The Host gRPC service (internal/hostserver) and
// the runner (internal/runner) both call into Store.
package persist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	pluginv1 "github.com/sirus20x6/adamomaton-platform/plugin-host/gen/go/dr/plugin/v1"
)

// Store holds the pool the methods use. Exported field is the test seam.
type Store struct{ Pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// IsKnown probes platform.plugin_items by (plugin_id, external_id).
// Returns (known, document_id_or_empty, err).
func (s *Store) IsKnown(ctx context.Context, pluginID, externalID string) (bool, string, error) {
	if pluginID == "" || externalID == "" {
		return false, "", errors.New("persist: plugin_id + external_id required")
	}
	var docID *string
	err := s.Pool.QueryRow(ctx, `
		SELECT document_id::text
		FROM platform.plugin_items
		WHERE plugin_id = $1 AND external_id = $2
		LIMIT 1
	`, pluginID, externalID).Scan(&docID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("persist.IsKnown: %w", err)
	}
	if docID == nil {
		return true, "", nil
	}
	return true, *docID, nil
}

// InsertPluginItem upserts a row into platform.plugin_items. The
// (plugin_id, external_id) unique index drives ON CONFLICT so re-emitting
// the same item across runs updates the existing row rather than 23505-ing.
// ingest_status starts at 'pending' and the ingest worker promotes it
// later; the runner just records that the plugin emitted the envelope.
func (s *Store) InsertPluginItem(ctx context.Context, runID string, item *pluginv1.PluginItem) error {
	if item == nil {
		return errors.New("persist.InsertPluginItem: item is nil")
	}
	if item.GetPluginId() == "" || item.GetExternalId() == "" {
		return errors.New("persist.InsertPluginItem: plugin_id + external_id required")
	}
	metaBytes := []byte("{}")
	if item.GetMetadata() != nil {
		b, err := protojson.Marshal(item.GetMetadata())
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		metaBytes = b
	}
	// Coerce empty string to nil for nullable columns so callers can read
	// them as actual NULL in psql -- avoids confusing "" vs unset in
	// debugging.
	titlePtr := nilIfEmpty(item.GetTitle())
	urlPtr := nilIfEmpty(item.GetExternalUrl())
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO platform.plugin_items
			(plugin_id, external_id, external_url, title, metadata,
			 ingest_status, last_run_id, fetched_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, 'pending', $6, NOW(), NOW(), NOW())
		ON CONFLICT (plugin_id, external_id) DO UPDATE
			SET external_url = COALESCE(EXCLUDED.external_url, platform.plugin_items.external_url),
			    title        = COALESCE(EXCLUDED.title, platform.plugin_items.title),
			    metadata     = EXCLUDED.metadata,
			    last_run_id  = EXCLUDED.last_run_id,
			    fetched_at   = NOW(),
			    updated_at   = NOW()
	`, item.GetPluginId(), item.GetExternalId(), urlPtr, titlePtr, string(metaBytes), nilIfEmpty(runID))
	if err != nil {
		return fmt.Errorf("persist.InsertPluginItem: %w", err)
	}
	return nil
}

// UpsertImportRow is the generic version of the zotero-specific upsert
// the Host RPC needs. Today it just routes by table name to the dedicated
// path; the column allowlist + checks live in hostserver.
func (s *Store) UpsertImportRow(ctx context.Context, runID, pluginID, table string, row map[string]any) (string, error) {
	switch table {
	case "zotero_imports":
		return s.upsertZoteroImport(ctx, runID, row)
	default:
		return "", fmt.Errorf("persist.UpsertImportRow: unknown table %q", table)
	}
}

func (s *Store) upsertZoteroImport(ctx context.Context, runID string, row map[string]any) (string, error) {
	// Marshal the metadata bag (we don't know all the keys ahead of time,
	// but the column list is fixed -- see column allowlist in hostserver).
	metaBytes, _ := json.Marshal(row["metadata"])
	if len(metaBytes) == 0 {
		metaBytes = []byte("{}")
	}
	// Some columns are nullable; the helper coerces "" to nil so a
	// missing field doesn't violate NOT NULL or write an empty string
	// where NULL is more honest.
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO platform.zotero_imports
			(zotero_user_id, zotero_key, zotero_version, canonical_id,
			 canonical_kind, doi, arxiv_id, isbn, content_hash, title,
			 ingest_status, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'pending', $11::jsonb, NOW(), NOW())
		ON CONFLICT (zotero_user_id, zotero_key) DO UPDATE
			SET zotero_version = EXCLUDED.zotero_version,
			    canonical_id   = COALESCE(EXCLUDED.canonical_id, platform.zotero_imports.canonical_id),
			    canonical_kind = COALESCE(EXCLUDED.canonical_kind, platform.zotero_imports.canonical_kind),
			    doi            = COALESCE(EXCLUDED.doi, platform.zotero_imports.doi),
			    arxiv_id       = COALESCE(EXCLUDED.arxiv_id, platform.zotero_imports.arxiv_id),
			    isbn           = COALESCE(EXCLUDED.isbn, platform.zotero_imports.isbn),
			    content_hash   = COALESCE(EXCLUDED.content_hash, platform.zotero_imports.content_hash),
			    title          = COALESCE(EXCLUDED.title, platform.zotero_imports.title),
			    metadata       = EXCLUDED.metadata,
			    updated_at     = NOW()
		RETURNING id::text
	`,
		strVal(row, "zotero_user_id"),
		strVal(row, "zotero_key"),
		intVal(row, "zotero_version"),
		nilIfEmpty(strVal(row, "canonical_id")),
		nilIfEmpty(strVal(row, "canonical_kind")),
		nilIfEmpty(strVal(row, "doi")),
		nilIfEmpty(strVal(row, "arxiv_id")),
		nilIfEmpty(strVal(row, "isbn")),
		bytesVal(row, "content_hash"),
		nilIfEmpty(strVal(row, "title")),
		string(metaBytes),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("persist.upsertZoteroImport: %w", err)
	}
	_ = runID // reserved for FK to plugin_runs once that link is wired
	return id, nil
}

// UpdateRunStarted moves plugin_runs.status from 'pending' to 'running'
// atomically -- the WHERE clause is the lock so two workers can't both
// grab the same row. Returns true if we got the row, false if someone
// else did or it disappeared.
func (s *Store) UpdateRunStarted(ctx context.Context, runID string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE platform.plugin_runs
		SET status = 'running', started_at = NOW()
		WHERE id = $1 AND status = 'pending'
	`, runID)
	if err != nil {
		return false, fmt.Errorf("persist.UpdateRunStarted: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateRunFinished closes a run with the final status + totals + optional
// error message. Idempotent under retry: subsequent calls with the same
// status are harmless.
func (s *Store) UpdateRunFinished(ctx context.Context, runID, status string, totals map[string]int64, errMsg string) error {
	totalsBytes, _ := json.Marshal(totals)
	if len(totalsBytes) == 0 {
		totalsBytes = []byte("{}")
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE platform.plugin_runs
		SET status      = $2,
		    totals      = $3::jsonb,
		    error       = NULLIF($4, ''),
		    finished_at = NOW()
		WHERE id = $1
	`, runID, status, string(totalsBytes), errMsg)
	if err != nil {
		return fmt.Errorf("persist.UpdateRunFinished: %w", err)
	}
	return nil
}

// PickPendingRun atomically grabs the oldest pending run for processing
// using SELECT FOR UPDATE SKIP LOCKED. Two workers polling concurrently
// won't dispatch the same run. Returns (id, plugin_id, args, error). id
// is "" when nothing's pending.
func (s *Store) PickPendingRun(ctx context.Context) (string, string, map[string]any, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", nil, err
	}
	defer tx.Rollback(ctx)

	var id, pluginID string
	var argsBytes []byte
	err = tx.QueryRow(ctx, `
		SELECT id::text, plugin_id, args
		FROM platform.plugin_runs
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&id, &pluginID, &argsBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", nil, nil
		}
		return "", "", nil, err
	}
	// Flip status under the same tx so SKIP LOCKED stays effective.
	if _, err := tx.Exec(ctx, `
		UPDATE platform.plugin_runs SET status = 'running', started_at = NOW() WHERE id = $1
	`, id); err != nil {
		return "", "", nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", nil, err
	}
	args := map[string]any{}
	if len(argsBytes) > 0 {
		_ = json.Unmarshal(argsBytes, &args)
	}
	return id, pluginID, args, nil
}

// ----- small helpers ------------------------------------------------------

func strVal(m map[string]any, k string) string {
	v, ok := m[k]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func intVal(m map[string]any, k string) int {
	v, ok := m[k]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64: // json.Unmarshal -> map[string]any defaults numbers to float64
		return int(x)
	}
	return 0
}

func bytesVal(m map[string]any, k string) []byte {
	v, ok := m[k]
	if !ok {
		return nil
	}
	switch x := v.(type) {
	case []byte:
		return x
	case string:
		if x == "" {
			return nil
		}
		return []byte(x)
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

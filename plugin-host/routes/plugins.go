// routes/plugins.go owns /platform/plugins/*. URL shape mirrors the
// legacy app/api/plugins.py (now retired) so the deepresearch frontend
// works unchanged against either backend during the cutover.
//
// Routes:
//
//	GET    /platform/plugins                  list manifests
//	GET    /platform/plugins/runs             paginated run history
//	GET    /platform/plugins/runs/{run_id}    single run row
//	GET    /platform/plugins/items            paginated item history
//	DELETE /platform/plugins/items/{item_id}  delete one item row
//	DELETE /platform/plugins/items            bulk-delete items by filter
//	GET    /platform/plugins/{id}             single manifest + config_schema
//	POST   /platform/plugins/{id}/run         kick off an importer run
//	GET    /platform/plugins/{id}/config      read decrypted config blob
//	PUT    /platform/plugins/{id}/config      write config blob
//
// Order matters: every static /runs and /items route is registered
// BEFORE /{id} so the variable path doesn't shadow them. gorilla/mux
// honours registration order on overlapping patterns.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/phmetrics"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/secrets"
)

// RegisterPlugins wires the plugins surface. See package doc for the
// canonical URL list + ordering rationale.
func RegisterPlugins(
	r *mux.Router,
	pool *pgxpool.Pool,
	manifests map[string]*manifest.Manifest,
	sec *secrets.Manager,
	logger *logrus.Logger,
) {
	// List manifests. Both shapes (trailing slash, no slash) because the
	// frontend hits one and curl smokes the other.
	r.HandleFunc("/platform/plugins/", listPluginsHandler(manifests)).Methods(http.MethodGet)
	r.HandleFunc("/platform/plugins", listPluginsHandler(manifests)).Methods(http.MethodGet)

	// Static /runs and /items routes BEFORE the /{id} catch-all.
	r.HandleFunc("/platform/plugins/runs", listRunsHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/plugins/runs/{run_id}", getRunHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/plugins/items", listItemsHandler(pool, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/plugins/items", bulkDeleteItemsHandler(pool, logger)).Methods(http.MethodDelete)
	r.HandleFunc("/platform/plugins/items/{item_id}", deleteItemHandler(pool, logger)).Methods(http.MethodDelete)

	// Per-plugin routes last.
	r.HandleFunc("/platform/plugins/{id}", getPluginHandler(manifests)).Methods(http.MethodGet)
	r.HandleFunc("/platform/plugins/{id}/run", createRunHandler(pool, manifests, logger)).Methods(http.MethodPost)
	r.HandleFunc("/platform/plugins/{id}/config", getConfigHandler(sec, manifests, logger)).Methods(http.MethodGet)
	r.HandleFunc("/platform/plugins/{id}/config", putConfigHandler(sec, manifests, logger)).Methods(http.MethodPut)
}

// ---------------------------------------------------------------------------
// Manifests
// ---------------------------------------------------------------------------

func listPluginsHandler(manifests map[string]*manifest.Manifest) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		items := make([]map[string]any, 0, len(manifests))
		for _, m := range manifests {
			items = append(items, manifestPayload(m))
		}
		writeJSON(w, http.StatusOK, map[string]any{"plugins": items})
	}
}

func getPluginHandler(manifests map[string]*manifest.Manifest) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := mux.Vars(req)["id"]
		m, ok := manifests[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, errorBody("plugin not found"))
			return
		}
		// Include config_schema on the single-plugin endpoint -- the
		// settings page needs it to render the form.
		payload := manifestPayload(m)
		payload["config_schema"] = m.ConfigSchema
		writeJSON(w, http.StatusOK, payload)
	}
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// runCreateBody is the JSON shape POST /{id}/run accepts. Every field
// is optional; the supervisor wire-up resolves defaults from the
// manifest args_schema once it lands.
type runCreateBody struct {
	CollectionID string         `json:"collection_id"`
	Since        string         `json:"since"`
	CorpusID     string         `json:"corpus_id"`
	Options      map[string]any `json:"options"`
}

func createRunHandler(pool *pgxpool.Pool, manifests map[string]*manifest.Manifest, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := mux.Vars(req)["id"]
		if _, ok := manifests[id]; !ok {
			writeJSON(w, http.StatusNotFound, errorBody("plugin not found"))
			return
		}
		var body runCreateBody
		// Empty body is fine -- defaults are usable; just decode if any
		// bytes are present so callers don't have to send {}.
		if req.Body != nil && req.ContentLength != 0 {
			if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
				return
			}
		}
		runID := uuid.NewString()
		// Best-effort insert into platform.plugin_runs. The supervisor
		// hand-off (which actually starts processing) lands once the
		// importer.sync RPC is wired end-to-end; today the row records
		// the intent so the frontend's poller has something to poll.
		// source='manual' is the placeholder until the supervisor wires
		// real run modes (web_api, sqlite_upload, …). plugin_runs.source
		// is NOT NULL so we have to fill something.
		_, err := pool.Exec(req.Context(), `
			INSERT INTO platform.plugin_runs (id, plugin_id, source, status, args, created_at)
			VALUES ($1, $2, 'manual', 'pending', $3, NOW())
		`, runID, id, mustMarshal(body))
		if err != nil {
			phmetrics.RequestErrors.WithLabelValues(id, "run_enqueue").Inc()
			logger.WithError(err).Warn("insert plugin_runs")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"run_id": runID,
			"status": "pending",
		})
	}
}

func getRunHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		runID := mux.Vars(req)["run_id"]
		row, err := selectRun(req.Context(), pool, runID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, errorBody("run not found"))
				return
			}
			logger.WithError(err).Warn("select plugin_runs")
			writeJSON(w, http.StatusInternalServerError, errorBody("read run failed"))
			return
		}
		writeJSON(w, http.StatusOK, row)
	}
}

func listRunsHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		pluginID := q.Get("plugin_id")
		statusFilter := q.Get("status")
		limit := parseIntInRange(q.Get("limit"), 50, 1, 500)
		offset := parseIntInRange(q.Get("offset"), 0, 0, 1<<30)

		where, args := buildRunWhere(pluginID, statusFilter)
		listSQL := `
			SELECT id, plugin_id, source, mode, args, corpus_id, status,
			       totals, error, started_at, finished_at, created_at
			FROM platform.plugin_runs ` + where + `
			ORDER BY created_at DESC NULLS LAST
			LIMIT $` + strconv.Itoa(len(args)+1) + `
			OFFSET $` + strconv.Itoa(len(args)+2)
		listArgs := append(append([]any{}, args...), limit, offset)

		rows, err := pool.Query(req.Context(), listSQL, listArgs...)
		if err != nil {
			logger.WithError(err).Warn("list plugin_runs")
			writeJSON(w, http.StatusInternalServerError, errorBody("list runs failed"))
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			row, scanErr := scanRun(rows)
			if scanErr != nil {
				logger.WithError(scanErr).Warn("scan plugin_runs")
				continue
			}
			items = append(items, row)
		}

		var total int
		_ = pool.QueryRow(req.Context(),
			`SELECT count(*) FROM platform.plugin_runs `+where, args...).Scan(&total)

		writeJSON(w, http.StatusOK, map[string]any{
			"items":  items,
			"limit":  limit,
			"offset": offset,
			"count":  len(items),
			"total":  total,
		})
	}
}

func buildRunWhere(pluginID, statusFilter string) (string, []any) {
	parts := []string{}
	args := []any{}
	if pluginID != "" {
		parts = append(parts, "plugin_id = $"+strconv.Itoa(len(args)+1))
		args = append(args, pluginID)
	}
	if statusFilter != "" {
		parts = append(parts, "status = $"+strconv.Itoa(len(args)+1))
		args = append(args, statusFilter)
	}
	if len(parts) == 0 {
		return "", args
	}
	return "WHERE " + join(parts, " AND "), args
}

func selectRun(ctx context.Context, pool *pgxpool.Pool, runID string) (map[string]any, error) {
	row := pool.QueryRow(ctx, `
		SELECT id, plugin_id, source, mode, args, corpus_id, status,
		       totals, error, started_at, finished_at, created_at
		FROM platform.plugin_runs WHERE id = $1
	`, runID)
	return scanRunRow(row)
}

// scanRun reads one pgx.Rows position into the wire shape.
func scanRun(rows pgx.Rows) (map[string]any, error) {
	return scanRunRow(rows)
}

// rowScanner abstracts QueryRow and Rows so scanRunRow handles both.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunRow(row rowScanner) (map[string]any, error) {
	var (
		id, pluginID, statusStr          string
		source, mode                     *string
		args, totals                     []byte
		corpusID                         *uuid.UUID
		errMsg                           *string
		startedAt, finishedAt, createdAt *jsonTime
	)
	if err := row.Scan(&id, &pluginID, &source, &mode, &args, &corpusID,
		&statusStr, &totals, &errMsg, &startedAt, &finishedAt, &createdAt); err != nil {
		return nil, err
	}
	return map[string]any{
		"id":          id,
		"plugin_id":   pluginID,
		"source":      strPtrOrNil(source),
		"mode":        strPtrOrNil(mode),
		"args":        decodeJSON(args),
		"corpus_id":   uuidPtrOrNil(corpusID),
		"status":      statusStr,
		"totals":      decodeJSON(totals),
		"error":       strPtrOrNil(errMsg),
		"started_at":  startedAt,
		"finished_at": finishedAt,
		"created_at":  createdAt,
	}, nil
}

// ---------------------------------------------------------------------------
// Items
// ---------------------------------------------------------------------------

func listItemsHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		pluginID := q.Get("plugin_id")
		statusFilter := q.Get("status")
		limit := parseIntInRange(q.Get("limit"), 50, 1, 500)
		offset := parseIntInRange(q.Get("offset"), 0, 0, 1<<30)

		where, args := buildItemWhere(pluginID, statusFilter)
		listSQL := `
			SELECT id, plugin_id, external_id, external_url, title, metadata,
			       markdown_path, document_id, ingest_status, ingest_error,
			       last_run_id, fetched_at, created_at, updated_at
			FROM platform.plugin_items ` + where + `
			ORDER BY updated_at DESC NULLS LAST
			LIMIT $` + strconv.Itoa(len(args)+1) + `
			OFFSET $` + strconv.Itoa(len(args)+2)
		listArgs := append(append([]any{}, args...), limit, offset)

		rows, err := pool.Query(req.Context(), listSQL, listArgs...)
		if err != nil {
			logger.WithError(err).Warn("list plugin_items")
			writeJSON(w, http.StatusInternalServerError, errorBody("list items failed"))
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			row, scanErr := scanItemRow(rows)
			if scanErr != nil {
				logger.WithError(scanErr).Warn("scan plugin_items")
				continue
			}
			items = append(items, row)
		}

		var total int
		_ = pool.QueryRow(req.Context(),
			`SELECT count(*) FROM platform.plugin_items `+where, args...).Scan(&total)

		writeJSON(w, http.StatusOK, map[string]any{
			"items":  items,
			"limit":  limit,
			"offset": offset,
			"count":  len(items),
			"total":  total,
		})
	}
}

func buildItemWhere(pluginID, statusFilter string) (string, []any) {
	parts := []string{}
	args := []any{}
	if pluginID != "" {
		parts = append(parts, "plugin_id = $"+strconv.Itoa(len(args)+1))
		args = append(args, pluginID)
	}
	if statusFilter != "" {
		parts = append(parts, "ingest_status = $"+strconv.Itoa(len(args)+1))
		args = append(args, statusFilter)
	}
	if len(parts) == 0 {
		return "", args
	}
	return "WHERE " + join(parts, " AND "), args
}

func scanItemRow(row rowScanner) (map[string]any, error) {
	var (
		id, pluginID, externalID, ingestStatus string
		externalURL, title, markdownPath       *string
		metadata                               []byte
		documentID, lastRunID                  *uuid.UUID
		ingestError                            *string
		fetchedAt, createdAt, updatedAt        *jsonTime
	)
	if err := row.Scan(&id, &pluginID, &externalID, &externalURL, &title,
		&metadata, &markdownPath, &documentID, &ingestStatus, &ingestError,
		&lastRunID, &fetchedAt, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	return map[string]any{
		"id":            id,
		"plugin_id":     pluginID,
		"external_id":   externalID,
		"external_url":  strPtrOrNil(externalURL),
		"title":         strPtrOrNil(title),
		"metadata":      decodeJSON(metadata),
		"markdown_path": strPtrOrNil(markdownPath),
		"document_id":   uuidPtrOrNil(documentID),
		"ingest_status": ingestStatus,
		"ingest_error":  strPtrOrNil(ingestError),
		"last_run_id":   uuidPtrOrNil(lastRunID),
		"fetched_at":    fetchedAt,
		"created_at":    createdAt,
		"updated_at":    updatedAt,
	}, nil
}

func deleteItemHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		itemID := mux.Vars(req)["item_id"]
		tag, err := pool.Exec(req.Context(),
			`DELETE FROM platform.plugin_items WHERE id = $1`, itemID)
		if err != nil {
			logger.WithError(err).Warn("delete plugin_items")
			writeJSON(w, http.StatusInternalServerError, errorBody("delete failed"))
			return
		}
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, errorBody("item not found"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": 1})
	}
}

func bulkDeleteItemsHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		pluginID := q.Get("plugin_id")
		statusFilter := q.Get("status")
		if pluginID == "" && statusFilter == "" {
			// Refuse a wide-open DELETE -- callers must scope to at
			// least one filter. Matches the legacy router's safety
			// behaviour.
			writeJSON(w, http.StatusBadRequest,
				errorBody("plugin_id or status filter required"))
			return
		}
		where, args := buildItemWhere(pluginID, statusFilter)
		tag, err := pool.Exec(req.Context(),
			`DELETE FROM platform.plugin_items `+where, args...)
		if err != nil {
			logger.WithError(err).Warn("bulk delete plugin_items")
			writeJSON(w, http.StatusInternalServerError, errorBody("delete failed"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"deleted": tag.RowsAffected(),
		})
	}
}

// ---------------------------------------------------------------------------
// Config (unchanged from previous shape)
// ---------------------------------------------------------------------------

func getConfigHandler(sec *secrets.Manager, manifests map[string]*manifest.Manifest, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := mux.Vars(req)["id"]
		if _, ok := manifests[id]; !ok {
			writeJSON(w, http.StatusNotFound, errorBody("plugin not found"))
			return
		}
		cfg, err := sec.Get(req.Context(), "singleton", id)
		if err != nil {
			phmetrics.RequestErrors.WithLabelValues(id, "config_read").Inc()
			logger.WithError(err).WithField("plugin_id", id).Error("get config")
			writeJSON(w, http.StatusInternalServerError, errorBody("read config failed"))
			return
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		// TODO(plugin-host): per-field masking once we have a schema-aware
		// secret tag. Today we return the raw decrypted blob; single-tenant
		// only.
		writeJSON(w, http.StatusOK, map[string]any{"plugin_id": id, "config": cfg})
	}
}

func putConfigHandler(sec *secrets.Manager, manifests map[string]*manifest.Manifest, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := mux.Vars(req)["id"]
		if _, ok := manifests[id]; !ok {
			writeJSON(w, http.StatusNotFound, errorBody("plugin not found"))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
			return
		}
		if err := sec.Set(req.Context(), "singleton", id, body); err != nil {
			phmetrics.RequestErrors.WithLabelValues(id, "config_write").Inc()
			logger.WithError(err).WithField("plugin_id", id).Error("set config")
			writeJSON(w, http.StatusInternalServerError, errorBody("write config failed"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// mustMarshal is the "I want a []byte for a pgx jsonb param" helper.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// join is strings.Join but in-package to avoid one import.
func join(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += sep + p
	}
	return out
}

// parseIntInRange parses s as int, clamps to [lo, hi], defaults to def
// when s is empty or unparseable.
func parseIntInRange(s string, def, lo, hi int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func strPtrOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func uuidPtrOrNil(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return u.String()
}

// decodeJSON unmarshals a jsonb column. Empty/null gets {}.
func decodeJSON(b []byte) any {
	if len(b) == 0 {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return map[string]any{}
	}
	return v
}

// jsonTime is just an alias for time.Time so `*jsonTime` is a clear
// nullable-timestamp parameter shape for pgx.Scan. encoding/json renders
// time.Time as RFC3339Nano, which the frontend's date parsers accept.
type jsonTime = time.Time

// routes/compat_zotero.go keeps the legacy /platform/zotero/* URLs the
// deepresearch frontend was built against. New code should POST to
// /platform/plugins/zotero/{run,config} instead -- these shims wrap
// the same persistence + plugin config and exist so the existing
// browser bundle works during the migration.
//
// Endpoints:
//
//   POST /platform/zotero/sync           — kicks a web_api sync
//   POST /platform/zotero/upload-sqlite  — multipart upload + sync from a
//                                          user-uploaded zotero.sqlite
//
// upload-sqlite is the long one: it streams the sqlite file to the
// shared dr_uploads volume, optionally extracts a storage/ tarball with
// a path-traversal guard, persists the resolved paths into the zotero
// plugin's config so the next run picks them up, and writes a
// platform.plugin_runs row so the frontend's poller can track progress.
package routes

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/secrets"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/stage"
)

// Upload caps. zotero.sqlite for a heavy library can reach a few GB
// once attachments are stored inline, so we set a generous ceiling
// rather than the 256MB default for /platform/sources uploads.
const (
	maxSqliteBytes        = 4 << 30 // 4 GiB
	maxStorageTarballByt  = 8 << 30 // 8 GiB
	maxTarMemberBytes     = 1 << 30 // 1 GiB per file inside the tarball
	multipartMemoryBudget = 16 << 20
)

// RegisterCompatZotero wires the legacy /platform/zotero/{sync,upload-sqlite}
// URLs onto plugin-host. sec + stg are required for upload-sqlite (config
// update + on-disk staging); pool is required for the plugin_runs insert.
func RegisterCompatZotero(r *mux.Router, pool *pgxpool.Pool, sec *secrets.Manager, stg *stage.Stager, logger *logrus.Logger) {
	r.HandleFunc("/platform/zotero/status", zoteroStatusHandler(pool, sec, logger)).
		Methods(http.MethodGet)
	r.HandleFunc("/platform/zotero/connect", zoteroConnectHandler(sec, logger)).
		Methods(http.MethodPost)
	r.HandleFunc("/platform/zotero/sync", zoteroCompatSyncHandler(pool, sec, logger)).
		Methods(http.MethodPost)
	r.HandleFunc("/platform/zotero/sync/{job_id}", zoteroSyncStatusHandler(pool, logger)).
		Methods(http.MethodGet)
	r.HandleFunc("/platform/zotero/upload-sqlite", zoteroUploadSqliteHandler(pool, sec, stg, logger)).
		Methods(http.MethodPost)
	r.HandleFunc("/platform/zotero/imports", zoteroImportsListHandler(pool, logger)).
		Methods(http.MethodGet)
	r.HandleFunc("/platform/zotero/imports", zoteroImportsBulkDeleteHandler(pool, logger)).
		Methods(http.MethodDelete)
	r.HandleFunc("/platform/zotero/imports/{import_id}", zoteroImportDeleteHandler(pool, logger)).
		Methods(http.MethodDelete)
}

// ---------------------------------------------------------------------------
// /status — quick "what does the zotero plugin know about itself" snapshot.
// ---------------------------------------------------------------------------

func zoteroStatusHandler(pool *pgxpool.Pool, sec *secrets.Manager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		cfg, err := sec.Get(req.Context(), "singleton", "zotero")
		if err != nil {
			logger.WithError(err).Warn("status: read zotero config")
			cfg = map[string]any{}
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		connected := false
		libraryName := any(nil)
		// "connected" today means there's *some* usable source. Web API
		// requires api_key + library_id; sqlite_upload requires
		// sqlite_path. Either one is good enough to surface as
		// connected=true in the dashboard.
		if hasAll(cfg, "api_key", "library_id") || hasAll(cfg, "sqlite_path") {
			connected = true
		}
		if name, ok := cfg["library_name"].(string); ok && name != "" {
			libraryName = name
		}

		// Most recent succeeded run gives us last_sync_at + last_totals.
		var (
			lastAt     *string
			lastTotals []byte
		)
		_ = pool.QueryRow(req.Context(), `
			SELECT to_char(finished_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
			       totals
			FROM platform.plugin_runs
			WHERE plugin_id = 'zotero' AND status = 'succeeded'
			ORDER BY finished_at DESC NULLS LAST
			LIMIT 1
		`).Scan(&lastAt, &lastTotals)

		var totals map[string]any
		if len(lastTotals) > 0 {
			_ = json.Unmarshal(lastTotals, &totals)
		}

		var itemCount int
		_ = pool.QueryRow(req.Context(),
			`SELECT count(*) FROM platform.zotero_imports`).Scan(&itemCount)

		writeJSON(w, http.StatusOK, map[string]any{
			"connected":    connected,
			"library_name": libraryName,
			"last_sync_at": strPtr(lastAt),
			"last_totals":  totals,
			"item_count":   itemCount,
		})
	}
}

// ---------------------------------------------------------------------------
// /connect — write Web API credentials. No live verify yet (pyzotero call
// equivalent would need the zotero plugin process running); the next
// /sync attempt will surface auth failures naturally.
// ---------------------------------------------------------------------------

func zoteroConnectHandler(sec *secrets.Manager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			APIKey      string `json:"api_key"`
			LibraryType string `json:"library_type"`
			LibraryID   string `json:"library_id"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("decode body: "+err.Error()))
			return
		}
		body.APIKey = strings.TrimSpace(body.APIKey)
		body.LibraryType = strings.TrimSpace(body.LibraryType)
		body.LibraryID = strings.TrimSpace(body.LibraryID)
		if body.APIKey == "" || body.LibraryID == "" {
			writeJSON(w, http.StatusBadRequest, errorBody("api_key + library_id required"))
			return
		}
		if body.LibraryType != "user" && body.LibraryType != "group" {
			body.LibraryType = "user"
		}

		cfg, err := sec.Get(req.Context(), "singleton", "zotero")
		if err != nil || cfg == nil {
			cfg = map[string]any{}
		}
		cfg["source"] = "web_api"
		cfg["api_key"] = body.APIKey
		cfg["library_type"] = body.LibraryType
		cfg["library_id"] = body.LibraryID
		if err := sec.Set(req.Context(), "singleton", "zotero", cfg); err != nil {
			logger.WithError(err).Error("connect: persist config")
			writeJSON(w, http.StatusInternalServerError, errorBody("write config failed"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"library_type": body.LibraryType,
			"library_id":   body.LibraryID,
			// library_name comes from the next sync's API verify.
			"library_name": any(nil),
		})
	}
}

// ---------------------------------------------------------------------------
// GET /sync/{job_id} — single run status. Frontend polls this from the
// SyncProgress component until status hits a terminal state.
// ---------------------------------------------------------------------------

func zoteroSyncStatusHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		jobID := mux.Vars(req)["job_id"]
		var (
			status                           string
			totalsBytes                      []byte
			startedAt, finishedAt, errMsg    *string
		)
		err := pool.QueryRow(req.Context(), `
			SELECT status, totals,
			       to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
			       to_char(finished_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
			       error
			FROM platform.plugin_runs WHERE id = $1
		`, jobID).Scan(&status, &totalsBytes, &startedAt, &finishedAt, &errMsg)
		if err != nil {
			logger.WithError(err).WithField("job_id", jobID).Debug("zotero sync status")
			writeJSON(w, http.StatusNotFound, errorBody("job not found"))
			return
		}
		var totals map[string]any
		if len(totalsBytes) > 0 {
			_ = json.Unmarshal(totalsBytes, &totals)
		}
		if totals == nil {
			totals = map[string]any{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          jobID,
			"status":      status,
			"totals":      totals,
			"started_at":  strPtr(startedAt),
			"finished_at": strPtr(finishedAt),
			"error":       strPtr(errMsg),
		})
	}
}

// ---------------------------------------------------------------------------
// /imports — paginated history. Mirrors the legacy app/api/zotero.py
// list_imports shape so the ImportsTable component works unchanged.
// ---------------------------------------------------------------------------

func zoteroImportsListHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		statusFilter := q.Get("status")
		limit := parseIntInRange(q.Get("limit"), 50, 1, 500)
		offset := parseIntInRange(q.Get("offset"), 0, 0, 1<<30)

		args := []any{}
		where := ""
		if statusFilter != "" {
			where = "WHERE ingest_status = $1"
			args = append(args, statusFilter)
		}
		args = append(args, limit, offset)
		listSQL := `
			SELECT id::text, zotero_key, title, doi, arxiv_id,
			       document_id::text, ingest_status, ingest_error,
			       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
			FROM platform.zotero_imports ` + where + `
			ORDER BY created_at DESC NULLS LAST
			LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

		rows, err := pool.Query(req.Context(), listSQL, args...)
		if err != nil {
			logger.WithError(err).Warn("list zotero_imports")
			writeJSON(w, http.StatusInternalServerError, errorBody("list failed"))
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var (
				id, zoteroKey, ingestStatus, createdAt string
				title, doi, arxivID, documentID, ingestError *string
			)
			if err := rows.Scan(&id, &zoteroKey, &title, &doi, &arxivID,
				&documentID, &ingestStatus, &ingestError, &createdAt); err != nil {
				logger.WithError(err).Warn("scan zotero_imports")
				continue
			}
			items = append(items, map[string]any{
				"id":            id,
				"zotero_key":    zoteroKey,
				"title":         strPtr(title),
				"doi":           strPtr(doi),
				"arxiv_id":      strPtr(arxivID),
				"document_id":   strPtr(documentID),
				"ingest_status": ingestStatus,
				"ingest_error":  strPtr(ingestError),
				"created_at":    createdAt,
			})
		}

		var total int
		countSQL := `SELECT count(*) FROM platform.zotero_imports`
		if statusFilter != "" {
			countSQL += ` WHERE ingest_status = $1`
			_ = pool.QueryRow(req.Context(), countSQL, statusFilter).Scan(&total)
		} else {
			_ = pool.QueryRow(req.Context(), countSQL).Scan(&total)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"items":  items,
			"limit":  limit,
			"offset": offset,
			"count":  len(items),
			"total":  total,
		})
	}
}

func zoteroImportDeleteHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := mux.Vars(req)["import_id"]
		var prevStatus string
		err := pool.QueryRow(req.Context(),
			`DELETE FROM platform.zotero_imports WHERE id = $1 RETURNING ingest_status`,
			id).Scan(&prevStatus)
		if err != nil {
			logger.WithError(err).Warn("delete zotero_import")
			writeJSON(w, http.StatusNotFound, errorBody("import not found"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"id":              id,
			"previous_status": prevStatus,
		})
	}
}

func zoteroImportsBulkDeleteHandler(pool *pgxpool.Pool, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		statusFilter := req.URL.Query().Get("status")
		if statusFilter == "" {
			// Refuse the wide-open DELETE; matches the legacy
			// app/api/zotero.py guard.
			writeJSON(w, http.StatusBadRequest,
				errorBody("status filter required"))
			return
		}
		tag, err := pool.Exec(req.Context(),
			`DELETE FROM platform.zotero_imports WHERE ingest_status = $1`,
			statusFilter)
		if err != nil {
			logger.WithError(err).Warn("bulk delete zotero_imports")
			writeJSON(w, http.StatusInternalServerError, errorBody("delete failed"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"status":  statusFilter,
			"deleted": tag.RowsAffected(),
		})
	}
}

// hasAll returns true when every k is present + non-empty in m.
func hasAll(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			return false
		}
		if s, isStr := v.(string); isStr && s == "" {
			return false
		}
	}
	return true
}

// strPtr is the "*string -> any (nil-or-value)" helper. Lets us emit
// JSON null for missing values cleanly rather than "".
func strPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// zoteroCompatSyncHandler relays POST /platform/zotero/sync to the same
// persistence path that POST /platform/plugins/zotero/run uses, with a
// body shape the legacy frontend already sends.
func zoteroCompatSyncHandler(pool *pgxpool.Pool, _ *secrets.Manager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			CorpusID    string `json:"corpus_id"`
			ForceFull   bool   `json:"force_full"`
			OnlyWithPDF bool   `json:"only_with_pdf"`
		}
		if req.Body != nil && req.ContentLength != 0 {
			_ = json.NewDecoder(io.LimitReader(req.Body, 1<<16)).Decode(&body)
		}
		args := map[string]any{
			"source":        "web_api",
			"corpus_id":     body.CorpusID,
			"force_full":    body.ForceFull,
			"only_with_pdf": body.OnlyWithPDF,
		}
		runID := uuid.NewString()
		if _, err := pool.Exec(req.Context(), `
			INSERT INTO platform.plugin_runs (id, plugin_id, source, status, args, created_at)
			VALUES ($1, 'zotero', 'web_api', 'pending', $2, NOW())
		`, runID, mustMarshal(args)); err != nil {
			logger.WithError(err).Warn("insert plugin_runs (web_api compat)")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"job_id":   runID,
			"status":   "pending",
			"source":   "web_api",
			"redirect": "/platform/plugins/zotero/run",
		})
	}
}

// zoteroUploadSqliteHandler accepts a multipart POST and stages the
// uploaded files onto the shared dr_uploads volume. The flow:
//
//  1. Parse the multipart form with a 16 MiB in-memory buffer; bigger
//     parts spill to disk via mime/multipart's tempfile path.
//  2. Stream sqlite_file into <staged>/zotero.sqlite, hard-capped at
//     maxSqliteBytes so a hostile client can't fill the volume.
//  3. If storage_tarball is present, stream into <staged>/storage.tgz
//     then extract it into <staged>/storage/ with a traversal guard.
//  4. Persist {source: "sqlite_upload", sqlite_path, storage_dir} into
//     the zotero plugin's config so the next sync picks them up.
//  5. Write a platform.plugin_runs row with the same args + status='pending'
//     and return its id as job_id (matching the legacy response shape).
//
// We DON'T try to start the sync ourselves -- the importer.sync RPC
// supervisor hand-off isn't wired yet (Phase C+). The run row + config
// are enough for the frontend to display the job and for a future
// supervisor sweep to pick it up.
func zoteroUploadSqliteHandler(pool *pgxpool.Pool, sec *secrets.Manager, stg *stage.Stager, logger *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// Multipart parser. 16 MiB stays in RAM; bigger files spill to
		// /tmp via os.CreateTemp which the runtime cleans up on form
		// close.
		if err := req.ParseMultipartForm(multipartMemoryBudget); err != nil {
			writeJSON(w, http.StatusBadRequest,
				errorBody("parse multipart: "+err.Error()))
			return
		}
		defer func() { _ = req.MultipartForm.RemoveAll() }()

		sqliteFile, sqliteHdr, err := req.FormFile("sqlite_file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest,
				errorBody("sqlite_file is required"))
			return
		}
		defer sqliteFile.Close()

		corpusID := strings.TrimSpace(req.FormValue("corpus_id"))
		onlyWithPDF := parseBoolForm(req.FormValue("only_with_pdf"))
		forceFull := parseBoolForm(req.FormValue("force_full"))
		if corpusID != "" {
			if _, err := uuid.Parse(corpusID); err != nil {
				writeJSON(w, http.StatusBadRequest,
					errorBody("invalid corpus_id: "+err.Error()))
				return
			}
		}

		// Job dir under dr_uploads/plugins/zotero/<run_id>/. Reuse the
		// stager so the path matches what Host.StagePath would mint for
		// a fully-wired supervisor run.
		runID := uuid.NewString()
		sqlitePath, err := stg.PluginPath("zotero", runID, "zotero.sqlite", "application/octet-stream")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError,
				errorBody("stage sqlite: "+err.Error()))
			return
		}
		// Make sure the parent dir exists (stager creates it lazily on
		// Write but we're using Path + manual stream).
		if err := os.MkdirAll(filepath.Dir(sqlitePath), 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError,
				errorBody("mkdir job dir: "+err.Error()))
			return
		}

		if err := streamToFile(sqliteFile, sqlitePath, maxSqliteBytes); err != nil {
			_ = os.RemoveAll(filepath.Dir(sqlitePath))
			writeJSON(w, http.StatusBadRequest,
				errorBody("save sqlite: "+err.Error()))
			return
		}
		_ = sqliteHdr // touched only to make the linter aware we considered the metadata

		storageDir := ""
		if tarF, _, err := req.FormFile("storage_tarball"); err == nil {
			storageDir = filepath.Join(filepath.Dir(sqlitePath), "storage")
			if err := os.MkdirAll(storageDir, 0o755); err != nil {
				_ = tarF.Close()
				_ = os.RemoveAll(filepath.Dir(sqlitePath))
				writeJSON(w, http.StatusInternalServerError,
					errorBody("mkdir storage: "+err.Error()))
				return
			}
			extractErr := safeExtractTarGz(tarF, storageDir, maxStorageTarballByt, maxTarMemberBytes)
			_ = tarF.Close()
			if extractErr != nil {
				_ = os.RemoveAll(filepath.Dir(sqlitePath))
				writeJSON(w, http.StatusBadRequest,
					errorBody("extract storage tarball: "+extractErr.Error()))
				return
			}
		}

		// Persist the resolved paths into the plugin's config so the
		// next importer.sync pulls from them. We MERGE with whatever's
		// already there so api_key etc. (web_api creds) aren't wiped --
		// users can flip between sources without re-entering each.
		cfg, err := sec.Get(req.Context(), "singleton", "zotero")
		if err != nil {
			logger.WithError(err).Warn("get zotero config; using empty")
			cfg = map[string]any{}
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		cfg["source"] = "sqlite_upload"
		cfg["sqlite_path"] = sqlitePath
		if storageDir != "" {
			cfg["storage_dir"] = storageDir
		}
		if err := sec.Set(req.Context(), "singleton", "zotero", cfg); err != nil {
			logger.WithError(err).Error("persist zotero config after upload")
			// Don't fail -- the files are staged; the user can retry the
			// config update via PUT /platform/plugins/zotero/config.
		}

		args := map[string]any{
			"source":        "sqlite_upload",
			"sqlite_path":   sqlitePath,
			"storage_dir":   storageDir,
			"corpus_id":     corpusID,
			"force_full":    forceFull,
			"only_with_pdf": onlyWithPDF,
		}
		if _, err := pool.Exec(req.Context(), `
			INSERT INTO platform.plugin_runs (id, plugin_id, source, status, args, created_at)
			VALUES ($1, 'zotero', 'sqlite_upload', 'pending', $2, NOW())
		`, runID, mustMarshal(args)); err != nil {
			logger.WithError(err).Warn("insert plugin_runs (sqlite upload)")
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"job_id":        runID,
			"status":        "pending",
			"source":        "sqlite_upload",
			"sqlite_path":   sqlitePath,
			"storage_dir":   storageDir,
			"corpus_id":     corpusID,
			"only_with_pdf": onlyWithPDF,
			"force_full":    forceFull,
		})
	}
}

// streamToFile copies r to path with a byte-cap. Aborts + removes the
// file on overflow.
func streamToFile(r io.Reader, path string, max int64) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	// Limit + 1 so we can detect overflow precisely (Copy returns the
	// underlying limit hit as a short read otherwise).
	n, err := io.Copy(out, io.LimitReader(r, max+1))
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	if n > max {
		_ = os.Remove(path)
		return fmt.Errorf("upload exceeds cap (%d > %d)", n, max)
	}
	return nil
}

// safeExtractTarGz untars a gzip-compressed tarball into dst, refusing
// any entry that would resolve outside dst (path-traversal guard) and
// rejecting anything that exceeds the per-member or total budgets.
func safeExtractTarGz(src io.Reader, dst string, totalMax, memberMax int64) error {
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(io.LimitReader(src, totalMax+1))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar header: %w", err)
		}
		// Refuse absolute paths + .. components.
		cleaned := filepath.Clean(hdr.Name)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("unsafe path in tarball: %q", hdr.Name)
		}
		target := filepath.Join(absDst, cleaned)
		// Final containment check: resolved target must still be under dst.
		if !strings.HasPrefix(target, absDst+string(os.PathSeparator)) && target != absDst {
			return fmt.Errorf("unsafe path escapes dst: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size > memberMax {
				return fmt.Errorf("tar member %q exceeds cap (%d > %d)", hdr.Name, hdr.Size, memberMax)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, memberMax+1))
			_ = out.Close()
			if err != nil {
				return err
			}
			if n > memberMax {
				return fmt.Errorf("tar member %q exceeds cap on read", hdr.Name)
			}
			total += n
			if total > totalMax {
				return fmt.Errorf("tarball total size exceeds cap (%d > %d)", total, totalMax)
			}
		default:
			// Skip symlinks, devices, FIFOs -- anything non-file/dir is
			// a vector for shenanigans and Zotero's storage tree doesn't
			// contain them.
			continue
		}
	}
}

func parseBoolForm(v string) bool {
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false
	}
	return b
}

package apiserver

// Dataset endpoints. Read views over evo_datasets.{datasets,dataset_versions}
// plus a POST that registers a new dataset row and a POST that fires
// ImportDatasetWorkflow on the dataset-mgr task queue. The dataset-worker
// (evolve/dataset-manager) owns the version lifecycle; the apiserver just
// reads from postgres and kicks workflows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	dsworkflows "github.com/sirus20x6/adamaton-evolve/dataset-manager/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// datasetTaskQueue mirrors evolve/dataset-manager/cmd/dataset-worker/main.go's
// defaultTaskQueue. Hard-coded here so the apiserver doesn't depend on the
// worker binary's cmd package just to read a string constant.
const datasetTaskQueue = "dataset-mgr"

// Dataset is the wire shape returned by GET /api/v1/datasets. LatestVersion
// is the highest-version row from dataset_versions for this dataset (nil
// when the dataset was registered but never imported).
type Dataset struct {
	ID            string               `json:"id"`
	DisplayName   string               `json:"display_name"`
	Description   string               `json:"description"`
	TaskType      string               `json:"task_type"`
	CreatedAt     time.Time            `json:"created_at"`
	ArchivedAt    *time.Time           `json:"archived_at,omitempty"`
	LatestVersion *DatasetVersionBrief `json:"latest_version,omitempty"`
}

// DatasetVersionBrief is the latest-version summary embedded in Dataset.
type DatasetVersionBrief struct {
	Version     int        `json:"version"`
	Status      string     `json:"status"`
	RowCount    *int64     `json:"row_count,omitempty"`
	ByteCount   *int64     `json:"byte_count,omitempty"`
	Format      string     `json:"format"`
	CreatedAt   time.Time  `json:"created_at"`
	FinalizedAt *time.Time `json:"finalized_at,omitempty"`
}

// DatasetCreateRequest is the body for POST /api/v1/datasets.
type DatasetCreateRequest struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	TaskType    string `json:"task_type"`
}

// DatasetImportRequest is the body for POST /api/v1/datasets/import.
// SourceKind must be one of local|huggingface|kaggle; the worker rejects
// anything else.
type DatasetImportRequest struct {
	DatasetID  string `json:"dataset_id"`
	SourceKind string `json:"source_kind"`
	SourceRef  string `json:"source_ref"`
	Notes      string `json:"notes"`
}

// DatasetImportResponse is the 202 body for POST /api/v1/datasets/import.
type DatasetImportResponse struct {
	WorkflowID     string `json:"workflow_id"`
	RunID          string `json:"run_id"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
}

func (s *APIServer) registerDatasetsEndpoints(api *mux.Router) {
	// Read.
	api.HandleFunc("/datasets", s.listDatasets).Methods("GET")
	api.HandleFunc("/datasets/{id}", s.getDataset).Methods("GET")
	api.HandleFunc("/datasets/{id}/quality", s.listQualityForDataset).Methods("GET")
	api.HandleFunc("/datasets/versions/{version_id}", s.getDatasetVersion).Methods("GET")

	// Create / archive.
	api.HandleFunc("/datasets", s.createDataset).Methods("POST")
	api.HandleFunc("/datasets/{id}/archive", s.archiveDataset).Methods("POST")

	// Tag CRUD (free-form, separate from the controlled task_type).
	api.HandleFunc("/datasets/{id}/tags", s.addDatasetTag).Methods("POST")
	api.HandleFunc("/datasets/{id}/tags/{tag}", s.removeDatasetTag).Methods("DELETE")

	// Workflow-firing actions. Each POST returns 202 with a workflow_id.
	api.HandleFunc("/datasets/import", s.importDataset).Methods("POST")
	api.HandleFunc("/datasets/transform", s.transformDataset).Methods("POST")
	api.HandleFunc("/datasets/splits", s.makeSplits).Methods("POST")
	api.HandleFunc("/datasets/splits/kfold", s.makeKFoldSplits).Methods("POST")
	api.HandleFunc("/datasets/quality", s.recordQualityObservation).Methods("POST")
}

const datasetsListSQL = `
SELECT d.id, d.display_name, d.description, d.task_type, d.created_at, d.archived_at,
       v.version, v.status, v.row_count, v.byte_count, v.format, v.created_at, v.finalized_at
FROM evo_datasets.datasets d
LEFT JOIN LATERAL (
    SELECT version, status, row_count, byte_count, format, created_at, finalized_at
    FROM evo_datasets.dataset_versions
    WHERE dataset_id = d.id
    ORDER BY version DESC
    LIMIT 1
) v ON true
WHERE d.archived_at IS NULL
ORDER BY d.created_at DESC
LIMIT 500
`

func (s *APIServer) listDatasets(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.evoPool.Query(ctx, datasetsListSQL)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Dataset, 0)
	for rows.Next() {
		var d Dataset
		var ver *int
		var status, format *string
		var rowCount, byteCount *int64
		var verCreated, verFinalized *time.Time
		if err := rows.Scan(
			&d.ID, &d.DisplayName, &d.Description, &d.TaskType, &d.CreatedAt, &d.ArchivedAt,
			&ver, &status, &rowCount, &byteCount, &format, &verCreated, &verFinalized,
		); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		if ver != nil && status != nil && verCreated != nil && format != nil {
			d.LatestVersion = &DatasetVersionBrief{
				Version:     *ver,
				Status:      *status,
				RowCount:    rowCount,
				ByteCount:   byteCount,
				Format:      *format,
				CreatedAt:   *verCreated,
				FinalizedAt: verFinalized,
			}
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

// Field-validation bounds for dataset create/import. The apiserver is the
// first line of defence: a malformed ID breaks URL routing (it becomes a
// path segment in /datasets/{id}/...), and oversized free-text strings
// would otherwise be forwarded to the dataset-worker unvalidated.
const (
	// datasetIDMaxLen keeps an ID short enough to live in a URL path and a
	// Temporal workflow ID ("import-<id>-<ts>") without bloating either.
	datasetIDMaxLen      = 128
	datasetDisplayMaxLen = 256
	datasetDescMaxLen    = 4096
	datasetSourceRefMax  = 2048
	datasetNotesMaxLen   = 4096
)

// datasetIDRE constrains an ID to URL-/identifier-safe characters so it
// can be embedded in a route path and a workflow ID without escaping:
// letters, digits, dot, underscore, and dash. No slashes (would split the
// route), no spaces, no control chars.
var datasetIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateDatasetID enforces length + charset on a dataset ID. Returns a
// human-readable reason on failure (surfaced as a 400).
func validateDatasetID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	if len(id) > datasetIDMaxLen {
		return fmt.Errorf("id too long (%d > %d chars)", len(id), datasetIDMaxLen)
	}
	if !datasetIDRE.MatchString(id) {
		return errors.New("id must start with a letter or digit and contain only letters, digits, '.', '_', '-'")
	}
	return nil
}

// capLen returns an error if s exceeds max runes (counting bytes is fine
// here — these are caps, not exact UTF-8 budgets, and byte length is the
// conservative bound).
func capLen(field, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s too long (%d > %d chars)", field, len(s), max)
	}
	return nil
}

// validTaskTypes mirrors the CHECK constraint on evo_datasets.datasets.task_type.
var validTaskTypes = map[string]bool{
	"dpo":          true,
	"sft":          true,
	"eval":         true,
	"kernel-bench": true,
	"pretrain":     true,
	"other":        true,
}

func (s *APIServer) createDataset(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req DatasetCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Description = strings.TrimSpace(req.Description)
	req.TaskType = strings.TrimSpace(req.TaskType)
	if err := validateDatasetID(req.ID); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.ID
	}
	if err := capLen("display_name", req.DisplayName, datasetDisplayMaxLen); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := capLen("description", req.Description, datasetDescMaxLen); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !validTaskTypes[req.TaskType] {
		writeEvoErr(w, http.StatusBadRequest, "task_type must be one of: dpo, sft, eval, kernel-bench, pretrain, other")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const insertSQL = `
INSERT INTO evo_datasets.datasets (id, display_name, description, task_type)
VALUES ($1, $2, $3, $4)
RETURNING id, display_name, description, task_type, created_at, archived_at`
	var d Dataset
	if err := s.evoPool.QueryRow(ctx, insertSQL,
		req.ID, req.DisplayName, req.Description, req.TaskType,
	).Scan(&d.ID, &d.DisplayName, &d.Description, &d.TaskType, &d.CreatedAt, &d.ArchivedAt); err != nil {
		// pgx surfaces unique-violation as 23505; treat as 409 so the
		// frontend can offer "open existing" instead of looping retries.
		if strings.Contains(err.Error(), "23505") {
			writeEvoErr(w, http.StatusConflict, "dataset id already exists")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, d)
}

// validSourceKinds matches dsworkflows.ImportDatasetInput.SourceKind ("derived"
// is intentionally excluded — that's an internal kind used by the transform
// path, not a user-facing import source).
var validSourceKinds = map[string]bool{
	"local":       true,
	"huggingface": true,
	"kaggle":      true,
}

func (s *APIServer) importDataset(w http.ResponseWriter, r *http.Request) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req DatasetImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.DatasetID = strings.TrimSpace(req.DatasetID)
	req.SourceKind = strings.TrimSpace(req.SourceKind)
	req.SourceRef = strings.TrimSpace(req.SourceRef)
	req.Notes = strings.TrimSpace(req.Notes)
	// dataset_id is interpolated into the workflow ID ("import-<id>-<ts>")
	// and used to route — validate it the same way as create so a bad ID
	// can't break Temporal's ID constraints or the URL surface.
	if err := validateDatasetID(req.DatasetID); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "dataset_id: "+err.Error())
		return
	}
	if !validSourceKinds[req.SourceKind] {
		writeEvoErr(w, http.StatusBadRequest, "source_kind must be one of: local, huggingface, kaggle")
		return
	}
	if req.SourceRef == "" {
		writeEvoErr(w, http.StatusBadRequest, "source_ref is required")
		return
	}
	if err := capLen("source_ref", req.SourceRef, datasetSourceRefMax); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := capLen("notes", req.Notes, datasetNotesMaxLen); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}

	workflowID := "import-" + req.DatasetID + "-" + time.Now().UTC().Format("20060102T150405Z")

	// 10s on the start call — the workflow runs in Temporal once accepted;
	// we don't tie it to r.Context() since a client hangup shouldn't
	// cancel an accepted import.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	run, err := s.temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: datasetTaskQueue,
	}, dsworkflows.ImportDatasetWorkflow, dsworkflows.ImportDatasetInput{
		DatasetID:  req.DatasetID,
		SourceKind: req.SourceKind,
		SourceRef:  req.SourceRef,
		Notes:      req.Notes,
	})
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			writeEvoJSONStatus(w, http.StatusAccepted, DatasetImportResponse{
				WorkflowID:     workflowID,
				AlreadyRunning: true,
			})
			return
		}
		s.logger.WithError(err).WithField("workflow_id", workflowID).
			Error("importDataset: failed to start ImportDatasetWorkflow")
		writeEvoErr(w, http.StatusInternalServerError, "execute workflow: "+err.Error())
		return
	}

	writeEvoJSONStatus(w, http.StatusAccepted, DatasetImportResponse{
		WorkflowID: run.GetID(),
		RunID:      run.GetRunID(),
	})
}

// ─────────────────────────────────────────────────────────────────────
// Detail views — dataset, version, quality observations.
// ─────────────────────────────────────────────────────────────────────

// DatasetSource is the wire shape of one row in evo_datasets.dataset_sources.
type DatasetSource struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Ref         string          `json:"ref"`
	CommitHash  string          `json:"commit_hash"`
	License     string          `json:"license"`
	RawMetadata json.RawMessage `json:"raw_metadata"`
	FetchedAt   time.Time       `json:"fetched_at"`
}

// DatasetFile is the wire shape of one row in evo_datasets.dataset_files.
type DatasetFile struct {
	ID        string `json:"id"`
	ObjectKey string `json:"object_key"`
	FilePath  string `json:"file_path"`
	ByteSize  int64  `json:"byte_size"`
	SHA256    string `json:"sha256"`
	RowCount  *int64 `json:"row_count,omitempty"`
	Format    string `json:"format"`
}

// DatasetSplit is the wire shape of one row in evo_datasets.dataset_splits.
type DatasetSplit struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Salt       string    `json:"salt"`
	Ratios     []float32 `json:"ratios"`
	SplitNames []string  `json:"split_names"`
	KFold      int       `json:"kfold"`
	CreatedAt  time.Time `json:"created_at"`
}

// DatasetVersion is the full version detail.
type DatasetVersion struct {
	ID          string          `json:"id"`
	DatasetID   string          `json:"dataset_id"`
	Version     int             `json:"version"`
	Status      string          `json:"status"`
	RowCount    *int64          `json:"row_count,omitempty"`
	ByteCount   *int64          `json:"byte_count,omitempty"`
	Format      string          `json:"format"`
	Notes       string          `json:"notes"`
	CreatedAt   time.Time       `json:"created_at"`
	FinalizedAt *time.Time      `json:"finalized_at,omitempty"`
	Sources     []DatasetSource `json:"sources"`
	Files       []DatasetFile   `json:"files"`
	Splits      []DatasetSplit  `json:"splits"`
}

// DatasetDetail is the wire shape of GET /api/v1/datasets/{id}.
type DatasetDetail struct {
	Dataset
	Tags     []string         `json:"tags"`
	Versions []DatasetVersion `json:"versions"`
}

const datasetByIDSQL = `
SELECT id, display_name, description, task_type, created_at, archived_at
FROM evo_datasets.datasets WHERE id = $1`

const datasetVersionsByDatasetSQL = `
SELECT id, dataset_id, version, status, row_count, byte_count, format,
       notes, created_at, finalized_at
FROM evo_datasets.dataset_versions
WHERE dataset_id = $1
ORDER BY version DESC`

const datasetTagsByDatasetSQL = `
SELECT tag FROM evo_datasets.dataset_tags WHERE dataset_id = $1 ORDER BY tag ASC`

const sourcesByVersionSQL = `
SELECT id, kind, ref, commit_hash, license, raw_metadata, fetched_at
FROM evo_datasets.dataset_sources WHERE version_id = $1 ORDER BY fetched_at ASC`

const filesByVersionSQL = `
SELECT id, object_key, file_path, byte_size, sha256, row_count, format
FROM evo_datasets.dataset_files WHERE version_id = $1 ORDER BY file_path ASC`

const splitsByVersionSQL = `
SELECT id, name, salt, ratios, split_names, kfold, created_at
FROM evo_datasets.dataset_splits WHERE version_id = $1 ORDER BY created_at DESC`

func (s *APIServer) getDataset(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var d Dataset
	if err := s.evoPool.QueryRow(ctx, datasetByIDSQL, id).Scan(
		&d.ID, &d.DisplayName, &d.Description, &d.TaskType, &d.CreatedAt, &d.ArchivedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "dataset not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "select dataset: "+err.Error())
		return
	}

	tags, err := s.fetchTags(ctx, id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "select tags: "+err.Error())
		return
	}

	versions, err := s.fetchVersionsForDataset(ctx, id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "select versions: "+err.Error())
		return
	}

	writeEvoJSON(w, DatasetDetail{Dataset: d, Tags: tags, Versions: versions})
}

func (s *APIServer) getDatasetVersion(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	idStr := mux.Vars(r)["version_id"]
	vid, err := uuid.Parse(idStr)
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid version_id (must be UUID)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	const selVersionSQL = `
SELECT id, dataset_id, version, status, row_count, byte_count, format,
       notes, created_at, finalized_at
FROM evo_datasets.dataset_versions WHERE id = $1`
	var v DatasetVersion
	if err := s.evoPool.QueryRow(ctx, selVersionSQL, vid).Scan(
		&v.ID, &v.DatasetID, &v.Version, &v.Status, &v.RowCount, &v.ByteCount,
		&v.Format, &v.Notes, &v.CreatedAt, &v.FinalizedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "version not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "select version: "+err.Error())
		return
	}
	if err := s.hydrateVersion(ctx, &v); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "hydrate version: "+err.Error())
		return
	}
	writeEvoJSON(w, v)
}

func (s *APIServer) fetchTags(ctx context.Context, datasetID string) ([]string, error) {
	rows, err := s.evoPool.Query(ctx, datasetTagsByDatasetSQL, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *APIServer) fetchVersionsForDataset(ctx context.Context, datasetID string) ([]DatasetVersion, error) {
	rows, err := s.evoPool.Query(ctx, datasetVersionsByDatasetSQL, datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DatasetVersion, 0)
	for rows.Next() {
		var v DatasetVersion
		if err := rows.Scan(
			&v.ID, &v.DatasetID, &v.Version, &v.Status, &v.RowCount, &v.ByteCount,
			&v.Format, &v.Notes, &v.CreatedAt, &v.FinalizedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.hydrateVersion(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// hydrateVersion fills sources/files/splits on a partially-scanned version.
func (s *APIServer) hydrateVersion(ctx context.Context, v *DatasetVersion) error {
	v.Sources = make([]DatasetSource, 0)
	v.Files = make([]DatasetFile, 0)
	v.Splits = make([]DatasetSplit, 0)

	srcs, err := s.evoPool.Query(ctx, sourcesByVersionSQL, v.ID)
	if err != nil {
		return err
	}
	for srcs.Next() {
		var src DatasetSource
		var raw []byte
		if err := srcs.Scan(&src.ID, &src.Kind, &src.Ref, &src.CommitHash, &src.License, &raw, &src.FetchedAt); err != nil {
			srcs.Close()
			return err
		}
		if len(raw) > 0 {
			src.RawMetadata = json.RawMessage(raw)
		} else {
			src.RawMetadata = json.RawMessage("{}")
		}
		v.Sources = append(v.Sources, src)
	}
	srcs.Close()
	if err := srcs.Err(); err != nil {
		return err
	}

	files, err := s.evoPool.Query(ctx, filesByVersionSQL, v.ID)
	if err != nil {
		return err
	}
	for files.Next() {
		var f DatasetFile
		if err := files.Scan(&f.ID, &f.ObjectKey, &f.FilePath, &f.ByteSize, &f.SHA256, &f.RowCount, &f.Format); err != nil {
			files.Close()
			return err
		}
		v.Files = append(v.Files, f)
	}
	files.Close()
	if err := files.Err(); err != nil {
		return err
	}

	splits, err := s.evoPool.Query(ctx, splitsByVersionSQL, v.ID)
	if err != nil {
		return err
	}
	for splits.Next() {
		var sp DatasetSplit
		if err := splits.Scan(&sp.ID, &sp.Name, &sp.Salt, &sp.Ratios, &sp.SplitNames, &sp.KFold, &sp.CreatedAt); err != nil {
			splits.Close()
			return err
		}
		v.Splits = append(v.Splits, sp)
	}
	splits.Close()
	return splits.Err()
}

// QualityObservation is the wire shape of one quality_observations row.
type QualityObservation struct {
	ID           string          `json:"id"`
	VersionID    string          `json:"version_id"`
	Version      int             `json:"version"`
	DistillRunID string          `json:"distill_run_id"`
	EvalDelta    *float32        `json:"eval_delta,omitempty"`
	Won          *bool           `json:"won,omitempty"`
	RawMetrics   json.RawMessage `json:"raw_metrics"`
	ObservedAt   time.Time       `json:"observed_at"`
}

const qualityByDatasetSQL = `
SELECT q.id, q.version_id, v.version, q.distill_run_id, q.eval_delta, q.won, q.raw_metrics, q.observed_at
FROM evo_datasets.quality_observations q
JOIN evo_datasets.dataset_versions v ON v.id = q.version_id
WHERE v.dataset_id = $1
ORDER BY q.observed_at DESC
LIMIT 500`

func (s *APIServer) listQualityForDataset(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rows, err := s.evoPool.Query(ctx, qualityByDatasetSQL, id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]QualityObservation, 0)
	for rows.Next() {
		var q QualityObservation
		var raw []byte
		if err := rows.Scan(&q.ID, &q.VersionID, &q.Version, &q.DistillRunID, &q.EvalDelta, &q.Won, &raw, &q.ObservedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		if len(raw) > 0 {
			q.RawMetrics = json.RawMessage(raw)
		} else {
			q.RawMetrics = json.RawMessage("{}")
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

// ─────────────────────────────────────────────────────────────────────
// Mutations — archive, tags.
// ─────────────────────────────────────────────────────────────────────

func (s *APIServer) archiveDataset(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const sql = `UPDATE evo_datasets.datasets SET archived_at = NOW() WHERE id = $1 AND archived_at IS NULL`
	tag, err := s.evoPool.Exec(ctx, sql, id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeEvoErr(w, http.StatusNotFound, "dataset not found or already archived")
		return
	}
	writeEvoJSONStatus(w, http.StatusOK, map[string]string{"id": id, "status": "archived"})
}

// DatasetTagRequest is the body for POST /api/v1/datasets/{id}/tags.
type DatasetTagRequest struct {
	Tag string `json:"tag"`
}

func (s *APIServer) addDatasetTag(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var req DatasetTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		writeEvoErr(w, http.StatusBadRequest, "tag is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const sql = `INSERT INTO evo_datasets.dataset_tags (dataset_id, tag) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	if _, err := s.evoPool.Exec(ctx, sql, id, tag); err != nil {
		// FK violation = dataset doesn't exist (23503).
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusNotFound, "dataset not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert tag: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, map[string]string{"dataset_id": id, "tag": tag})
}

func (s *APIServer) removeDatasetTag(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	tag := mux.Vars(r)["tag"]
	if id == "" || tag == "" {
		writeEvoErr(w, http.StatusBadRequest, "id and tag required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const sql = `DELETE FROM evo_datasets.dataset_tags WHERE dataset_id = $1 AND tag = $2`
	if _, err := s.evoPool.Exec(ctx, sql, id, tag); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "delete tag: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────────────────────────────────────────────
// Workflow-firing actions — transform, splits, kfold, quality.
// Common patterns:
//   - validate request body
//   - mint a deterministic workflow_id so duplicate POSTs are idempotent
//     under WorkflowExecutionAlreadyStarted
//   - ExecuteWorkflow against dataset-mgr task queue
//   - return 202 with workflow_id
// ─────────────────────────────────────────────────────────────────────

// WorkflowStartResponse is the common 202 body for the workflow-firing
// action endpoints (transform, splits, kfold, quality).
type WorkflowStartResponse struct {
	WorkflowID     string `json:"workflow_id"`
	RunID          string `json:"run_id,omitempty"`
	AlreadyRunning bool   `json:"already_running,omitempty"`
}

// startWorkflow factors the boilerplate of executing one of the
// dataset-mgr workflows: 10s start timeout, 202 on success, idempotent
// 202 on AlreadyStarted, 500 with the workflow_id in the log on other
// errors.
func (s *APIServer) startWorkflow(w http.ResponseWriter, workflowID string, wf any, in any) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	run, err := s.temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: datasetTaskQueue,
	}, wf, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			writeEvoJSONStatus(w, http.StatusAccepted, WorkflowStartResponse{
				WorkflowID:     workflowID,
				AlreadyRunning: true,
			})
			return
		}
		s.logger.WithError(err).WithField("workflow_id", workflowID).Error("dataset workflow start failed")
		writeEvoErr(w, http.StatusInternalServerError, "execute workflow: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusAccepted, WorkflowStartResponse{
		WorkflowID: run.GetID(),
		RunID:      run.GetRunID(),
	})
}

func wfTimestamp() string {
	return time.Now().UTC().Format("20060102T150405Z")
}

// DatasetTransformRequest is the body for POST /api/v1/datasets/transform.
type DatasetTransformRequest struct {
	DatasetID       string `json:"dataset_id"`
	SourceVersionID string `json:"source_version_id"`
	TargetFormat    string `json:"target_format"`
	Notes           string `json:"notes"`
}

// validTransformFormats matches the workflow's accepted TargetFormat values
// (transform.go's JSONLToParquet / ParquetToJSONL registry).
var validTransformFormats = map[string]bool{
	"jsonl":   true,
	"parquet": true,
}

func (s *APIServer) transformDataset(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var req DatasetTransformRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.DatasetID = strings.TrimSpace(req.DatasetID)
	req.TargetFormat = strings.TrimSpace(req.TargetFormat)
	if req.DatasetID == "" {
		writeEvoErr(w, http.StatusBadRequest, "dataset_id is required")
		return
	}
	srcID, err := uuid.Parse(strings.TrimSpace(req.SourceVersionID))
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "source_version_id must be UUID")
		return
	}
	if !validTransformFormats[req.TargetFormat] {
		writeEvoErr(w, http.StatusBadRequest, "target_format must be one of: jsonl, parquet")
		return
	}

	workflowID := "transform-" + req.DatasetID + "-" + wfTimestamp()
	s.startWorkflow(w, workflowID, dsworkflows.TransformDatasetWorkflow, dsworkflows.TransformDatasetInput{
		DatasetID:       req.DatasetID,
		SourceVersionID: srcID,
		TargetFormat:    req.TargetFormat,
		Notes:           req.Notes,
	})
}

// DatasetMakeSplitsRequest is the body for POST /api/v1/datasets/splits.
type DatasetMakeSplitsRequest struct {
	VersionID  string    `json:"version_id"`
	Name       string    `json:"name"`
	Salt       string    `json:"salt"`
	Ratios     []float32 `json:"ratios"`
	SplitNames []string  `json:"split_names"`
}

func (s *APIServer) makeSplits(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var req DatasetMakeSplitsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	vid, err := uuid.Parse(strings.TrimSpace(req.VersionID))
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "version_id must be UUID")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeEvoErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Ratios) == 0 || len(req.Ratios) != len(req.SplitNames) {
		writeEvoErr(w, http.StatusBadRequest, "ratios and split_names must be non-empty and the same length")
		return
	}
	var sum float32
	for _, r := range req.Ratios {
		if r <= 0 {
			writeEvoErr(w, http.StatusBadRequest, "each ratio must be > 0")
			return
		}
		sum += r
	}
	// Tolerate a small drift around 1.0 — clients may build ratios from
	// percentages that don't sum exactly (e.g. 0.7 + 0.15 + 0.15 = 1.000001
	// under float32 rounding).
	if sum < 0.99 || sum > 1.01 {
		writeEvoErr(w, http.StatusBadRequest, "ratios must sum to ~1.0")
		return
	}

	workflowID := "splits-" + req.Name + "-" + vid.String()[:8] + "-" + wfTimestamp()
	s.startWorkflow(w, workflowID, dsworkflows.MakeSplitsWorkflow, dsworkflows.MakeSplitsInput{
		VersionID:  vid,
		Name:       req.Name,
		Salt:       strings.TrimSpace(req.Salt),
		Ratios:     req.Ratios,
		SplitNames: req.SplitNames,
	})
}

// DatasetMakeKFoldSplitsRequest is the body for POST /api/v1/datasets/splits/kfold.
type DatasetMakeKFoldSplitsRequest struct {
	VersionID  string `json:"version_id"`
	K          int    `json:"k"`
	Salt       string `json:"salt"`
	NamePrefix string `json:"name_prefix"`
}

func (s *APIServer) makeKFoldSplits(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var req DatasetMakeKFoldSplitsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	vid, err := uuid.Parse(strings.TrimSpace(req.VersionID))
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "version_id must be UUID")
		return
	}
	if req.K < 2 {
		writeEvoErr(w, http.StatusBadRequest, "k must be >= 2")
		return
	}
	prefix := strings.TrimSpace(req.NamePrefix)
	if prefix == "" {
		prefix = "fold"
	}

	workflowID := "kfold-" + prefix + "-" + vid.String()[:8] + "-" + wfTimestamp()
	s.startWorkflow(w, workflowID, dsworkflows.MakeKFoldSplitsWorkflow, dsworkflows.MakeKFoldSplitsInput{
		VersionID:  vid,
		K:          req.K,
		Salt:       strings.TrimSpace(req.Salt),
		NamePrefix: prefix,
	})
}

// DatasetQualityRequest is the body for POST /api/v1/datasets/quality.
// EvalDelta and Won are pointer-typed so the client can record one
// without the other (a downstream eval may produce a delta but no
// pairwise verdict, or vice versa).
type DatasetQualityRequest struct {
	VersionID    string                 `json:"version_id"`
	DistillRunID string                 `json:"distill_run_id"`
	EvalDelta    *float32               `json:"eval_delta,omitempty"`
	Won          *bool                  `json:"won,omitempty"`
	RawMetrics   map[string]interface{} `json:"raw_metrics,omitempty"`
}

func (s *APIServer) recordQualityObservation(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req DatasetQualityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	vid, err := uuid.Parse(strings.TrimSpace(req.VersionID))
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "version_id must be UUID")
		return
	}
	rid, err := uuid.Parse(strings.TrimSpace(req.DistillRunID))
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "distill_run_id must be UUID")
		return
	}
	if req.EvalDelta == nil && req.Won == nil && len(req.RawMetrics) == 0 {
		writeEvoErr(w, http.StatusBadRequest, "at least one of eval_delta, won, raw_metrics must be set")
		return
	}

	workflowID := "quality-" + vid.String()[:8] + "-" + rid.String()[:8] + "-" + wfTimestamp()
	s.startWorkflow(w, workflowID, dsworkflows.RecordQualityObservationWorkflow, dsworkflows.RecordQualityObservationInput{
		DatasetVersionID: vid,
		DistillRunID:     rid,
		EvalDelta:        req.EvalDelta,
		Won:              req.Won,
		RawMetrics:       req.RawMetrics,
	})
}

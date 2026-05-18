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
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
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
	api.HandleFunc("/datasets", s.listDatasets).Methods("GET")
	api.HandleFunc("/datasets", s.createDataset).Methods("POST")
	api.HandleFunc("/datasets/import", s.importDataset).Methods("POST")
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
	req.TaskType = strings.TrimSpace(req.TaskType)
	if req.ID == "" {
		writeEvoErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.ID
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
	if req.DatasetID == "" {
		writeEvoErr(w, http.StatusBadRequest, "dataset_id is required")
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

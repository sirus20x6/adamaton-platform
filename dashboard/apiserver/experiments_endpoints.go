package apiserver

// Experiments endpoints. Mounted at /platform/experiments/* on the top
// router (not under /api/v1) so the existing React app at
// frontend/src/api/experiments.ts works unchanged: the Vite proxy + pi5
// Caddy forward /platform/experiments to this apiserver.
//
// Reads use this pool + the matching PostgREST view (GET /db/experiments)
// interchangeably; the React app already reads via PostgREST. The handlers
// here cover the write + search + metrics surface that PostgREST can't
// model (insert with NOTNULL defaults, similarity-ranked search, bulk
// pivoted metrics, session rollup).
//
// JSON shapes mirror frontend/src/api/experiments.ts exactly. Any change
// here is a breaking API change for the SPA.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
)

// validExperimentStatus mirrors the CHECK constraint on platform.experiments.status.
var validExperimentStatus = map[string]bool{
	"pending":     true,
	"running":     true,
	"succeeded":   true,
	"failed":      true,
	"interrupted": true,
}

// Experiment is the wire shape for a single row. Mirrors the TS `Experiment`
// type in frontend/src/api/experiments.ts. Pointer/optional fields use
// JSON null on the wire to match the TS `T | null` shape.
type Experiment struct {
	ID               string          `json:"id"`
	AgentSessionID   *string         `json:"agent_session_id"`
	Name             string          `json:"name"`
	Hypothesis       string          `json:"hypothesis"`
	CodeDiff         *string         `json:"code_diff"`
	ValBPB           *float64        `json:"val_bpb"`
	PeakMemoryMB     *int            `json:"peak_memory_mb"`
	Status           string          `json:"status"`
	Notes            *string         `json:"notes"`
	ParentID         *string         `json:"parent_id"`
	CommitHash       *string         `json:"commit_hash"`
	Tags             json.RawMessage `json:"tags"`
	StartedAt        time.Time       `json:"started_at"`
	FinishedAt       *time.Time      `json:"finished_at"`
	CreatedAt        time.Time       `json:"created_at"`
	DatasetVersionID *string         `json:"dataset_version_id,omitempty"`
	Split            *string         `json:"split,omitempty"`
	ScheduleID       *string         `json:"schedule_id,omitempty"`
	WorkflowID       *string         `json:"workflow_id,omitempty"`
}

// ExperimentCreateRequest is the POST body. agent_session_id is optional
// (Manual UI creations leave it null; agent-driven creations stamp it).
type ExperimentCreateRequest struct {
	Name             string          `json:"name"`
	Hypothesis       string          `json:"hypothesis"`
	CodeDiff         *string         `json:"code_diff,omitempty"`
	ValBPB           *float64        `json:"val_bpb,omitempty"`
	PeakMemoryMB     *int            `json:"peak_memory_mb,omitempty"`
	Status           *string         `json:"status,omitempty"`
	Notes            *string         `json:"notes,omitempty"`
	ParentID         *string         `json:"parent_id,omitempty"`
	CommitHash       *string         `json:"commit_hash,omitempty"`
	Tags             json.RawMessage `json:"tags,omitempty"`
	AgentSessionID   *string         `json:"agent_session_id,omitempty"`
	DatasetVersionID *string         `json:"dataset_version_id,omitempty"`
	Split            *string         `json:"split,omitempty"`
}

// ExperimentUpdateRequest is the PATCH body — every field optional. A nil
// pointer leaves the column untouched; a pointer to nil/empty clears it.
// FinishedAt is on the update path only (callers mark the run done).
type ExperimentUpdateRequest struct {
	Name             *string         `json:"name,omitempty"`
	Hypothesis       *string         `json:"hypothesis,omitempty"`
	CodeDiff         *string         `json:"code_diff,omitempty"`
	ValBPB           *float64        `json:"val_bpb,omitempty"`
	PeakMemoryMB     *int            `json:"peak_memory_mb,omitempty"`
	Status           *string         `json:"status,omitempty"`
	Notes            *string         `json:"notes,omitempty"`
	ParentID         *string         `json:"parent_id,omitempty"`
	CommitHash       *string         `json:"commit_hash,omitempty"`
	Tags             json.RawMessage `json:"tags,omitempty"`
	FinishedAt       *time.Time      `json:"finished_at,omitempty"`
	DatasetVersionID *string         `json:"dataset_version_id,omitempty"`
	Split            *string         `json:"split,omitempty"`
}

// ExperimentSearchHit is the trimmed-row shape used by search. hypothesis_preview
// is left-truncated to 200 chars so the typeahead doesn't ship multi-KB bodies.
type ExperimentSearchHit struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	HypothesisPreview string    `json:"hypothesis_preview"`
	ValBPB            *float64  `json:"val_bpb"`
	Status            string    `json:"status"`
	StartedAt         time.Time `json:"started_at"`
	Similarity        float64   `json:"similarity"`
}

// ExperimentSearchResponse wraps hits in an object so callers can extend
// without breaking the array contract (per the existing TS shape).
type ExperimentSearchResponse struct {
	Hits []ExperimentSearchHit `json:"hits"`
}

// MetricPointWire is a single (step, value) sample for one (experiment, key).
type MetricPointWire struct {
	Step       int       `json:"step"`
	Key        string    `json:"key"`
	Value      float64   `json:"value"`
	RecordedAt time.Time `json:"recorded_at"`
}

// SingleMetricsResponse is the per-experiment metrics envelope. key is nil
// when the caller didn't filter by key.
type SingleMetricsResponse struct {
	ExperimentID string            `json:"experiment_id"`
	Key          *string           `json:"key"`
	Points       []MetricPointWire `json:"points"`
	Count        int               `json:"count"`
}

// BulkMetricPoint is the per-point shape used inside BulkMetricsSeries.
// recorded_at is nullable so the frontend can survive partially-filled rows.
type BulkMetricPoint struct {
	Step       int        `json:"step"`
	Value      float64    `json:"value"`
	RecordedAt *time.Time `json:"recorded_at"`
}

// BulkMetricsResponse is the multi-experiment pivot. series is an empty
// object (not nil) when nothing matches so JSON.parse sees {} not null.
type BulkMetricsResponse struct {
	Series    map[string]map[string][]BulkMetricPoint `json:"series"`
	Count     int                                     `json:"count"`
	SinceStep *int                                    `json:"since_step"`
}

// SessionSummary aggregates across all experiments in one agent_session.
type SessionSummary struct {
	Count          int        `json:"count"`
	BestValBPB     *float64   `json:"best_val_bpb"`
	LatestValBPB   *float64   `json:"latest_val_bpb"`
	StartedAt      *time.Time `json:"started_at"`
	LastActiveAt   *time.Time `json:"last_active_at"`
}

// registerExperimentsEndpoints mounts everything on the top router under
// /platform/experiments so the existing frontend contract works unchanged.
// Caddyfile must route /platform/experiments* to this apiserver (the old
// route to backend:7272 is dead since the autoresearch FastAPI was retired).
func (s *APIServer) registerExperimentsEndpoints() {
	r := s.router

	// Writes.
	r.HandleFunc("/platform/experiments", s.createExperiment).Methods("POST")
	r.HandleFunc("/platform/experiments/", s.createExperiment).Methods("POST")
	r.HandleFunc("/platform/experiments/{id}", s.updateExperiment).Methods("PATCH")

	// Search — declared BEFORE the {id}/metrics catch-all so "search" doesn't
	// match as an experiment id and 404.
	r.HandleFunc("/platform/experiments/search", s.searchExperiments).Methods("GET")
	r.HandleFunc("/platform/experiments/metrics", s.bulkExperimentMetrics).Methods("GET")
	r.HandleFunc("/platform/experiments/sessions/{sid}/summary", s.experimentSessionSummary).Methods("GET")

	// Per-experiment metrics. Mounted last so "search"/"metrics"/"sessions"
	// have already been claimed.
	r.HandleFunc("/platform/experiments/{id}/metrics", s.experimentMetrics).Methods("GET")
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// readExperimentRow scans the canonical SELECT * row order into Experiment.
// Shared between create/update RETURNING and any future single-row reads.
const experimentColumns = `
id, agent_session_id, name, hypothesis, code_diff, val_bpb,
peak_memory_mb, status, notes, parent_id, commit_hash, tags,
started_at, finished_at, created_at,
dataset_version_id, split, schedule_id, workflow_id`

func scanExperiment(row pgx.Row) (Experiment, error) {
	var e Experiment
	var tagsRaw []byte
	err := row.Scan(
		&e.ID, &e.AgentSessionID, &e.Name, &e.Hypothesis, &e.CodeDiff, &e.ValBPB,
		&e.PeakMemoryMB, &e.Status, &e.Notes, &e.ParentID, &e.CommitHash, &tagsRaw,
		&e.StartedAt, &e.FinishedAt, &e.CreatedAt,
		&e.DatasetVersionID, &e.Split, &e.ScheduleID, &e.WorkflowID,
	)
	if err != nil {
		return e, err
	}
	if len(tagsRaw) > 0 {
		e.Tags = json.RawMessage(tagsRaw)
	} else {
		e.Tags = json.RawMessage("[]")
	}
	return e, nil
}

// parseUUID returns the parsed UUID and a 400-friendly error when the input
// isn't a valid UUID. Use for caller-supplied ids before hitting the DB so
// the response says "bad id" instead of pgx's verbose parse error.
func parseUUID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid uuid: %s", raw)
	}
	return id, nil
}

/* ── POST /platform/experiments ────────────────────────────────────────── */

func (s *APIServer) createExperiment(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // code_diff can be sizeable
	var req ExperimentCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Hypothesis = strings.TrimSpace(req.Hypothesis)
	if req.Name == "" {
		writeEvoErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Hypothesis == "" {
		writeEvoErr(w, http.StatusBadRequest, "hypothesis is required")
		return
	}
	status := "pending"
	if req.Status != nil && *req.Status != "" {
		if !validExperimentStatus[*req.Status] {
			writeEvoErr(w, http.StatusBadRequest, "status must be one of: pending, running, succeeded, failed, interrupted")
			return
		}
		status = *req.Status
	}
	// agent_session_id, parent_id, dataset_version_id are UUIDs when present.
	for _, p := range []struct {
		name string
		val  *string
	}{
		{"agent_session_id", req.AgentSessionID},
		{"parent_id", req.ParentID},
		{"dataset_version_id", req.DatasetVersionID},
	} {
		if p.val != nil && *p.val != "" {
			if _, err := parseUUID(*p.val); err != nil {
				writeEvoErr(w, http.StatusBadRequest, p.name+": "+err.Error())
				return
			}
		}
	}
	tags := req.Tags
	if len(tags) == 0 {
		tags = json.RawMessage("[]")
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const insertSQL = `
INSERT INTO platform.experiments
    (agent_session_id, name, hypothesis, code_diff, val_bpb, peak_memory_mb,
     status, notes, parent_id, commit_hash, tags, dataset_version_id, split)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13)
RETURNING ` + experimentColumns
	row := s.evoPool.QueryRow(ctx, insertSQL,
		req.AgentSessionID, req.Name, req.Hypothesis, req.CodeDiff,
		req.ValBPB, req.PeakMemoryMB, status, req.Notes, req.ParentID,
		req.CommitHash, string(tags), req.DatasetVersionID, req.Split,
	)
	exp, err := scanExperiment(row)
	if err != nil {
		// FK violation on dataset_version_id or parent_id → 400, not 500.
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusBadRequest, "foreign key violation: "+err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	writeEvoJSONStatus(w, http.StatusCreated, exp)
}

/* ── PATCH /platform/experiments/{id} ──────────────────────────────────── */

func (s *APIServer) updateExperiment(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id, err := parseUUID(mux.Vars(r)["id"])
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req ExperimentUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Status != nil && *req.Status != "" && !validExperimentStatus[*req.Status] {
		writeEvoErr(w, http.StatusBadRequest, "status must be one of: pending, running, succeeded, failed, interrupted")
		return
	}

	// Build SET clause dynamically — only present fields get updated.
	sets := make([]string, 0, 12)
	args := make([]any, 0, 12)
	add := func(col string, v any) {
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)+1))
		args = append(args, v)
	}
	if req.Name != nil {
		add("name", *req.Name)
	}
	if req.Hypothesis != nil {
		add("hypothesis", *req.Hypothesis)
	}
	if req.CodeDiff != nil {
		add("code_diff", *req.CodeDiff)
	}
	if req.ValBPB != nil {
		add("val_bpb", *req.ValBPB)
	}
	if req.PeakMemoryMB != nil {
		add("peak_memory_mb", *req.PeakMemoryMB)
	}
	if req.Status != nil {
		add("status", *req.Status)
	}
	if req.Notes != nil {
		add("notes", *req.Notes)
	}
	if req.ParentID != nil {
		if *req.ParentID != "" {
			if _, err := parseUUID(*req.ParentID); err != nil {
				writeEvoErr(w, http.StatusBadRequest, "parent_id: "+err.Error())
				return
			}
		}
		add("parent_id", req.ParentID)
	}
	if req.CommitHash != nil {
		add("commit_hash", *req.CommitHash)
	}
	if len(req.Tags) > 0 {
		add("tags", string(req.Tags))
	}
	if req.FinishedAt != nil {
		add("finished_at", *req.FinishedAt)
	}
	if req.DatasetVersionID != nil {
		if *req.DatasetVersionID != "" {
			if _, err := parseUUID(*req.DatasetVersionID); err != nil {
				writeEvoErr(w, http.StatusBadRequest, "dataset_version_id: "+err.Error())
				return
			}
		}
		add("dataset_version_id", req.DatasetVersionID)
	}
	if req.Split != nil {
		add("split", *req.Split)
	}
	if len(sets) == 0 {
		writeEvoErr(w, http.StatusBadRequest, "no fields to update")
		return
	}
	args = append(args, id)
	updateSQL := fmt.Sprintf(
		"UPDATE platform.experiments SET %s WHERE id = $%d RETURNING %s",
		strings.Join(sets, ", "), len(args), experimentColumns,
	)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, updateSQL, args...)
	exp, err := scanExperiment(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "experiment not found")
			return
		}
		if strings.Contains(err.Error(), "23503") {
			writeEvoErr(w, http.StatusBadRequest, "foreign key violation: "+err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	writeEvoJSON(w, exp)
}

/* ── GET /platform/experiments/search ──────────────────────────────────── */

func (s *APIServer) searchExperiments(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeEvoJSON(w, ExperimentSearchResponse{Hits: []ExperimentSearchHit{}})
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	sessFilter := strings.TrimSpace(r.URL.Query().Get("agent_session_id"))
	var sessUUID *uuid.UUID
	if sessFilter != "" {
		id, err := parseUUID(sessFilter)
		if err != nil {
			writeEvoErr(w, http.StatusBadRequest, "agent_session_id: "+err.Error())
			return
		}
		sessUUID = &id
	}

	// Use the trigram similarity operator + GREATEST(similarity(name,q),
	// similarity(hypothesis,q)) as the rank score. The trgm GIN indexes on
	// name + hypothesis make the `%` op cheap.
	const sql = `
SELECT id, name, LEFT(hypothesis, 200), val_bpb, status, started_at,
       GREATEST(similarity(name, $1), similarity(hypothesis, $1)) AS sim
FROM platform.experiments
WHERE (name % $1 OR hypothesis % $1)
  AND ($2::uuid IS NULL OR agent_session_id = $2)
ORDER BY sim DESC, val_bpb ASC NULLS LAST, created_at DESC
LIMIT $3`

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.evoPool.Query(ctx, sql, q, sessUUID, limit)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	hits := make([]ExperimentSearchHit, 0)
	for rows.Next() {
		var h ExperimentSearchHit
		if err := rows.Scan(&h.ID, &h.Name, &h.HypothesisPreview, &h.ValBPB, &h.Status, &h.StartedAt, &h.Similarity); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, ExperimentSearchResponse{Hits: hits})
}

/* ── GET /platform/experiments/{id}/metrics ────────────────────────────── */

func (s *APIServer) experimentMetrics(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id, err := parseUUID(mux.Vars(r)["id"])
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(r.URL.Query().Get("key"))

	const baseSQL = `
SELECT step, key, value, recorded_at
FROM platform.experiment_metrics
WHERE experiment_id = $1
  AND ($2::text IS NULL OR key = $2)
ORDER BY key ASC, step ASC`

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var keyArg any
	if key != "" {
		keyArg = key
	}
	rows, err := s.evoPool.Query(ctx, baseSQL, id, keyArg)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := SingleMetricsResponse{
		ExperimentID: id.String(),
		Points:       make([]MetricPointWire, 0),
	}
	if key != "" {
		k := key
		out.Key = &k
	}
	for rows.Next() {
		var p MetricPointWire
		if err := rows.Scan(&p.Step, &p.Key, &p.Value, &p.RecordedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out.Points = append(out.Points, p)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	out.Count = len(out.Points)
	writeEvoJSON(w, out)
}

/* ── GET /platform/experiments/metrics?ids=…&keys=…&since_step=… ───────── */

func (s *APIServer) bulkExperimentMetrics(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	idsRaw := strings.TrimSpace(r.URL.Query().Get("ids"))
	if idsRaw == "" {
		writeEvoJSON(w, BulkMetricsResponse{Series: map[string]map[string][]BulkMetricPoint{}, Count: 0})
		return
	}
	ids := make([]uuid.UUID, 0, 16)
	for _, raw := range strings.Split(idsRaw, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, err := parseUUID(raw)
		if err != nil {
			writeEvoErr(w, http.StatusBadRequest, "ids: "+err.Error())
			return
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeEvoJSON(w, BulkMetricsResponse{Series: map[string]map[string][]BulkMetricPoint{}, Count: 0})
		return
	}

	var keys []string
	if raw := strings.TrimSpace(r.URL.Query().Get("keys")); raw != "" {
		for _, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(k)
			if k != "" {
				keys = append(keys, k)
			}
		}
	}
	var sinceStep *int
	if raw := strings.TrimSpace(r.URL.Query().Get("since_step")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeEvoErr(w, http.StatusBadRequest, "since_step: not an integer")
			return
		}
		sinceStep = &n
	}

	// One query, pivot client-side. The (experiment_id, key, step) index
	// makes this an ordered range scan even with the IN + ANY filters.
	const sql = `
SELECT experiment_id, step, key, value, recorded_at
FROM platform.experiment_metrics
WHERE experiment_id = ANY($1)
  AND (cardinality($2::text[]) = 0 OR key = ANY($2))
  AND ($3::int IS NULL OR step > $3)
ORDER BY experiment_id, key, step`

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, err := s.evoPool.Query(ctx, sql, ids, keys, sinceStep)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	series := make(map[string]map[string][]BulkMetricPoint)
	count := 0
	for rows.Next() {
		var eid, k string
		var p BulkMetricPoint
		var recAt time.Time
		if err := rows.Scan(&eid, &p.Step, &k, &p.Value, &recAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		recCopy := recAt
		p.RecordedAt = &recCopy
		bucket, ok := series[eid]
		if !ok {
			bucket = make(map[string][]BulkMetricPoint)
			series[eid] = bucket
		}
		bucket[k] = append(bucket[k], p)
		count++
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, BulkMetricsResponse{Series: series, Count: count, SinceStep: sinceStep})
}

/* ── GET /platform/experiments/sessions/{sid}/summary ──────────────────── */

func (s *APIServer) experimentSessionSummary(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	sid, err := parseUUID(mux.Vars(r)["sid"])
	if err != nil {
		writeEvoErr(w, http.StatusBadRequest, "sid: "+err.Error())
		return
	}

	// latest_val_bpb is the val_bpb of the most recently created experiment
	// in the session that has a non-null score; best is the minimum across
	// the whole session. last_active_at picks the latest of finished_at /
	// started_at / created_at per row, then aggregates.
	const sql = `
WITH e AS (
    SELECT val_bpb, started_at, created_at, finished_at
    FROM platform.experiments
    WHERE agent_session_id = $1
)
SELECT
    (SELECT COUNT(*) FROM e)                                    AS cnt,
    (SELECT MIN(val_bpb) FROM e WHERE val_bpb IS NOT NULL)      AS best,
    (
        SELECT val_bpb FROM e
        WHERE val_bpb IS NOT NULL
        ORDER BY created_at DESC
        LIMIT 1
    )                                                            AS latest,
    (SELECT MIN(started_at)                          FROM e)    AS started_at,
    (SELECT MAX(GREATEST(
        COALESCE(finished_at, started_at, created_at),
        COALESCE(started_at, created_at),
        created_at
    ))                                                FROM e)   AS last_active`

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var out SessionSummary
	if err := s.evoPool.QueryRow(ctx, sql, sid).Scan(
		&out.Count, &out.BestValBPB, &out.LatestValBPB, &out.StartedAt, &out.LastActiveAt,
	); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

// Worker registry endpoints used by the Nodes UI to surface the
// self-registered worker grid. Reads are best-effort against evo.workers
// — the table is populated by the worker-registry sidecar on each
// worker, and the dashboard never writes to it.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
)

// Worker is the wire shape returned by /api/v1/workers and
// /api/v1/workers/{id}. Nullable hardware columns use pointers so the
// JSON output collapses to null / omitted when a worker hasn't
// advertised that capability yet (e.g. CPU-only nodes have no GPU
// fields). DeclaredQueues / CPUFeatures / Permissions never serialize
// as null — they default to empty arrays so the UI's .map() calls don't
// blow up on a fresh registration.
type Worker struct {
	ID             string    `json:"id"`
	Identity       string    `json:"identity"`
	Hostname       string    `json:"hostname"`
	TailscaleIP    *string   `json:"tailscale_ip,omitempty"`
	DeclaredQueues []string  `json:"declared_queues"`
	CPUArch        *string   `json:"cpu_arch,omitempty"`
	CPUFeatures    []string  `json:"cpu_features"`
	CPUCount       *int      `json:"cpu_count,omitempty"`
	RAMGB          *int      `json:"ram_gb,omitempty"`
	GPUModel       *string   `json:"gpu_model,omitempty"`
	GPUCount       *int      `json:"gpu_count,omitempty"`
	GPUVRAMGB      *int      `json:"gpu_vram_gb,omitempty"`
	DriverVersion  *string   `json:"driver_version,omitempty"`
	Permissions    []string  `json:"permissions"`
	Status         string    `json:"status"`
	LastHeartbeat  time.Time `json:"last_heartbeat"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	// Live host telemetry sampled on each heartbeat tick. Null when the
	// worker is on a non-Linux host, when the very first tick hasn't
	// landed yet (CPU% needs a delta), or when the /proc probe failed.
	// Migration 0010 adds the underlying columns.
	CPUPct        *float64  `json:"cpu_pct,omitempty"`
	RAMUsedGB     *float64  `json:"ram_used_gb,omitempty"`
	LoadAvg1m     *float64  `json:"load_avg_1m,omitempty"`
	JobsAssigned   int       `json:"jobs_assigned"`
	JobsCompleted  int       `json:"jobs_completed"`
}

// registerWorkerEndpoints mounts the read-only worker views. Wired
// into the /api/v1 subrouter by server.go's setupRoutes.
func (s *APIServer) registerWorkerEndpoints(api *mux.Router) {
	api.HandleFunc("/workers", s.listWorkers).Methods("GET")
	api.HandleFunc("/workers/{id}", s.getWorker).Methods("GET")
}

// workersSelectSQL is the shared column list + LATERAL joins. The
// joins compute per-worker job counts so the UI can show "3 running /
// 142 completed" without a second round-trip. Status ordering puts
// healthy workers first ('online' < 'stale' alphabetically is the
// pleasant accident we lean on; an explicit CASE would only matter
// once more statuses land).
const workersSelectSQL = `
SELECT w.id, w.identity, w.hostname, w.tailscale_ip, w.declared_queues,
       w.cpu_arch, w.cpu_features, w.cpu_count, w.ram_gb,
       w.gpu_model, w.gpu_count, w.gpu_vram_gb, w.driver_version,
       w.permissions, w.status, w.last_heartbeat, w.first_seen, w.last_seen,
       w.cpu_pct, w.ram_used_gb, w.load_avg_1m,
       COALESCE(a.cnt, 0) AS jobs_assigned,
       COALESCE(c.cnt, 0) AS jobs_completed
FROM evo.workers w
LEFT JOIN LATERAL (
  SELECT COUNT(*) AS cnt FROM evo.jobs
  WHERE assigned_worker = w.id AND status IN ('assigned', 'running')
) a ON TRUE
LEFT JOIN LATERAL (
  SELECT COUNT(*) AS cnt FROM evo.jobs
  WHERE assigned_worker = w.id AND status = 'succeeded'
) c ON TRUE
`

func (s *APIServer) listWorkers(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Default of 200 preserves the historical "no limit, but realistic
	// fleet size" behaviour while still bounding hostile queries.
	limit, offset := parseLimitOffset(r, 200, 500, 100000)
	rows, err := s.evoPool.Query(ctx, workersSelectSQL+`
		ORDER BY w.status ASC, w.identity ASC, w.hostname ASC
		LIMIT $1 OFFSET $2
	`, limit, offset)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Worker, 0)
	for rows.Next() {
		wk, err := scanWorker(rows)
		if err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, wk)
	}
	if err := rows.Err(); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeEvoJSON(w, out)
}

func (s *APIServer) getWorker(w http.ResponseWriter, r *http.Request) {
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

	row := s.evoPool.QueryRow(ctx, workersSelectSQL+` WHERE w.id = $1`, id)
	wk, err := scanWorker(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "worker not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
		return
	}
	writeEvoJSON(w, wk)
}

// scanWorker consumes one row (pgx.Row or pgx.Rows) and produces a
// Worker. Centralised so list + get keep their column lists in lockstep.
// Empty arrays are normalised here so the JSON encoder never emits
// "declared_queues": null — the UI maps over these unconditionally.
func scanWorker(rs rowScanner) (Worker, error) {
	var wk Worker
	if err := rs.Scan(
		&wk.ID, &wk.Identity, &wk.Hostname, &wk.TailscaleIP, &wk.DeclaredQueues,
		&wk.CPUArch, &wk.CPUFeatures, &wk.CPUCount, &wk.RAMGB,
		&wk.GPUModel, &wk.GPUCount, &wk.GPUVRAMGB, &wk.DriverVersion,
		&wk.Permissions, &wk.Status, &wk.LastHeartbeat, &wk.FirstSeen, &wk.LastSeen,
		&wk.CPUPct, &wk.RAMUsedGB, &wk.LoadAvg1m,
		&wk.JobsAssigned, &wk.JobsCompleted,
	); err != nil {
		return Worker{}, err
	}
	if wk.DeclaredQueues == nil {
		wk.DeclaredQueues = []string{}
	}
	if wk.CPUFeatures == nil {
		wk.CPUFeatures = []string{}
	}
	if wk.Permissions == nil {
		wk.Permissions = []string{}
	}
	return wk, nil
}
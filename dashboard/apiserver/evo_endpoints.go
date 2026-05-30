// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EvoRun is one row in the /api/v1/evo/runs list.
type EvoRun struct {
	ID          string     `json:"id"`
	TaskID      string     `json:"task_id"`
	TaskName    string     `json:"task_name"`
	Domain      string     `json:"domain"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	NumPrograms int        `json:"num_programs"`
	BestSpeedup *float64   `json:"best_speedup,omitempty"`
}

// EvoProgram is one row in /api/v1/evo/runs/{id}/programs.
type EvoProgram struct {
	ID         int64                  `json:"id"`
	ParentID   *int64                 `json:"parent_id,omitempty"`
	Generation int                    `json:"generation"`
	Island     int                    `json:"island"`
	Compiled   bool                   `json:"compiled"`
	Correct    bool                   `json:"correct"`
	Speedup    *float64               `json:"speedup,omitempty"`
	Backend    string                 `json:"backend"`
	CreatedAt  time.Time              `json:"created_at"`
	Metrics    map[string]interface{} `json:"metrics,omitempty"`
}

// EvoInsight is one row in /api/v1/evo/insights.
type EvoInsight struct {
	ID              int64     `json:"id"`
	Domain          string    `json:"domain"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	Tags            []string  `json:"tags"`
	HasEmbedding    bool      `json:"has_embedding"`
	SourceProgramID *int64    `json:"source_program_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// registerEvoEndpoints adds the /api/v1/evo/* routes. Called from
// server.go's setupRoutes when an evoPool is configured; the routes
// silently do nothing when it's nil.
func (s *APIServer) registerEvoEndpoints(api *mux.Router) {
	api.HandleFunc("/evo/runs", s.handleEvoRuns).Methods("GET")
	api.HandleFunc("/evo/runs/{id}/programs", s.handleEvoRunPrograms).Methods("GET")
	api.HandleFunc("/evo/insights", s.handleEvoInsights).Methods("GET")
	s.registerEvoRunCreate(api)
}

func (s *APIServer) handleEvoRuns(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit, offset := parseLimitOffset(r, 50, 500, 100000)
	// LEFT JOIN on evo.tasks so a run whose task row was deleted still
	// appears in the listing (previously an INNER JOIN silently dropped
	// these — the UI would just lose the row with no explanation). The
	// COALESCE pins a recognisable placeholder for the deleted-task case.
	const q = `
		SELECT r.id, r.task_id,
		       COALESCE(t.name, '<deleted>') AS task_name,
		       COALESCE(t.domain, '<deleted>') AS domain,
		       r.status,
		       r.started_at, r.finished_at,
		       coalesce(p.cnt, 0) AS num_programs,
		       p.best_speedup
		FROM evo.runs r
		LEFT JOIN evo.tasks t ON t.id = r.task_id
		LEFT JOIN (
			SELECT run_id, count(*) AS cnt, max(speedup) AS best_speedup
			FROM evo.programs
			GROUP BY run_id
		) p ON p.run_id = r.id
		ORDER BY r.started_at DESC
		LIMIT $1 OFFSET $2
	`
	rows, err := s.evoPool.Query(ctx, q, limit, offset)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]EvoRun, 0, limit)
	for rows.Next() {
		var rn EvoRun
		if err := rows.Scan(&rn.ID, &rn.TaskID, &rn.TaskName, &rn.Domain, &rn.Status,
			&rn.StartedAt, &rn.FinishedAt, &rn.NumPrograms, &rn.BestSpeedup); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, rn)
	}
	writeEvoJSON(w, out)
}

func (s *APIServer) handleEvoRunPrograms(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "run id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit, offset := parseLimitOffset(r, 1000, 500, 100000)
	// Note: 500-row cap is intentional even though the historical default
	// is 1000 — once a caller opts in to ?limit= they shouldn't be able
	// to ask for more rows than the rest of the API allows. The default
	// (no ?limit) still returns up to 1000 rows for legacy callers.
	const q = `
		SELECT id, parent_id, generation, island,
		       compiled, correct, speedup, backend, created_at, metrics
		FROM evo.programs
		WHERE run_id = $1
		ORDER BY generation ASC, id ASC
		LIMIT $2 OFFSET $3
	`
	rows, err := s.evoPool.Query(ctx, q, id, limit, offset)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]EvoProgram, 0)
	for rows.Next() {
		var p EvoProgram
		var metricsJSON []byte
		if err := rows.Scan(&p.ID, &p.ParentID, &p.Generation, &p.Island,
			&p.Compiled, &p.Correct, &p.Speedup, &p.Backend, &p.CreatedAt, &metricsJSON); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		if len(metricsJSON) > 0 {
			_ = json.Unmarshal(metricsJSON, &p.Metrics)
		}
		out = append(out, p)
	}
	writeEvoJSON(w, out)
}

func (s *APIServer) handleEvoInsights(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	const q = `
		SELECT id, domain, title, body, tags,
		       embedding IS NOT NULL AS has_embedding,
		       source_program_id, created_at
		FROM evo.insights
		ORDER BY created_at DESC
		LIMIT 50
	`
	rows, err := s.evoPool.Query(ctx, q)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]EvoInsight, 0)
	for rows.Next() {
		var i EvoInsight
		if err := rows.Scan(&i.ID, &i.Domain, &i.Title, &i.Body, &i.Tags,
			&i.HasEmbedding, &i.SourceProgramID, &i.CreatedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, i)
	}
	writeEvoJSON(w, out)
}

func writeEvoJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeEvoJSONStatus sets Content-Type, writes the status code, then
// encodes the body. Order matters — WriteHeader after a body Write
// would emit a default 200 first. Callers that need a non-200
// successful response (e.g. 202 Accepted on /jobs/submit) should use
// this helper rather than calling w.WriteHeader+writeEvoJSON.
func writeEvoJSONStatus(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeEvoErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseLimitOffset reads ?limit=N and ?offset=M from the request,
// applying sensible bounds. Defaults preserve the caller's historic
// page size when the params are absent. Both values are clamped:
// limit to [1, maxLimit] and offset to [0, maxOffset], so a malformed
// or hostile query string can't drag the database into a multi-million
// row scan.
func parseLimitOffset(r *http.Request, defLimit, maxLimit, maxOffset int) (limit, offset int) {
	limit = defLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxLimit {
				n = maxLimit
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			if n > maxOffset {
				n = maxOffset
			}
			offset = n
		}
	}
	return limit, offset
}

// evoPoolType exists solely so APIServer can declare its field
// without importing pgxpool everywhere — keeps the surface small.
// Use the alias directly in server.go.
type evoPoolType = *pgxpool.Pool

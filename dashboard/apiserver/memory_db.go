// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
)

// MemoryInsight is the wire shape for evo.insights rows. Mirrors the
// EvoInsight struct in evo_endpoints.go but lives here so memory
// pagination filters can evolve independently of the evo runs listing.
type MemoryInsight struct {
	ID              int64     `json:"id"`
	Domain          string    `json:"domain"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	Tags            []string  `json:"tags"`
	HasEmbedding    bool      `json:"has_embedding"`
	SourceProgramID *int64    `json:"source_program_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// MemoryEntity / MemoryRelationship mirror the deepresearch.documents_*
// tables. We keep the field names lower_snake to match the column
// layout; the UI re-aliases when needed.
type MemoryEntity struct {
	ID          string    `json:"id"`
	ParentID    string    `json:"parent_id"`
	Name        string    `json:"name"`
	Category    string    `json:"category"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type MemoryRelationship struct {
	ID          string    `json:"id"`
	ParentID    string    `json:"parent_id"`
	Subject     string    `json:"subject"`
	Predicate   string    `json:"predicate"`
	Object      string    `json:"object"`
	Description string    `json:"description"`
	Weight      float64   `json:"weight"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// registerMemoryDBEndpoints wires the Postgres half of the Memory API.
// Filesystem-backed routes are in memory_files.go; together they form
// the full /api/v1/memory/* surface.
func (s *APIServer) registerMemoryDBEndpoints(api *mux.Router) {
	api.HandleFunc("/memory/insights", s.handleMemoryInsightsList).Methods("GET")
	api.HandleFunc("/memory/insights", s.handleMemoryInsightsCreate).Methods("POST")
	api.HandleFunc("/memory/insights/{id}", s.handleMemoryInsightsUpdate).Methods("PATCH")
	api.HandleFunc("/memory/insights/{id}", s.handleMemoryInsightsDelete).Methods("DELETE")

	api.HandleFunc("/memory/entities", s.handleMemoryEntitiesList).Methods("GET")
	api.HandleFunc("/memory/entities/{id}", s.handleMemoryEntitiesUpdate).Methods("PATCH")
	api.HandleFunc("/memory/entities/{id}", s.handleMemoryEntitiesDelete).Methods("DELETE")

	api.HandleFunc("/memory/relationships", s.handleMemoryRelationshipsList).Methods("GET")
	api.HandleFunc("/memory/relationships/{id}", s.handleMemoryRelationshipsUpdate).Methods("PATCH")
	api.HandleFunc("/memory/relationships/{id}", s.handleMemoryRelationshipsDelete).Methods("DELETE")
}

// ---- evo.insights ----

func (s *APIServer) handleMemoryInsightsList(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit, offset := parseLimitOffset(r, 100, 500, 100000)
	q := strings.Builder{}
	q.WriteString(`
		SELECT id, domain, title, body, tags,
		       embedding IS NOT NULL AS has_embedding,
		       source_program_id, created_at
		FROM evo.insights
		WHERE 1=1
	`)
	args := []interface{}{}
	if v := strings.TrimSpace(r.URL.Query().Get("domain")); v != "" {
		args = append(args, v)
		q.WriteString(fmt.Sprintf(" AND domain = $%d", len(args)))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("tag")); v != "" {
		args = append(args, v)
		q.WriteString(fmt.Sprintf(" AND $%d = ANY(tags)", len(args)))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("q")); v != "" {
		args = append(args, "%"+v+"%")
		q.WriteString(fmt.Sprintf(" AND (title ILIKE $%d OR body ILIKE $%d)", len(args), len(args)))
	}
	args = append(args, limit, offset)
	q.WriteString(fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)))

	rows, err := s.evoPool.Query(ctx, q.String(), args...)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]MemoryInsight, 0, limit)
	for rows.Next() {
		var ins MemoryInsight
		if err := rows.Scan(&ins.ID, &ins.Domain, &ins.Title, &ins.Body, &ins.Tags,
			&ins.HasEmbedding, &ins.SourceProgramID, &ins.CreatedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		if ins.Tags == nil {
			ins.Tags = []string{}
		}
		out = append(out, ins)
	}
	writeEvoJSON(w, out)
}

type memoryInsightInput struct {
	Domain string   `json:"domain"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Tags   []string `json:"tags"`
}

func (s *APIServer) handleMemoryInsightsCreate(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in memoryInsightInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.Domain = strings.TrimSpace(in.Domain)
	in.Title = strings.TrimSpace(in.Title)
	if in.Domain == "" || in.Title == "" || strings.TrimSpace(in.Body) == "" {
		writeEvoErr(w, http.StatusBadRequest, "domain, title, body are required")
		return
	}
	if in.Tags == nil {
		in.Tags = []string{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, `
		INSERT INTO evo.insights (domain, title, body, tags)
		VALUES ($1, $2, $3, $4)
		RETURNING id, domain, title, body, tags,
		          embedding IS NOT NULL AS has_embedding,
		          source_program_id, created_at
	`, in.Domain, in.Title, in.Body, in.Tags)
	var ins MemoryInsight
	if err := row.Scan(&ins.ID, &ins.Domain, &ins.Title, &ins.Body, &ins.Tags,
		&ins.HasEmbedding, &ins.SourceProgramID, &ins.CreatedAt); err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	if ins.Tags == nil {
		ins.Tags = []string{}
	}
	w.WriteHeader(http.StatusCreated)
	writeEvoJSON(w, ins)
}

func (s *APIServer) handleMemoryInsightsUpdate(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in memoryInsightInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	// COALESCE the new value against the column so callers can omit any
	// field they don't want to change. NULL-typed text/text[] needs an
	// explicit cast or Postgres throws a "could not determine type" error
	// the first time a row uses a column with no DEFAULT.
	row := s.evoPool.QueryRow(ctx, `
		UPDATE evo.insights SET
		  domain = COALESCE(NULLIF($1::text, ''), domain),
		  title  = COALESCE(NULLIF($2::text, ''), title),
		  body   = COALESCE(NULLIF($3::text, ''), body),
		  tags   = COALESCE($4::text[], tags)
		WHERE id = $5
		RETURNING id, domain, title, body, tags,
		          embedding IS NOT NULL AS has_embedding,
		          source_program_id, created_at
	`, in.Domain, in.Title, in.Body, in.Tags, id)
	var ins MemoryInsight
	if err := row.Scan(&ins.ID, &ins.Domain, &ins.Title, &ins.Body, &ins.Tags,
		&ins.HasEmbedding, &ins.SourceProgramID, &ins.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "insight not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	if ins.Tags == nil {
		ins.Tags = []string{}
	}
	writeEvoJSON(w, ins)
}

func (s *APIServer) handleMemoryInsightsDelete(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tag, err := s.evoPool.Exec(ctx, `DELETE FROM evo.insights WHERE id = $1`, id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeEvoErr(w, http.StatusNotFound, "insight not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- deepresearch entities / relationships ----

// dpSchemaRE is the same allowlist r2g uses. Re-derived locally so
// memory_db.go doesn't need to depend on r2g.
var dpSchemaRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// deepresearchSchema resolves R2R_PROJECT_NAME (default "deepresearch")
// with the same regex r2g uses, so a hostile env var can't smuggle in
// SQL via the schema name. Caller surfaces the error.
func deepresearchSchema() (string, error) {
	s := os.Getenv("R2R_PROJECT_NAME")
	if s == "" {
		s = "deepresearch"
	}
	if !dpSchemaRE.MatchString(s) {
		return "", fmt.Errorf("unsafe R2R_PROJECT_NAME: %q", s)
	}
	return s, nil
}

func (s *APIServer) handleMemoryEntitiesList(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	schema, err := deepresearchSchema()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit, offset := parseLimitOffset(r, 100, 500, 100000)
	q := strings.Builder{}
	q.WriteString(fmt.Sprintf(`
		SELECT id, parent_id, name,
		       COALESCE(category, '') AS category,
		       COALESCE(description, '') AS description,
		       created_at, updated_at
		FROM %s.documents_entities
		WHERE 1=1
	`, schema))
	args := []interface{}{}
	if v := strings.TrimSpace(r.URL.Query().Get("category")); v != "" {
		args = append(args, v)
		q.WriteString(fmt.Sprintf(" AND category = $%d", len(args)))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("q")); v != "" {
		args = append(args, "%"+v+"%")
		q.WriteString(fmt.Sprintf(" AND (name ILIKE $%d OR description ILIKE $%d)", len(args), len(args)))
	}
	args = append(args, limit, offset)
	q.WriteString(fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)))

	rows, err := s.evoPool.Query(ctx, q.String(), args...)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]MemoryEntity, 0, limit)
	for rows.Next() {
		var (
			e        MemoryEntity
			eid, pid uuid.UUID
		)
		if err := rows.Scan(&eid, &pid, &e.Name, &e.Category, &e.Description, &e.CreatedAt, &e.UpdatedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		e.ID = eid.String()
		e.ParentID = pid.String()
		out = append(out, e)
	}
	writeEvoJSON(w, out)
}

type memoryEntityInput struct {
	Description string `json:"description"`
	Category    string `json:"category"`
}

func (s *APIServer) handleMemoryEntitiesUpdate(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	schema, err := deepresearchSchema()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := mux.Vars(r)["id"]
	if _, err := uuid.Parse(id); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<19)
	var in memoryEntityInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.documents_entities SET
		  description = COALESCE(NULLIF($1::text, ''), description),
		  category    = COALESCE(NULLIF($2::text, ''), category),
		  updated_at  = NOW()
		WHERE id = $3::uuid
		RETURNING id, parent_id, name,
		          COALESCE(category, '') AS category,
		          COALESCE(description, '') AS description,
		          created_at, updated_at
	`, schema), in.Description, in.Category, id)
	var (
		e        MemoryEntity
		eid, pid uuid.UUID
	)
	if err := row.Scan(&eid, &pid, &e.Name, &e.Category, &e.Description, &e.CreatedAt, &e.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "entity not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	e.ID = eid.String()
	e.ParentID = pid.String()
	writeEvoJSON(w, e)
}

func (s *APIServer) handleMemoryEntitiesDelete(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	schema, err := deepresearchSchema()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := mux.Vars(r)["id"]
	if _, err := uuid.Parse(id); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tag, err := s.evoPool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s.documents_entities WHERE id = $1::uuid`, schema), id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeEvoErr(w, http.StatusNotFound, "entity not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleMemoryRelationshipsList(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	schema, err := deepresearchSchema()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit, offset := parseLimitOffset(r, 100, 500, 100000)
	q := strings.Builder{}
	q.WriteString(fmt.Sprintf(`
		SELECT id, parent_id, subject,
		       COALESCE(predicate, '') AS predicate,
		       object,
		       COALESCE(description, '') AS description,
		       COALESCE(weight, 0) AS weight,
		       created_at, updated_at
		FROM %s.documents_relationships
		WHERE 1=1
	`, schema))
	args := []interface{}{}
	if v := strings.TrimSpace(r.URL.Query().Get("predicate")); v != "" {
		args = append(args, v)
		q.WriteString(fmt.Sprintf(" AND predicate = $%d", len(args)))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("q")); v != "" {
		args = append(args, "%"+v+"%")
		q.WriteString(fmt.Sprintf(" AND (subject ILIKE $%d OR object ILIKE $%d OR description ILIKE $%d)", len(args), len(args), len(args)))
	}
	args = append(args, limit, offset)
	q.WriteString(fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)))

	rows, err := s.evoPool.Query(ctx, q.String(), args...)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()
	out := make([]MemoryRelationship, 0, limit)
	for rows.Next() {
		var (
			rel      MemoryRelationship
			rid, pid uuid.UUID
		)
		if err := rows.Scan(&rid, &pid, &rel.Subject, &rel.Predicate, &rel.Object,
			&rel.Description, &rel.Weight, &rel.CreatedAt, &rel.UpdatedAt); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		rel.ID = rid.String()
		rel.ParentID = pid.String()
		out = append(out, rel)
	}
	writeEvoJSON(w, out)
}

type memoryRelationshipInput struct {
	Predicate   string  `json:"predicate"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight"`
}

func (s *APIServer) handleMemoryRelationshipsUpdate(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	schema, err := deepresearchSchema()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := mux.Vars(r)["id"]
	if _, err := uuid.Parse(id); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<19)
	var in memoryRelationshipInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE %s.documents_relationships SET
		  predicate   = COALESCE(NULLIF($1::text, ''), predicate),
		  description = COALESCE(NULLIF($2::text, ''), description),
		  weight      = CASE WHEN $3::float8 = 0 THEN weight ELSE $3 END,
		  updated_at  = NOW()
		WHERE id = $4::uuid
		RETURNING id, parent_id, subject,
		          COALESCE(predicate, '') AS predicate,
		          object,
		          COALESCE(description, '') AS description,
		          COALESCE(weight, 0) AS weight,
		          created_at, updated_at
	`, schema), in.Predicate, in.Description, in.Weight, id)
	var (
		rel      MemoryRelationship
		rid, pid uuid.UUID
	)
	if err := row.Scan(&rid, &pid, &rel.Subject, &rel.Predicate, &rel.Object,
		&rel.Description, &rel.Weight, &rel.CreatedAt, &rel.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "relationship not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	rel.ID = rid.String()
	rel.ParentID = pid.String()
	writeEvoJSON(w, rel)
}

func (s *APIServer) handleMemoryRelationshipsDelete(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	schema, err := deepresearchSchema()
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := mux.Vars(r)["id"]
	if _, err := uuid.Parse(id); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tag, err := s.evoPool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s.documents_relationships WHERE id = $1::uuid`, schema), id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeEvoErr(w, http.StatusNotFound, "relationship not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- count helpers used by /memory/sources ----

func (s *APIServer) countInsights(r *http.Request) (int, time.Time) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var (
		n  int
		ts *time.Time
	)
	if err := s.evoPool.QueryRow(ctx,
		`SELECT COUNT(*), MAX(created_at) FROM evo.insights`).Scan(&n, &ts); err != nil {
		return -1, time.Time{}
	}
	if ts == nil {
		return n, time.Time{}
	}
	return n, *ts
}

func (s *APIServer) countDeepresearchTable(r *http.Request, table string) (int, time.Time) {
	schema, err := deepresearchSchema()
	if err != nil {
		return -1, time.Time{}
	}
	if !dpSchemaRE.MatchString(table) {
		return -1, time.Time{}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var (
		n  int
		ts *time.Time
	)
	if err := s.evoPool.QueryRow(ctx,
		fmt.Sprintf(`SELECT COUNT(*), MAX(updated_at) FROM %s.%s`, schema, table)).Scan(&n, &ts); err != nil {
		// Schema may not exist in dev — surface -1 so the UI flags it
		// as unavailable rather than reporting zero rows.
		return -1, time.Time{}
	}
	if ts == nil {
		return n, time.Time{}
	}
	return n, *ts
}
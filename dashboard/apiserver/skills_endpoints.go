// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Skill is the wire shape returned by /api/v1/skills/* endpoints.
// r2r_* fields are populated once Phase 2 (R2R mirror) is wired in;
// callers tolerate them being absent.
type Skill struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	Body            string     `json:"body"`
	WhenToUse       *string    `json:"when_to_use,omitempty"`
	Example         *string    `json:"example,omitempty"`
	Community       *string    `json:"community,omitempty"`
	Tags            []string   `json:"tags"`
	DependsOn       []string   `json:"depends_on"`
	Origin          string     `json:"origin"`
	SourceURL       *string    `json:"source_url,omitempty"`
	SourceSHA       *string    `json:"source_sha,omitempty"`
	SourceCheckedAt *time.Time `json:"source_checked_at,omitempty"`
	R2RDocumentID   *string    `json:"r2r_document_id,omitempty"`
	R2RCorpusID     *string    `json:"r2r_corpus_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	UsageCount      int        `json:"usage_count,omitempty"`
}

// SkillInput is the shape accepted by POST + PUT. id is server-assigned
// on POST; on PUT the path captures it instead.
type SkillInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	WhenToUse   *string  `json:"when_to_use,omitempty"`
	Example     *string  `json:"example,omitempty"`
	Community   *string  `json:"community,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Origin      string   `json:"origin,omitempty"` // defaults to "manual"
	SourceURL   *string  `json:"source_url,omitempty"`
}

// registerSkillsEndpoints wires the full /api/v1/skills/* surface.
// CRUD + import (Phase 1–3) + search/graph (Phase 4) + usages (Phase 5).
func (s *APIServer) registerSkillsEndpoints(api *mux.Router) {
	api.HandleFunc("/skills", s.withListRateLimit(s.listSkills)).Methods("GET")
	api.HandleFunc("/skills", s.createSkill).Methods("POST")
	api.HandleFunc("/skills/import", s.importSkillsHandler).Methods("POST")
	api.HandleFunc("/skills/search", s.searchSkills).Methods("POST")
	api.HandleFunc("/skills/graph", s.skillsGraph).Methods("GET")
	api.HandleFunc("/skills/usages", s.recordSkillUsage).Methods("POST")
	api.HandleFunc("/skills/{id}", s.getSkill).Methods("GET")
	api.HandleFunc("/skills/{id}", s.updateSkill).Methods("PUT")
	api.HandleFunc("/skills/{id}", s.deleteSkill).Methods("DELETE")
	api.HandleFunc("/skills/{id}/check-source", s.checkSkillSource).Methods("POST")
}

func (s *APIServer) listSkills(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	qb := strings.Builder{}
	qb.WriteString(`
		SELECT s.id, s.name, s.description, s.body, s.when_to_use, s.example, s.community,
		       s.tags, s.depends_on, s.origin, s.source_url, s.source_sha, s.source_checked_at,
		       s.r2r_document_id, s.r2r_corpus_id, s.created_at, s.updated_at,
		       COALESCE(u.cnt, 0) AS usage_count
		FROM evo.skills s
		LEFT JOIN (
		  SELECT skill_id, COUNT(*) AS cnt
		  FROM evo.skill_usages
		  GROUP BY skill_id
		) u ON u.skill_id = s.id
		WHERE 1=1
	`)
	args := []interface{}{}
	placeholder := func(v interface{}) string {
		args = append(args, v)
		return "$" + itoa(len(args))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("community")); v != "" {
		qb.WriteString(" AND community = " + placeholder(v))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("origin")); v != "" {
		qb.WriteString(" AND origin = " + placeholder(v))
	}
	if v := strings.TrimSpace(r.URL.Query().Get("tag")); v != "" {
		qb.WriteString(" AND " + placeholder(v) + " = ANY(tags)")
	}
	if v := strings.TrimSpace(r.URL.Query().Get("q")); v != "" {
		like := "%" + v + "%"
		qb.WriteString(" AND (s.name ILIKE " + placeholder(like) +
			" OR s.description ILIKE " + placeholder(like) + ")")
	}
	// Bound the result set with the shared parseLimitOffset clamp so a
	// ?limit=/?offset= here behaves identically to the other list
	// endpoints (jobs, workers, memory, evo runs) instead of being a
	// hard-coded 500-row wall. Default of 500 preserves prior behaviour
	// for callers that don't pass the params.
	limit, offset := parseLimitOffset(r, 500, 500, 100000)
	qb.WriteString(" ORDER BY s.community NULLS LAST, s.name LIMIT " +
		placeholder(limit) + " OFFSET " + placeholder(offset))

	rows, err := s.evoPool.Query(ctx, qb.String(), args...)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := make([]Skill, 0)
	for rows.Next() {
		sk, err := scanSkillWithUsage(rows)
		if err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, sk)
	}
	writeEvoJSON(w, out)
}

// scanSkillWithUsage is the list-path scanner — the LEFT JOIN appends
// one COUNT(*) column over evo.skill_usages so the UI can show
// "used N times" without a second round-trip.
func scanSkillWithUsage(rs rowScanner) (Skill, error) {
	var sk Skill
	if err := rs.Scan(
		&sk.ID, &sk.Name, &sk.Description, &sk.Body,
		&sk.WhenToUse, &sk.Example, &sk.Community,
		&sk.Tags, &sk.DependsOn,
		&sk.Origin, &sk.SourceURL, &sk.SourceSHA, &sk.SourceCheckedAt,
		&sk.R2RDocumentID, &sk.R2RCorpusID,
		&sk.CreatedAt, &sk.UpdatedAt,
		&sk.UsageCount,
	); err != nil {
		return Skill{}, err
	}
	if sk.Tags == nil {
		sk.Tags = []string{}
	}
	if sk.DependsOn == nil {
		sk.DependsOn = []string{}
	}
	return sk, nil
}

func (s *APIServer) getSkill(w http.ResponseWriter, r *http.Request) {
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

	row := s.evoPool.QueryRow(ctx, `
		SELECT id, name, description, body, when_to_use, example, community,
		       tags, depends_on, origin, source_url, source_sha, source_checked_at,
		       r2r_document_id, r2r_corpus_id, created_at, updated_at
		FROM evo.skills WHERE id = $1
	`, id)
	sk, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "skill not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
		return
	}
	writeEvoJSON(w, sk)
}

func (s *APIServer) createSkill(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in SkillInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := validateSkillInput(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if in.Origin == "" {
		in.Origin = "manual"
	}
	sha := bodySHA(in.Body)
	if in.Tags == nil {
		in.Tags = []string{}
	}
	if in.DependsOn == nil {
		in.DependsOn = []string{}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, `
		INSERT INTO evo.skills
		  (name, description, body, when_to_use, example, community,
		   tags, depends_on, origin, source_url, source_sha)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, name, description, body, when_to_use, example, community,
		          tags, depends_on, origin, source_url, source_sha, source_checked_at,
		          r2r_document_id, r2r_corpus_id, created_at, updated_at
	`, in.Name, in.Description, in.Body, in.WhenToUse, in.Example, in.Community,
		in.Tags, in.DependsOn, in.Origin, in.SourceURL, sha)

	sk, err := scanSkill(row)
	if err != nil {
		if isUniqueViolation(err) {
			writeEvoErr(w, http.StatusConflict, "skill exists: "+err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	s.recordR2RDocID(ctx, sk.ID)
	s.syncSkillToR2R(sk)
	w.WriteHeader(http.StatusCreated)
	writeEvoJSON(w, sk)
}

func (s *APIServer) updateSkill(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in SkillInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := validateSkillInput(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sha := bodySHA(in.Body)
	if in.Tags == nil {
		in.Tags = []string{}
	}
	if in.DependsOn == nil {
		in.DependsOn = []string{}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row := s.evoPool.QueryRow(ctx, `
		UPDATE evo.skills SET
		  name = $1, description = $2, body = $3, when_to_use = $4,
		  example = $5, community = $6, tags = $7, depends_on = $8,
		  source_url = COALESCE($9, source_url),
		  source_sha = $10,
		  updated_at = NOW()
		WHERE id = $11
		RETURNING id, name, description, body, when_to_use, example, community,
		          tags, depends_on, origin, source_url, source_sha, source_checked_at,
		          r2r_document_id, r2r_corpus_id, created_at, updated_at
	`, in.Name, in.Description, in.Body, in.WhenToUse, in.Example, in.Community,
		in.Tags, in.DependsOn, in.SourceURL, sha, id)

	sk, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "skill not found")
			return
		}
		if isUniqueViolation(err) {
			writeEvoErr(w, http.StatusConflict, "skill name or content collides: "+err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "update: "+err.Error())
		return
	}
	s.recordR2RDocID(ctx, sk.ID)
	s.syncSkillToR2R(sk)
	writeEvoJSON(w, sk)
}

func (s *APIServer) deleteSkill(w http.ResponseWriter, r *http.Request) {
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
	tag, err := s.evoPool.Exec(ctx, `DELETE FROM evo.skills WHERE id = $1`, id)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeEvoErr(w, http.StatusNotFound, "skill not found")
		return
	}
	s.deleteSkillFromR2R(id)
	w.WriteHeader(http.StatusNoContent)
}

// skillUsageInput is the body of POST /api/v1/skills/usages. The
// delegator integration POSTs one of these per skill it surfaced into
// a task's prompt. Multiple skills per task → multiple POSTs (cheap;
// the orchestrator already runs in a goroutine).
type skillUsageInput struct {
	SkillID    string `json:"skill_id"`
	TaskID     string `json:"task_id"`
	WasHelpful *bool  `json:"was_helpful,omitempty"`
}

func (s *APIServer) recordSkillUsage(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var in skillUsageInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.SkillID = strings.TrimSpace(in.SkillID)
	in.TaskID = strings.TrimSpace(in.TaskID)
	if in.SkillID == "" || in.TaskID == "" {
		writeEvoErr(w, http.StatusBadRequest, "skill_id and task_id are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.evoPool.Exec(ctx, `
		INSERT INTO evo.skill_usages (skill_id, task_id, was_helpful)
		VALUES ($1, $2, $3)
		ON CONFLICT (skill_id, task_id) DO UPDATE
		  SET was_helpful = COALESCE(EXCLUDED.was_helpful, evo.skill_usages.was_helpful)
	`, in.SkillID, in.TaskID, in.WasHelpful)
	if err != nil {
		// Foreign-key violations mean the caller referenced a deleted
		// skill — return 409 so the delegator can keep moving.
		if isUniqueViolation(err) {
			writeEvoErr(w, http.StatusConflict, err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "insert: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordR2RDocID stamps the evo.skills row with the R2R document_id
// (which is just the skill id itself — the platform mirror reuses it).
// Called after every insert/update so a future "show me this skill in
// R2R" link has the id even before the async POST returns.
func (s *APIServer) recordR2RDocID(ctx context.Context, skillID string) {
	corpus := skillsR2RCorpusID()
	if corpus == "" {
		_, _ = s.evoPool.Exec(ctx, `
			UPDATE evo.skills SET r2r_document_id = $1
			WHERE id = $1 AND (r2r_document_id IS NULL OR r2r_document_id <> $1)
		`, skillID)
		return
	}
	_, _ = s.evoPool.Exec(ctx, `
		UPDATE evo.skills SET r2r_document_id = $1, r2r_corpus_id = $2
		WHERE id = $1
	`, skillID, corpus)
}

// scanSkill consumes a pgx.Row (or pgx.Rows) and returns a Skill. Used
// by getSkill / listSkills / createSkill / updateSkill so the column
// list stays in one place.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanSkill(rs rowScanner) (Skill, error) {
	var sk Skill
	if err := rs.Scan(
		&sk.ID, &sk.Name, &sk.Description, &sk.Body,
		&sk.WhenToUse, &sk.Example, &sk.Community,
		&sk.Tags, &sk.DependsOn,
		&sk.Origin, &sk.SourceURL, &sk.SourceSHA, &sk.SourceCheckedAt,
		&sk.R2RDocumentID, &sk.R2RCorpusID,
		&sk.CreatedAt, &sk.UpdatedAt,
	); err != nil {
		return Skill{}, err
	}
	if sk.Tags == nil {
		sk.Tags = []string{}
	}
	if sk.DependsOn == nil {
		sk.DependsOn = []string{}
	}
	return sk, nil
}

func validateSkillInput(in *SkillInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" {
		return errors.New("name is required")
	}
	if in.Description == "" {
		return errors.New("description is required")
	}
	if strings.TrimSpace(in.Body) == "" {
		return errors.New("body is required")
	}
	return nil
}

func bodySHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// itoa is a tiny non-allocating int-to-string helper used by listSkills
// to build the $N placeholders. Inlined to avoid pulling in strconv for
// one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

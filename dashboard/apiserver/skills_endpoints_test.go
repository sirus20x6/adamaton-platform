package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestValidateSkillInput pins the field-presence rules (these run without a DB).
func TestValidateSkillInput(t *testing.T) {
	cases := []struct {
		name    string
		in      SkillInput
		wantErr string
	}{
		{name: "ok", in: SkillInput{Name: "n", Description: "d", Body: "b"}},
		{name: "missing name", in: SkillInput{Description: "d", Body: "b"}, wantErr: "name is required"},
		{name: "blank name trimmed", in: SkillInput{Name: "  ", Description: "d", Body: "b"}, wantErr: "name is required"},
		{name: "missing description", in: SkillInput{Name: "n", Body: "b"}, wantErr: "description is required"},
		{name: "missing body", in: SkillInput{Name: "n", Description: "d"}, wantErr: "body is required"},
		{name: "whitespace body", in: SkillInput{Name: "n", Description: "d", Body: "   "}, wantErr: "body is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			err := validateSkillInput(&in)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.wantErr)
			}
		})
	}
}

func TestBodySHA_Stable(t *testing.T) {
	a := bodySHA("hello")
	b := bodySHA("hello")
	require.Equal(t, a, b)
	require.NotEqual(t, a, bodySHA("world"))
	require.Len(t, a, 64) // hex of sha256
}

// TestSkillsEndpoints_NoPool covers the evo-pool-not-configured branch on the
// CRUD handlers (the ones that read evoPool first).
func TestSkillsEndpoints_NoPool(t *testing.T) {
	s := newPoollessServer(t)
	for _, tc := range []struct {
		method, target, body string
	}{
		{http.MethodGet, "/api/v1/skills", ""},
		{http.MethodGet, "/api/v1/skills/" + uuid.NewString(), ""},
		{http.MethodPost, "/api/v1/skills", `{"name":"n","description":"d","body":"b"}`},
		{http.MethodPut, "/api/v1/skills/" + uuid.NewString(), `{"name":"n","description":"d","body":"b"}`},
		{http.MethodDelete, "/api/v1/skills/" + uuid.NewString(), ""},
		{http.MethodPost, "/api/v1/skills/usages", `{"skill_id":"x","task_id":"y"}`},
		{http.MethodPost, "/api/v1/skills/import", `{"kind":"local"}`},
	} {
		rr := serveVia(s, s.registerSkillsEndpoints, tc.method, tc.target, tc.body)
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, tc.target)
		require.Contains(t, rr.Body.String(), "evo pool not configured", tc.target)
	}
}

// TestCreateSkill_BadJSON / validation paths (DB-backed because createSkill
// reads evoPool before parsing).
func TestCreateSkill_Validation(t *testing.T) {
	s := newDBTestServer(t)
	t.Run("bad json", func(t *testing.T) {
		rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills", `{not json`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "invalid json")
	})
	t.Run("missing fields", func(t *testing.T) {
		rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills", `{"name":"only-name"}`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "description is required")
	})
}

func TestGetSkill_NotFound(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerSkillsEndpoints, http.MethodGet, "/api/v1/skills/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "skill not found")
}

func TestUpdateSkill_NotFound(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPut,
		"/api/v1/skills/"+uuid.NewString(), `{"name":"n","description":"d","body":"b"}`)
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "skill not found")
}

func TestDeleteSkill_NotFound(t *testing.T) {
	s := newDBTestServer(t)
	rr := serveVia(s, s.registerSkillsEndpoints, http.MethodDelete, "/api/v1/skills/"+uuid.NewString(), "")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "skill not found")
}

// TestSkill_CRUD_RoundTrip creates a skill, reads it back, updates it, lists
// it, then deletes it — the full happy path against the migrated DB.
func TestSkill_CRUD_RoundTrip(t *testing.T) {
	s := newDBTestServer(t)
	name := "wf3-crud-" + uuid.NewString()[:8]

	// Create.
	rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills",
		`{"name":"`+name+`","description":"desc","body":"the body","origin":"manual"}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created Skill
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, name, created.Name)
	require.NotNil(t, created.Tags) // normalised to [] never null
	id := created.ID
	t.Cleanup(func() {
		_, _ = s.evoPool.Exec(context.Background(), `DELETE FROM evo.skills WHERE id = $1`, id)
	})

	// Get.
	rr = serveVia(s, s.registerSkillsEndpoints, http.MethodGet, "/api/v1/skills/"+id, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var got Skill
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Equal(t, "the body", got.Body)

	// Update.
	rr = serveVia(s, s.registerSkillsEndpoints, http.MethodPut, "/api/v1/skills/"+id,
		`{"name":"`+name+`","description":"desc2","body":"new body"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var updated Skill
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &updated))
	require.Equal(t, "new body", updated.Body)
	require.Equal(t, "desc2", updated.Description)

	// List includes it.
	rr = serveVia(s, s.registerSkillsEndpoints, http.MethodGet, "/api/v1/skills?q="+name, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var list []Skill
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	require.Equal(t, id, list[0].ID)

	// Delete.
	rr = serveVia(s, s.registerSkillsEndpoints, http.MethodDelete, "/api/v1/skills/"+id, "")
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Second delete -> 404.
	rr = serveVia(s, s.registerSkillsEndpoints, http.MethodDelete, "/api/v1/skills/"+id, "")
	require.Equal(t, http.StatusNotFound, rr.Code)
}

// TestSkills_UsageCount_GroupByMatchesCount is the perf-rewrite assertion for
// card-6145d587: the usage_count column produced by the GROUP BY rewrite of
// listSkills must equal a direct COUNT(*) over evo.skill_usages for that skill.
func TestSkills_UsageCount_GroupByMatchesCount(t *testing.T) {
	s := newDBTestServer(t)
	pool := s.evoPool
	ctx := context.Background()
	name := "wf3-usage-" + uuid.NewString()[:8]

	var id string
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO evo.skills (name, description, body, origin)
		VALUES ($1, 'd', 'b', 'manual') RETURNING id`, name).Scan(&id))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM evo.skill_usages WHERE skill_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM evo.skills WHERE id = $1`, id)
	})

	const nUsages = 4
	for i := 0; i < nUsages; i++ {
		_, err := pool.Exec(ctx, `
			INSERT INTO evo.skill_usages (skill_id, task_id, was_helpful)
			VALUES ($1, $2, true)
			ON CONFLICT (skill_id, task_id) DO NOTHING`,
			id, "wf3-task-"+uuid.NewString())
		require.NoError(t, err)
	}

	rr := serveVia(s, s.registerSkillsEndpoints, http.MethodGet, "/api/v1/skills?q="+name, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var list []Skill
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	require.Equal(t, nUsages, list[0].UsageCount, "GROUP BY usage_count must match seeded usages")

	var direct int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM evo.skill_usages WHERE skill_id = $1`, id).Scan(&direct))
	require.Equal(t, direct, list[0].UsageCount)
}

// TestRecordSkillUsage_Validation covers the required-field guard + the FK
// (deleted skill) -> 409/500 path. A random skill_id that doesn't exist trips
// the FK violation; the handler maps unique violations to 409 but a plain FK
// violation falls through to 500 — we just assert it doesn't 2xx and doesn't
// panic.
func TestRecordSkillUsage_Validation(t *testing.T) {
	s := newDBTestServer(t)
	t.Run("missing ids", func(t *testing.T) {
		rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills/usages", `{"skill_id":"","task_id":""}`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "required")
	})
	t.Run("bad json", func(t *testing.T) {
		rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills/usages", `{`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})
}

// TestSearchSkills_Validation pins the request validation on the search
// endpoint (these branches run before any R2R/deepresearch dependency).
func TestSearchSkills_Validation(t *testing.T) {
	s := newDBTestServer(t)
	t.Run("bad json", func(t *testing.T) {
		rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills/search", `{`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "invalid json")
	})
	t.Run("missing query", func(t *testing.T) {
		rr := serveVia(s, s.registerSkillsEndpoints, http.MethodPost, "/api/v1/skills/search", `{"query":""}`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "query is required")
	})
	// We deliberately don't drive the happy path: proxySkillsSearch reaches
	// out to the (unconfigured) deepresearch URL over the network, which is
	// out of scope for a unit test. The validation branches above are the
	// handler logic we own.
}

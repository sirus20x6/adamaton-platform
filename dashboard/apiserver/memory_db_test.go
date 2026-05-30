package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestDeepresearchSchema_InjectionGuard pins the allowlist that keeps a hostile
// R2R_PROJECT_NAME from smuggling SQL into the fmt.Sprintf'd schema name.
func TestDeepresearchSchema_InjectionGuard(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "default when unset", env: "", want: "deepresearch"},
		{name: "plain identifier", env: "myproj", want: "myproj"},
		{name: "underscore + digits", env: "proj_2", want: "proj_2"},
		{name: "leading underscore", env: "_x", want: "_x"},
		{name: "semicolon injection", env: "x; DROP TABLE skills;--", wantErr: true},
		{name: "space", env: "a b", wantErr: true},
		{name: "quote", env: `x"`, wantErr: true},
		{name: "leading digit", env: "1abc", wantErr: true},
		{name: "dash", env: "a-b", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("R2R_PROJECT_NAME", tc.env)
			got, err := deepresearchSchema()
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "unsafe R2R_PROJECT_NAME")
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}

// TestMemoryEndpoints_NoPool covers the evo-pool-not-configured branch.
func TestMemoryEndpoints_NoPool(t *testing.T) {
	s := newPoollessServer(t)
	cases := []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/memory/insights", ""},
		{http.MethodPost, "/api/v1/memory/insights", `{"domain":"d","title":"t","body":"b"}`},
		{http.MethodPatch, "/api/v1/memory/insights/1", `{}`},
		{http.MethodDelete, "/api/v1/memory/insights/1", ""},
		{http.MethodGet, "/api/v1/memory/entities", ""},
		{http.MethodPatch, "/api/v1/memory/entities/" + uuid.NewString(), `{}`},
		{http.MethodDelete, "/api/v1/memory/entities/" + uuid.NewString(), ""},
		{http.MethodGet, "/api/v1/memory/relationships", ""},
		{http.MethodPatch, "/api/v1/memory/relationships/" + uuid.NewString(), `{}`},
		{http.MethodDelete, "/api/v1/memory/relationships/" + uuid.NewString(), ""},
	}
	for _, tc := range cases {
		rr := serveVia(s, s.registerMemoryDBEndpoints, tc.method, tc.target, tc.body)
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, tc.target)
		require.Contains(t, rr.Body.String(), "evo pool not configured", tc.target)
	}
}

// TestMemoryInsights_CRUD round-trips an insight through create/list/update/
// delete against evo.insights (which exists in the migrated test DB).
func TestMemoryInsights_CRUD(t *testing.T) {
	s := newDBTestServer(t)
	domain := "wf3-mem-" + uuid.NewString()[:8]

	// Create.
	rr := serveVia(s, s.registerMemoryDBEndpoints, http.MethodPost, "/api/v1/memory/insights",
		`{"domain":"`+domain+`","title":"t1","body":"b1","tags":["x","y"]}`)
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var created MemoryInsight
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &created))
	require.NotZero(t, created.ID)
	require.Equal(t, []string{"x", "y"}, created.Tags)
	id := created.ID
	t.Cleanup(func() {
		_, _ = s.evoPool.Exec(context.Background(), `DELETE FROM evo.insights WHERE id = $1`, id)
	})

	// List with domain filter.
	rr = serveVia(s, s.registerMemoryDBEndpoints, http.MethodGet, "/api/v1/memory/insights?domain="+domain, "")
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var list []MemoryInsight
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list))
	require.Len(t, list, 1)
	require.Equal(t, id, list[0].ID)

	// Update (partial — only body, others retained via COALESCE/NULLIF).
	rr = serveVia(s, s.registerMemoryDBEndpoints, http.MethodPatch, idPath("/api/v1/memory/insights/", id),
		`{"body":"b2"}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var updated MemoryInsight
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &updated))
	require.Equal(t, "b2", updated.Body)
	require.Equal(t, "t1", updated.Title, "title must be retained when omitted")

	// Delete.
	rr = serveVia(s, s.registerMemoryDBEndpoints, http.MethodDelete, idPath("/api/v1/memory/insights/", id), "")
	require.Equal(t, http.StatusNoContent, rr.Code)

	// Second delete -> 404.
	rr = serveVia(s, s.registerMemoryDBEndpoints, http.MethodDelete, idPath("/api/v1/memory/insights/", id), "")
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func idPath(prefix string, id int64) string {
	return prefix + itoa64(id)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
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

func TestMemoryInsights_CreateValidation(t *testing.T) {
	s := newDBTestServer(t)
	t.Run("bad json", func(t *testing.T) {
		rr := serveVia(s, s.registerMemoryDBEndpoints, http.MethodPost, "/api/v1/memory/insights", `{`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})
	t.Run("missing required", func(t *testing.T) {
		rr := serveVia(s, s.registerMemoryDBEndpoints, http.MethodPost, "/api/v1/memory/insights", `{"domain":"d"}`)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Contains(t, rr.Body.String(), "required")
	})
}

func TestMemoryInsights_UpdateNotFound(t *testing.T) {
	s := newDBTestServer(t)
	// id 0 won't match any row.
	rr := serveVia(s, s.registerMemoryDBEndpoints, http.MethodPatch, "/api/v1/memory/insights/0", `{"title":"x"}`)
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "insight not found")
}

// TestMemoryInsights_MaxBytesReader sends a body larger than the 1MiB create
// limit; the decoder's first read fails on the cap and the handler returns 400.
func TestMemoryInsights_MaxBytesReader(t *testing.T) {
	s := newDBTestServer(t)
	huge := `{"domain":"d","title":"t","body":"` + strings.Repeat("z", (1<<20)+1024) + `"}`
	rr := serveVia(s, s.registerMemoryDBEndpoints, http.MethodPost, "/api/v1/memory/insights", huge)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid json")
}

// TestMemoryEntities_InvalidID asserts the UUID guard on the entity
// update/delete handlers (reached after the pool + schema checks, before any
// query — so it works even though the deepresearch schema is absent here).
func TestMemoryEntities_InvalidID(t *testing.T) {
	s := newDBTestServer(t)
	t.Setenv("R2R_PROJECT_NAME", "deepresearch") // valid schema name -> past the guard
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPatch, "/api/v1/memory/entities/not-a-uuid", `{}`},
		{http.MethodDelete, "/api/v1/memory/entities/not-a-uuid", ""},
		{http.MethodPatch, "/api/v1/memory/relationships/not-a-uuid", `{}`},
		{http.MethodDelete, "/api/v1/memory/relationships/not-a-uuid", ""},
	} {
		rr := serveVia(s, s.registerMemoryDBEndpoints, tc.method, tc.target, tc.body)
		require.Equal(t, http.StatusBadRequest, rr.Code, tc.target)
		require.Contains(t, rr.Body.String(), "invalid id", tc.target)
	}
}

// TestMemoryEntities_SchemaInjection503 confirms that a hostile R2R_PROJECT_NAME
// makes the entity/relationship handlers fail closed (500 from
// deepresearchSchema) rather than executing a query with an injected schema.
func TestMemoryEntities_SchemaInjection503(t *testing.T) {
	s := newDBTestServer(t)
	t.Setenv("R2R_PROJECT_NAME", "x; DROP TABLE evo.skills;--")
	rr := serveVia(s, s.registerMemoryDBEndpoints, http.MethodGet, "/api/v1/memory/entities", "")
	require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "unsafe R2R_PROJECT_NAME")
}

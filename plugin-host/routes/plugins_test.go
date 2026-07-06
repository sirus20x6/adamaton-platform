package routes

// Tests for the /platform/plugins/* surface: manifests, runs, items, and
// the encrypted config path (the "secret" handling in getConfigHandler /
// putConfigHandler backed by internal/secrets). Handler-only tests run
// everywhere; DB-backed tests target the local test Postgres and SKIP
// when it is unreachable (override with EVO_TEST_DSN), mirroring the
// dashboard test-helper convention.
//
// Note on masking: getConfigHandler currently returns the full decrypted
// blob (see the TODO in plugins.go) — the security property pinned here
// is that secrets are encrypted AT REST (the stored blob never contains
// plaintext) and that decrypt failures return an opaque 500 without
// leaking blob contents.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/secrets"
)

// ---------------------------------------------------------------------------
// test scaffolding
// ---------------------------------------------------------------------------

const defaultTestDSN = "postgres://postgres:postgres@localhost:5432/postgres"

func testDSN() string {
	if v := os.Getenv("EVO_TEST_DSN"); v != "" {
		return v
	}
	return defaultTestDSN
}

var (
	poolOnce sync.Once
	testPool *pgxpool.Pool
	poolErr  error
)

// pluginTablesDDL creates the platform.* tables plugin-host writes when
// they don't exist yet on the local test database. Column set mirrors
// the SQL in plugins.go / secrets.go.
const pluginTablesDDL = `
CREATE SCHEMA IF NOT EXISTS platform;
CREATE TABLE IF NOT EXISTS platform.plugin_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    plugin_id   text NOT NULL,
    source      text NOT NULL,
    mode        text,
    args        jsonb,
    corpus_id   uuid,
    status      text NOT NULL DEFAULT 'pending',
    totals      jsonb,
    error       text,
    started_at  timestamptz,
    finished_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS platform.plugin_items (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    plugin_id     text NOT NULL,
    external_id   text NOT NULL,
    external_url  text,
    title         text,
    metadata      jsonb,
    markdown_path text,
    document_id   uuid,
    ingest_status text NOT NULL DEFAULT 'pending',
    ingest_error  text,
    last_run_id   uuid,
    fetched_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS platform.plugin_config (
    user_id     text NOT NULL,
    plugin_id   text NOT NULL,
    config_blob bytea NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, plugin_id)
);
`

func sharedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, testDSN())
		if err != nil {
			poolErr = err
			return
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			poolErr = err
			return
		}
		if _, err := p.Exec(ctx, pluginTablesDDL); err != nil {
			p.Close()
			poolErr = err
			return
		}
		testPool = p
	})
	if poolErr != nil {
		t.Skipf("test database unavailable: %v", poolErr)
	}
	return testPool
}

func testManifests() map[string]*manifest.Manifest {
	return map[string]*manifest.Manifest{
		"zotero": {
			ID:           "zotero",
			Name:         "Zotero",
			Description:  "reference importer",
			Version:      "1.2.3",
			Category:     "importer",
			Icon:         "book",
			Capabilities: []string{"sync"},
			ConfigSchema: map[string]any{"type": "object"},
			ArgsSchema:   map[string]any{"type": "object"},
		},
	}
}

func newSecrets(t *testing.T, p *pgxpool.Pool) *secrets.Manager {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	m, err := secrets.New(p, base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("secrets.New: %v", err)
	}
	return m
}

// newRouter wires RegisterPlugins exactly like cmd/plugin-host does.
// pool / sec may be nil for tests that never reach the DB.
func newRouter(pool *pgxpool.Pool, sec *secrets.Manager) *mux.Router {
	logger := logrus.New()
	logger.SetOutput(bytes.NewBuffer(nil))
	r := mux.NewRouter()
	RegisterPlugins(r, pool, testManifests(), sec, logger)
	return r
}

func doJSON(t *testing.T, r http.Handler, method, path string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	var decoded map[string]any
	if rr.Body.Len() > 0 {
		if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s: non-JSON body %q", method, path, rr.Body.String())
		}
	}
	return rr, decoded
}

// ---------------------------------------------------------------------------
// manifests (no DB needed)
// ---------------------------------------------------------------------------

func TestListPluginsBothShapes(t *testing.T) {
	r := newRouter(nil, nil)
	for _, path := range []string{"/platform/plugins", "/platform/plugins/"} {
		rr, body := doJSON(t, r, http.MethodGet, path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		plugins, ok := body["plugins"].([]any)
		if !ok || len(plugins) != 1 {
			t.Fatalf("GET %s plugins = %v", path, body["plugins"])
		}
		entry := plugins[0].(map[string]any)
		if entry["id"] != "zotero" || entry["version"] != "1.2.3" {
			t.Errorf("entry = %v", entry)
		}
		if _, present := entry["config_schema"]; present {
			t.Error("config_schema must stay off the list endpoint")
		}
	}
}

func TestGetPlugin(t *testing.T) {
	r := newRouter(nil, nil)

	rr, body := doJSON(t, r, http.MethodGet, "/platform/plugins/zotero", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET plugin = %d", rr.Code)
	}
	if body["id"] != "zotero" {
		t.Errorf("id = %v", body["id"])
	}
	if _, present := body["config_schema"]; !present {
		t.Error("single-plugin endpoint must include config_schema")
	}

	rr, body = doJSON(t, r, http.MethodGet, "/platform/plugins/nope", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET unknown plugin = %d, want 404", rr.Code)
	}
	if body["ok"] != false || body["error"] != "plugin not found" {
		t.Errorf("error body = %v", body)
	}
}

// ---------------------------------------------------------------------------
// runs
// ---------------------------------------------------------------------------

func TestCreateRunUnknownPluginAndBadBody(t *testing.T) {
	r := newRouter(nil, nil)

	rr, _ := doJSON(t, r, http.MethodPost, "/platform/plugins/nope/run", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("run on unknown plugin = %d, want 404", rr.Code)
	}

	// Bad JSON body is rejected before any DB access (nil pool proves it).
	rr, body := doJSON(t, r, http.MethodPost, "/platform/plugins/zotero/run", "{nope")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rr.Code)
	}
	if body["ok"] != false {
		t.Errorf("error envelope = %v", body)
	}
}

func TestRunLifecycleAgainstDB(t *testing.T) {
	p := sharedPool(t)
	r := newRouter(p, nil)

	rr, body := doJSON(t, r, http.MethodPost, "/platform/plugins/zotero/run",
		`{"collection_id":"col-1","options":{"full":true}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create run = %d: %v", rr.Code, body)
	}
	runID, _ := body["run_id"].(string)
	if runID == "" || body["status"] != "pending" {
		t.Fatalf("create run body = %v", body)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM platform.plugin_runs WHERE id = $1`, runID)
	})

	// Single-run read.
	rr, body = doJSON(t, r, http.MethodGet, "/platform/plugins/runs/"+runID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get run = %d: %v", rr.Code, body)
	}
	if body["id"] != runID || body["plugin_id"] != "zotero" || body["status"] != "pending" {
		t.Errorf("run row = %v", body)
	}
	args, _ := body["args"].(map[string]any)
	if args == nil || args["collection_id"] != "col-1" {
		t.Errorf("run args = %v", body["args"])
	}

	// Listing scoped by plugin_id + status finds it.
	rr, body = doJSON(t, r, http.MethodGet,
		"/platform/plugins/runs?plugin_id=zotero&status=pending&limit=500", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list runs = %d", rr.Code)
	}
	found := false
	for _, it := range body["items"].([]any) {
		if it.(map[string]any)["id"] == runID {
			found = true
		}
	}
	if !found {
		t.Errorf("run %s not in filtered listing", runID)
	}
	if total, _ := body["total"].(float64); total < 1 {
		t.Errorf("total = %v, want >= 1", body["total"])
	}

	// A status filter that can't match excludes it.
	rr, body = doJSON(t, r, http.MethodGet,
		"/platform/plugins/runs?plugin_id=zotero&status=no-such-status", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list runs (filtered) = %d", rr.Code)
	}
	if n, _ := body["count"].(float64); n != 0 {
		t.Errorf("count = %v, want 0", body["count"])
	}

	// Unknown run id → 404.
	rr, _ = doJSON(t, r, http.MethodGet, "/platform/plugins/runs/"+uuid.NewString(), "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("get unknown run = %d, want 404", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// items
// ---------------------------------------------------------------------------

func insertItem(t *testing.T, p *pgxpool.Pool, pluginID, externalID, status string) string {
	t.Helper()
	var id string
	err := p.QueryRow(context.Background(), `
		INSERT INTO platform.plugin_items (plugin_id, external_id, title, ingest_status, metadata)
		VALUES ($1, $2, $3, $4, '{}'::jsonb)
		RETURNING id::text
	`, pluginID, externalID, "item "+externalID, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DELETE FROM platform.plugin_items WHERE id = $1`, id)
	})
	return id
}

func TestItemsListDeleteAndBulkDelete(t *testing.T) {
	p := sharedPool(t)
	r := newRouter(p, nil)

	// Unique plugin id per run so bulk deletes can't touch foreign rows.
	pluginID := "itemtest-" + uuid.NewString()[:8]
	idA := insertItem(t, p, pluginID, "ext-a", "pending")
	idB := insertItem(t, p, pluginID, "ext-b", "ingested")

	// List filtered by plugin_id.
	rr, body := doJSON(t, r, http.MethodGet,
		"/platform/plugins/items?plugin_id="+pluginID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list items = %d", rr.Code)
	}
	if n, _ := body["count"].(float64); n != 2 {
		t.Fatalf("count = %v, want 2", body["count"])
	}

	// Status filter narrows.
	rr, body = doJSON(t, r, http.MethodGet,
		"/platform/plugins/items?plugin_id="+pluginID+"&status=ingested", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list items (status) = %d", rr.Code)
	}
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != idB {
		t.Errorf("status-filtered items = %v", items)
	}

	// Single delete: 200 then 404 on repeat.
	rr, body = doJSON(t, r, http.MethodDelete, "/platform/plugins/items/"+idA, "")
	if rr.Code != http.StatusOK || body["deleted"] != float64(1) {
		t.Fatalf("delete item = %d %v", rr.Code, body)
	}
	rr, _ = doJSON(t, r, http.MethodDelete, "/platform/plugins/items/"+idA, "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("re-delete = %d, want 404", rr.Code)
	}

	// Wide-open bulk delete refused.
	rr, body = doJSON(t, r, http.MethodDelete, "/platform/plugins/items", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unfiltered bulk delete = %d, want 400", rr.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "filter required") {
		t.Errorf("error = %v", body)
	}

	// Scoped bulk delete removes the remainder.
	rr, body = doJSON(t, r, http.MethodDelete,
		"/platform/plugins/items?plugin_id="+pluginID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk delete = %d", rr.Code)
	}
	if body["deleted"] != float64(1) {
		t.Errorf("bulk deleted = %v, want 1 (only %s left)", body["deleted"], idB)
	}
}

// ---------------------------------------------------------------------------
// config: the secret-handling path
// ---------------------------------------------------------------------------

func TestConfigUnknownPluginAndBadBody(t *testing.T) {
	r := newRouter(nil, nil)

	rr, _ := doJSON(t, r, http.MethodGet, "/platform/plugins/nope/config", "")
	if rr.Code != http.StatusNotFound {
		t.Errorf("get config unknown plugin = %d, want 404", rr.Code)
	}
	rr, _ = doJSON(t, r, http.MethodPut, "/platform/plugins/nope/config", `{}`)
	if rr.Code != http.StatusNotFound {
		t.Errorf("put config unknown plugin = %d, want 404", rr.Code)
	}
	rr, _ = doJSON(t, r, http.MethodPut, "/platform/plugins/zotero/config", `{broken`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("put config bad body = %d, want 400", rr.Code)
	}
}

func TestConfigSecretRoundTripEncryptedAtRest(t *testing.T) {
	p := sharedPool(t)
	sec := newSecrets(t, p)
	r := newRouter(p, sec)
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(),
			`DELETE FROM platform.plugin_config WHERE plugin_id = 'zotero' AND user_id = 'singleton'`)
	})

	// No saved config yet → empty object, not an error.
	rr, body := doJSON(t, r, http.MethodGet, "/platform/plugins/zotero/config", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get empty config = %d: %v", rr.Code, body)
	}
	cfg, _ := body["config"].(map[string]any)
	if cfg == nil || len(cfg) != 0 {
		t.Errorf("empty config = %v, want {}", body["config"])
	}

	const apiKey = "zk-SUPER-SECRET-4242"
	rr, body = doJSON(t, r, http.MethodPut, "/platform/plugins/zotero/config",
		`{"api_key":"`+apiKey+`","library_id":"12345"}`)
	if rr.Code != http.StatusOK || body["ok"] != true {
		t.Fatalf("put config = %d %v", rr.Code, body)
	}

	// The at-rest blob must be ciphertext: no plaintext secret inside.
	var blob []byte
	err := p.QueryRow(context.Background(), `
		SELECT config_blob FROM platform.plugin_config
		WHERE user_id = 'singleton' AND plugin_id = 'zotero'
	`).Scan(&blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if bytes.Contains(blob, []byte(apiKey)) {
		t.Error("stored config_blob contains the plaintext secret")
	}
	if bytes.Contains(blob, []byte("api_key")) {
		t.Error("stored config_blob contains plaintext field names")
	}

	// Round-trip: the GET path decrypts back to the original values.
	rr, body = doJSON(t, r, http.MethodGet, "/platform/plugins/zotero/config", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("get config = %d", rr.Code)
	}
	cfg, _ = body["config"].(map[string]any)
	if cfg["api_key"] != apiKey || cfg["library_id"] != "12345" {
		t.Errorf("round-tripped config = %v", cfg)
	}
	if body["plugin_id"] != "zotero" {
		t.Errorf("plugin_id = %v", body["plugin_id"])
	}
}

func TestConfigDecryptFailureIsOpaque(t *testing.T) {
	p := sharedPool(t)
	sec := newSecrets(t, p)
	r := newRouter(p, sec)

	// Plant a blob this manager's key cannot decrypt (simulates key
	// rotation / corruption). The handler must 500 with a generic message
	// and must not echo blob contents.
	garbage := []byte("not-a-valid-nonce-or-ciphertext-SENTINEL")
	_, err := p.Exec(context.Background(), `
		INSERT INTO platform.plugin_config (user_id, plugin_id, config_blob, updated_at)
		VALUES ('singleton', 'zotero', $1, now())
		ON CONFLICT (user_id, plugin_id) DO UPDATE SET config_blob = EXCLUDED.config_blob
	`, garbage)
	if err != nil {
		t.Fatalf("plant garbage blob: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(),
			`DELETE FROM platform.plugin_config WHERE plugin_id = 'zotero' AND user_id = 'singleton'`)
	})

	rr, body := doJSON(t, r, http.MethodGet, "/platform/plugins/zotero/config", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("get corrupted config = %d, want 500", rr.Code)
	}
	if body["error"] != "read config failed" {
		t.Errorf("error = %v, want the opaque message", body["error"])
	}
	if strings.Contains(rr.Body.String(), "SENTINEL") {
		t.Error("response leaked blob contents")
	}
}

// ---------------------------------------------------------------------------
// pure helpers
// ---------------------------------------------------------------------------

func TestParseIntInRange(t *testing.T) {
	cases := []struct {
		in          string
		def, lo, hi int
		want        int
	}{
		{"", 50, 1, 500, 50},
		{"abc", 50, 1, 500, 50},
		{"25", 50, 1, 500, 25},
		{"0", 50, 1, 500, 1},
		{"-3", 50, 1, 500, 1},
		{"9999", 50, 1, 500, 500},
		{"500", 50, 1, 500, 500},
	}
	for _, c := range cases {
		if got := parseIntInRange(c.in, c.def, c.lo, c.hi); got != c.want {
			t.Errorf("parseIntInRange(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestBuildWhereClauses(t *testing.T) {
	where, args := buildRunWhere("", "")
	if where != "" || len(args) != 0 {
		t.Errorf("empty run where = %q %v", where, args)
	}
	where, args = buildRunWhere("zotero", "")
	if where != "WHERE plugin_id = $1" || len(args) != 1 {
		t.Errorf("plugin run where = %q %v", where, args)
	}
	where, args = buildRunWhere("zotero", "pending")
	if where != "WHERE plugin_id = $1 AND status = $2" || len(args) != 2 {
		t.Errorf("both run where = %q %v", where, args)
	}
	where, args = buildItemWhere("", "ingested")
	if where != "WHERE ingest_status = $1" || len(args) != 1 {
		t.Errorf("item where = %q %v", where, args)
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := join([]string{}, ", "); got != "" {
		t.Errorf("join empty = %q", got)
	}
	if got := join([]string{"a", "b", "c"}, " AND "); got != "a AND b AND c" {
		t.Errorf("join = %q", got)
	}
	if got := string(mustMarshal(map[string]any{"k": 1})); got != `{"k":1}` {
		t.Errorf("mustMarshal = %q", got)
	}
	if got := string(mustMarshal(func() {})); got != "{}" {
		t.Errorf("mustMarshal unmarshalable = %q, want {}", got)
	}
	if got := decodeJSON(nil); len(got.(map[string]any)) != 0 {
		t.Errorf("decodeJSON(nil) = %v", got)
	}
	if got := decodeJSON([]byte("{broken")); len(got.(map[string]any)) != 0 {
		t.Errorf("decodeJSON(bad) = %v", got)
	}
	if got := decodeJSON([]byte(`{"x":true}`)).(map[string]any); got["x"] != true {
		t.Errorf("decodeJSON = %v", got)
	}
	if strPtrOrNil(nil) != nil {
		t.Error("strPtrOrNil(nil) != nil")
	}
	s := "v"
	if strPtrOrNil(&s) != "v" {
		t.Error("strPtrOrNil deref failed")
	}
	if uuidPtrOrNil(nil) != nil {
		t.Error("uuidPtrOrNil(nil) != nil")
	}
	u := uuid.New()
	if uuidPtrOrNil(&u) != u.String() {
		t.Error("uuidPtrOrNil deref failed")
	}
	eb := errorBody("oops")
	if eb["ok"] != false || eb["error"] != "oops" {
		t.Errorf("errorBody = %v", eb)
	}
}

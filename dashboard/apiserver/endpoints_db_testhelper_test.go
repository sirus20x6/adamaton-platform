// DEPRECATED dashboard package, but its handlers are still load-bearing — these
// unit tests pin the behaviour while the surface is harvested.
//
// Shared test helpers for the DB-backed endpoint tests (datasets, skills,
// jobs, memory, workers, evo, nodes). The apiserver hard-requires Temporal to
// *boot*, but the individual handlers only read s.evoPool / s.config, so the
// tests construct an APIServer struct directly (white-box) and drive handlers
// through httptest — exactly like server_test.go's TestAuthMiddleware etc.
//
// The evoPool points at the locally-migrated evo DB (schema version 17). When
// that DB is unreachable the helper SKIPs rather than failing, so the package
// still builds + runs (auth / parse / validation tests) on a machine with no
// Postgres.
package apiserver

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/types"
)

// defaultTestDSN is the locally-migrated evo DB the wave-1 tests target. It is
// overridable via EVO_TEST_DSN for CI / alternate ports.
const defaultTestDSN = "postgres://postgres:postgres@localhost:5432/postgres"

func testDSN() string {
	if v := os.Getenv("EVO_TEST_DSN"); v != "" {
		return v
	}
	return defaultTestDSN
}

var (
	testPoolOnce sync.Once
	testPool     *pgxpool.Pool
	testPoolErr  error
)

// sharedTestPool dials the evo DB once and caches the pool (or the dial
// error). Cached so the dozens of DB-backed subtests don't each open a pool.
func sharedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testPoolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cfg, err := pgxpool.ParseConfig(testDSN())
		if err != nil {
			testPoolErr = err
			return
		}
		cfg.MaxConns = 4
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			testPoolErr = err
			return
		}
		if err := pool.Ping(ctx); err != nil {
			testPoolErr = err
			pool.Close()
			return
		}
		testPool = pool
	})
	if testPoolErr != nil || testPool == nil {
		t.Skipf("evo test DB unavailable at %s (set EVO_TEST_DSN): %v", testDSN(), testPoolErr)
	}
	return testPool
}

// newDBTestServer builds an APIServer wired to the real evo pool. temporalClient
// stays nil, so workflow-firing handlers take their "temporal not configured"
// 503 branch — which is exactly the error path several cards ask us to cover.
func newDBTestServer(t *testing.T) *APIServer {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return &APIServer{
		logger:  logger,
		config:  &types.Config{},
		router:  mux.NewRouter(),
		evoPool: sharedTestPool(t),
	}
}

// newPoollessServer builds an APIServer with a nil evoPool, for asserting the
// "evo pool not configured" 503 branch without touching a DB.
func newPoollessServer(t *testing.T) *APIServer {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return &APIServer{
		logger:  logger,
		config:  &types.Config{},
		router:  mux.NewRouter(),
		evoPool: nil,
	}
}

// serveVia registers the endpoint group on a fresh /api/v1 subrouter of the
// server's router and drives one request through it, so mux.Vars are populated
// for path-variable handlers. A new router is built per call to avoid
// duplicate-route panics across subtests sharing one APIServer.
func serveVia(s *APIServer, register func(*mux.Router), method, target, body string) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	s.router = router
	api := router.PathPrefix("/api/v1").Subrouter()
	register(api)

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

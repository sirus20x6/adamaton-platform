// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
package apiserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/cors"
	"github.com/sirupsen/logrus"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamaton-core/metrics"
	"github.com/sirus20x6/adamaton-core/types"
	"github.com/sirus20x6/adamaton-delegator/delegator"
	"github.com/sirus20x6/adamaton-delegator/delegator/llm"
	"github.com/sirus20x6/adamaton-evolve/workflow-builder/pluginloader"
	"github.com/sirus20x6/adamaton-evolve/workflow-builder/workflowstore"
	"github.com/sirus20x6/adamaton-platform/dashboard/apiserver/health"
	"github.com/sirus20x6/adamaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamaton-platform/temporal/workflows"
)

// defaultMaxInflightWorkflows bounds concurrent triggerWorkflow / runDefinition
// requests so a flood of POSTs from the dashboard or a misbehaving client
// can't exhaust the Temporal client connection pool. Override via
// GOGENTS_APISERVER_MAX_INFLIGHT_WORKFLOWS. Mirrors the webhook's pattern
// (cmd/gitea-webhook/main.go) so operators can reason about the two
// surfaces the same way.
const defaultMaxInflightWorkflows = 50

// maxInflightWorkflows reads the configured semaphore size at startup. Values
// <= 0 fall back to the default. Returned int sizes a buffered channel.
func maxInflightWorkflows() int {
	raw := os.Getenv("GOGENTS_APISERVER_MAX_INFLIGHT_WORKFLOWS")
	if raw == "" {
		return defaultMaxInflightWorkflows
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultMaxInflightWorkflows
	}
	return n
}

// resolvePluginDirs picks the plugin-YAML search paths for the workflow-node
// loader. Precedence:
//
//  1. EVO_PLUGIN_DIRS — colon-separated, honored verbatim (absolute paths
//     expected). Lets operators point at any layout (e.g. a shared mount).
//  2. EVO_HOME/plugins/{builtin,community,n8n} — the canonical layout the
//     Pi5 docker image and the /opt/evo systemd unit both install into.
//  3. ./plugins/{builtin,community,n8n} — last-resort relative paths, kept
//     for dev runs from the workflow-builder source tree.
//
// The previous hard-coded relative list silently loaded zero nodes in
// production because the docker image's WORKDIR is /, not the source tree.
func resolvePluginDirs() []string {
	if raw := os.Getenv("EVO_PLUGIN_DIRS"); raw != "" {
		parts := strings.Split(raw, ":")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	base := os.Getenv("EVO_HOME")
	if base == "" {
		base = "/opt/evo"
	}
	canonical := []string{
		filepath.Join(base, "plugins", "builtin"),
		filepath.Join(base, "plugins", "community"),
		filepath.Join(base, "plugins", "n8n"),
	}
	if _, err := os.Stat(canonical[0]); err == nil {
		return canonical
	}
	// Dev fallback: walk upward from cwd looking for the umbrella's
	// evolve/workflow-builder/plugins/builtin tree. Lets `go run ./cmd/api`
	// from anywhere inside the umbrella checkout find the catalog without
	// requiring EVO_PLUGIN_DIRS to be set.
	if cwd, err := os.Getwd(); err == nil {
		for cur := cwd; ; {
			candidate := filepath.Join(cur, "evolve", "workflow-builder", "plugins", "builtin")
			if _, err := os.Stat(candidate); err == nil {
				root := filepath.Dir(candidate) // .../plugins
				return []string{
					filepath.Join(root, "builtin"),
					filepath.Join(root, "community"),
					filepath.Join(root, "n8n"),
				}
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
		}
	}
	// Last-resort relative paths (dev run from workflow-builder source tree).
	return []string{"plugins/builtin", "plugins/community", "plugins/n8n"}
}

// temporalStarter is the narrow slice of client.Client the mutation handlers
// depend on. Pulling this out lets unit tests substitute a fake without
// dialing a real Temporal server (mirrors the webhook handler's pattern in
// cmd/gitea-webhook/main.go).
type temporalStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// temporalDescriber is the narrow slice of client.Client used by status-poll
// handlers (getRun, getWorkflowStatus). Same testability rationale as
// temporalStarter.
type temporalDescriber interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// temporalScheduler is the narrow slice of client.ScheduleClient the schedule
// CRUD endpoints depend on. Same testability rationale as the two interfaces
// above — handlers take this, tests inject a fake without dialing Temporal.
type temporalScheduler interface {
	Create(ctx context.Context, opts client.ScheduleOptions) (client.ScheduleHandle, error)
	GetHandle(ctx context.Context, scheduleID string) client.ScheduleHandle
	List(ctx context.Context, opts client.ScheduleListOptions) (client.ScheduleListIterator, error)
}

// Compile-time guards: a real client.Client must satisfy these interfaces so
// the APIServer can use it without an explicit cast. If a Temporal SDK
// upgrade ever changes a signature, the build fails instead of letting
// production code drift away from the test stubs.
var (
	_ temporalStarter   = (client.Client)(nil)
	_ temporalDescriber = (client.Client)(nil)
	_ temporalScheduler = (client.ScheduleClient)(nil)
)

type APIServer struct {
	logger         *logrus.Logger
	config         *types.Config
	temporalClient client.Client
	router         *mux.Router
	vllmClient     *llm.VLLMClient
	workflowStore  *workflowstore.Store
	delegatorStore *delegator.PgStore
	evoPool        evoPoolType
	pluginLoader   *pluginloader.Loader
	// giteaClient is used by mutation handlers (triggerWorkflow) to look up
	// the PR head SHA before scheduling a workflow so the merge step can pin
	// against the exact commit observed at review start. nil when Gitea is
	// not configured (GitHub-only deployments) — handlers fall through to
	// the empty-HeadSHA path with a warning, mirroring the workflow's own
	// fallback.
	giteaClient *gitea.GiteaClient
	// inflightSem caps concurrent mutation requests (workflow trigger /
	// dynamic run). GET endpoints are not bounded by this — they're cheap
	// and don't fan out into Temporal scheduling RPCs.
	inflightSem chan struct{}
	// prFetcher abstracts the Gitea PR lookup so unit tests can stub it
	// without standing up a real HTTP server. nil means "use giteaClient".
	prFetcher prFetcher
	// starter abstracts ExecuteWorkflow so unit tests can capture the
	// workflow ID and args. nil means "use temporalClient".
	starter temporalStarter
	// describer abstracts DescribeWorkflowExecution for the status-poll
	// path. nil means "use temporalClient".
	describer temporalDescriber
	// scheduler abstracts the Temporal ScheduleClient surface for the
	// /schedules endpoints. nil means "use temporalClient.ScheduleClient()".
	scheduler temporalScheduler
	// fleetHealth + fleetTopology back the /api/v1/health/{fleet,roles,
	// instances,topology} surface and the /platform/health/ready compat
	// shim. Both nil when topology.yml couldn't be loaded — handlers 503.
	fleetHealth   *health.Aggregator
	fleetTopology *health.Topology
}

// prFetcher is the narrow slice of *gitea.GiteaClient the trigger path needs.
// Defining the interface in the apiserver package keeps test stubs local.
type prFetcher interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int64) (*gitea.PullRequest, error)
}

// pickPRFetcher returns the configured stub when present, otherwise the
// production Gitea client wrapped to satisfy prFetcher (nil if neither).
func (s *APIServer) pickPRFetcher() prFetcher {
	if s.prFetcher != nil {
		return s.prFetcher
	}
	if s.giteaClient != nil {
		return s.giteaClient
	}
	return nil
}

// pickStarter returns the test stub if set, otherwise the production
// Temporal client.
func (s *APIServer) pickStarter() temporalStarter {
	if s.starter != nil {
		return s.starter
	}
	return s.temporalClient
}

// pickDescriber returns the test stub if set, otherwise the production
// Temporal client. nil is possible only in tests that skip the workflow
// poll path entirely.
func (s *APIServer) pickDescriber() temporalDescriber {
	if s.describer != nil {
		return s.describer
	}
	return s.temporalClient
}

// pickScheduler returns the test stub if set, otherwise the production
// Temporal client's ScheduleClient. Returns nil only when no Temporal
// connection was established, in which case the schedule handlers 503.
func (s *APIServer) pickScheduler() temporalScheduler {
	if s.scheduler != nil {
		return s.scheduler
	}
	if s.temporalClient == nil {
		return nil
	}
	return s.temporalClient.ScheduleClient()
}

type APIResponse struct {
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Success bool        `json:"success"`
}

type DashboardStats struct {
	TotalPRs       int     `json:"totalPRs"`
	TodayPRs       int     `json:"todayPRs"`
	AutoMerged     int     `json:"autoMerged"`
	IssuesFound    int     `json:"issuesFound"`
	CriticalIssues int     `json:"criticalIssues"`
	AvgReviewTime  string  `json:"avgReviewTime"`
	SuccessRate    float64 `json:"successRate"`
}

type PRReview struct {
	ID       int       `json:"id"`
	PRNumber int       `json:"prNumber"`
	Title    string    `json:"title"`
	Repo     string    `json:"repo"`
	Author   string    `json:"author"`
	Status   string    `json:"status"` // "merged", "review", "blocked"
	Score    float64   `json:"score"`
	Passed   int       `json:"passed"`
	Failed   int       `json:"failed"`
	Warning  int       `json:"warning"`
	Created  time.Time `json:"created"`
	TimeAgo  string    `json:"timeAgo"`
}

type AgentStatus struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"` // "active", "warning", "error"
	Weight      float64 `json:"weight"`
	Accuracy    float64 `json:"accuracy"`
	TotalChecks int     `json:"totalChecks"`
	Enabled     bool    `json:"enabled"`
}

type WorkflowTriggerRequest struct {
	PRNumber  int    `json:"prNumber"`
	RepoOwner string `json:"repoOwner"`
	RepoName  string `json:"repoName"`
}

func NewAPIServer(config *types.Config, logger *logrus.Logger) (*APIServer, error) {
	temporalClient, err := client.Dial(client.Options{
		HostPort:  config.Temporal.Address,
		Namespace: config.Temporal.Namespace,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Temporal client: %w", err)
	}

	llmTimeout := config.LLM.Timeout
	if llmTimeout == 0 {
		llmTimeout = 30 * time.Second
	}

	// Open workflow store (Postgres) for the workflow builder. Best-
	// effort: if postgres is unavailable, the builder endpoints will
	// be unavailable but the rest of the apiserver continues to serve.
	var wfStore *workflowstore.Store
	if config.Postgres.DSN != "" {
		wfStore, err = workflowstore.NewStore(config.Postgres.DSN, logger)
		if err != nil {
			logger.WithError(err).Warn("Failed to open workflow store; builder endpoints will be unavailable")
		}
	} else {
		logger.Warn("Postgres DSN not configured; workflow builder endpoints will be unavailable")
	}

	// Open the delegator task store. Best-effort: if postgres is
	// unavailable, the /api/delegator/tasks endpoint will report
	// 503; everything else continues. The MCP writer keeps its own
	// pool, so a brief apiserver outage doesn't cost writes.
	var delegatorStore *delegator.PgStore
	if config.Postgres.DSN != "" {
		delegatorStore, err = delegator.NewPgStore(config.Postgres.DSN, 0, logger)
		if err != nil {
			logger.WithError(err).Warn("Failed to open delegator task store; tasks endpoint will return 503")
		}
	}

	// Open a Postgres pool dedicated to the evo.* schema for the new
	// dashboard endpoints. Reads only — writes still flow through
	// evo-worker. Best-effort: when Postgres is unavailable, the
	// /api/v1/evo/* endpoints return 503 and the rest of the
	// apiserver continues to serve.
	var evoPool evoPoolType
	if config.Postgres.DSN != "" {
		poolCfg, perr := pgxpool.ParseConfig(config.Postgres.DSN)
		if perr != nil {
			logger.WithError(perr).Warn("Failed to parse Postgres DSN for evo pool; /api/v1/evo endpoints will return 503")
		} else {
			// MaxConns=16 covers the 6-way fan-out in system_status
			// (every refresh hits delegator/skills/evo/workflows on the
			// same pool, plus headroom for concurrent list endpoints).
			// MinConns=2 keeps a warm pool so the first request after
			// an idle window doesn't pay full connection setup.
			poolCfg.MaxConns = 16
			poolCfg.MinConns = 2
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			pool, oerr := pgxpool.NewWithConfig(ctx, poolCfg)
			cancel()
			if oerr != nil {
				logger.WithError(oerr).Warn("Failed to open evo pool; /api/v1/evo endpoints will return 503")
			} else {
				evoPool = pool
			}
		}
	}

	// Load plugin nodes from YAML files. Paths come from EVO_PLUGIN_DIRS
	// (colon-separated absolute paths) so the same binary works from any
	// working directory. Without that env var, fall back to EVO_HOME's
	// canonical layout (matches the Pi docker image's COPY destination
	// and the systemd unit's WorkingDirectory=/opt/evo). The pre-existing
	// relative-path list silently loaded zero nodes when the binary ran
	// from a WORKDIR other than the workflow-builder source tree.
	pl := pluginloader.NewLoader(resolvePluginDirs(), logger)
	if err := pl.LoadAll(); err != nil {
		logger.WithError(err).Warn("Failed to load plugins")
	}
	logger.WithField("count", pl.Count()).Info("Plugin loader initialized")

	// Build a Gitea client when Gitea is configured. Without it, the
	// triggerWorkflow handler can't look up the PR head SHA — the workflow
	// already handles HeadSHA="" by logging a warning and proceeding without
	// the SHA pin, so a missing Gitea config is not fatal at startup.
	var giteaClient *gitea.GiteaClient
	if config.Gitea.BaseURL != "" && config.Gitea.Token != "" {
		giteaClient = gitea.NewGiteaClient(config.Gitea, logger)
	}

	semSize := maxInflightWorkflows()
	logger.WithField("max_inflight_workflows", semSize).Info("APIServer inflight semaphore configured")

	server := &APIServer{
		logger:         logger,
		config:         config,
		temporalClient: temporalClient,
		router:         mux.NewRouter(),
		vllmClient:     llm.NewVLLMClient(config.LLM.Endpoint, llmTimeout, logger),
		workflowStore:  wfStore,
		delegatorStore: delegatorStore,
		evoPool:        evoPool,
		pluginLoader:   pl,
		giteaClient:    giteaClient,
		inflightSem:    make(chan struct{}, semSize),
	}

	// Load fleet-health topology (deploy/health/topology.yml). On failure
	// we log a warning + leave fleetHealth nil; the /api/v1/health/*
	// surface 503s, the SPA falls back to its degraded-pill state, and
	// the rest of the apiserver keeps working. Path is overridable for
	// per-host config layouts.
	if topo, agg := buildFleetHealth(evoPool, logger); topo != nil {
		server.fleetTopology = topo
		server.fleetHealth = agg
		// Spawn the refresh loop with a background context — apiserver
		// itself runs for the lifetime of the process. agg.Stop()
		// would belong in a future Shutdown path; today the process
		// exit takes the goroutine with it.
		go agg.Start(context.Background())
	}

	server.setupRoutes()

	// Persistent terminals: reconcile the evo.terminal_sessions table against
	// `tmux ls` once at boot (live rows whose tmux session vanished -> dead),
	// then start the ~60s reaper that kills orphan "adam-" sessions and keeps
	// the table honest. Both no-op when PTY_BACKEND=none or evoPool is nil.
	// The reaper runs on a background context for the lifetime of the process
	// — process exit takes the goroutine with it (same rationale as the
	// fleet-health refresh loop above).
	reconcileCtx, reconcileCancel := context.WithTimeout(context.Background(), 30*time.Second)
	server.ReconcileTerminals(reconcileCtx)
	reconcileCancel()
	server.StartTerminalReaper(context.Background())

	return server, nil
}

// buildFleetHealth loads deploy/health/topology.yml (path overridable
// via HEALTH_TOPOLOGY_PATH env) and constructs the aggregator. Returns
// nil/nil when the file is missing or invalid — caller logs + falls
// through; we don't fail server boot on health-check config.
func buildFleetHealth(pool evoPoolType, logger *logrus.Logger) (*health.Topology, *health.Aggregator) {
	path := os.Getenv("HEALTH_TOPOLOGY_PATH")
	if path == "" {
		// Two sensible defaults: the container-mount location from
		// docker-compose (/etc/adamaton/) and the repo-relative path
		// for local dev.
		for _, candidate := range []string{
			"/etc/adamaton/health-topology.yml",
			"deploy/health/topology.yml",
			"../../../deploy/health/topology.yml",
		} {
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
				break
			}
		}
	}
	if path == "" {
		logger.Warn("fleet-health: no topology.yml found; /api/v1/health/* will 503")
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		logger.WithError(err).WithField("path", path).
			Warn("fleet-health: open topology")
		return nil, nil
	}
	defer f.Close()
	topo, err := health.LoadTopology(f)
	if err != nil {
		logger.WithError(err).WithField("path", path).
			Warn("fleet-health: parse topology")
		return nil, nil
	}

	probers := health.Probers{
		HTTP:  health.NewHTTPProber(envBool("EVO_DASHBOARD_TLS_INSECURE")),
		TCP:   health.TCPProber{},
		Redis: health.RedisProber{},
	}
	// Postgres + TemporalQueue probers need the pool. evoPoolType is a
	// type alias for *pgxpool.Pool. When the apiserver doesn't have a
	// pool wired (Gitea-only deploys, pool is nil), those probes report
	// unknown / degraded.
	if pool != nil {
		probers.Postgres = &health.PostgresProber{Pool: pool}
		probers.TemporalQueue = &health.TemporalQueueProber{Pool: pool}
	}
	fleet := health.NewFleetClient()
	agg := health.NewAggregator(topo, fleet, probers, 15*time.Second, inferLocalHost())
	logger.WithFields(logrus.Fields{
		"path":         path,
		"roles":        len(topo.Roles),
		"capabilities": len(topo.Capabilities),
	}).Info("fleet-health: topology loaded")
	return topo, agg
}

// Router returns the HTTP router for testing
func (s *APIServer) Router() *mux.Router {
	return s.router
}

// metricsPathLabel returns a low-cardinality label value for the request's
// route. Mux's matched route template ("/api/v1/workflow/status/{workflowID}")
// is preferred over r.URL.Path so a flood of distinct workflow IDs does not
// each create their own time series. If the route can't be resolved (e.g.
// the request hit a 404), we fall back to a literal "unknown" label rather
// than the raw path, again to bound cardinality.
func (s *APIServer) metricsPathLabel(r *http.Request) string {
	if route := mux.CurrentRoute(r); route != nil {
		if tmpl, err := route.GetPathTemplate(); err == nil {
			return tmpl
		}
	}
	return "unknown"
}

func (s *APIServer) setupRoutes() {
	// /metrics is exposed at the top level (no auth, no /api/v1 prefix) so
	// Prometheus scrape configurations don't need API keys. This matches the
	// convention used by every server in the gogents stack and Prometheus
	// itself. If you need to lock /metrics down, do it at the ingress.
	s.router.Handle("/metrics", metrics.Handler()).Methods("GET")

	// Per-request HTTP metric middleware runs at the top level so /metrics
	// itself is also counted (this is fine — its cardinality is bounded by
	// the path label below).
	s.router.Use(metrics.Middleware("apiserver", s.metricsPathLabel))

	api := s.router.PathPrefix("/api/v1").Subrouter()
	api.Use(s.authMiddleware)

	// Dashboard endpoints
	api.HandleFunc("/dashboard/stats", s.getDashboardStats).Methods("GET")
	api.HandleFunc("/dashboard/recent-prs", s.getRecentPRs).Methods("GET")
	api.HandleFunc("/dashboard/agent-status", s.getAgentStatus).Methods("GET")

	// Workflow management
	api.HandleFunc("/workflow/trigger", s.triggerWorkflow).Methods("POST")
	api.HandleFunc("/workflow/status/{workflowID}", s.getWorkflowStatus).Methods("GET")

	// Configuration
	api.HandleFunc("/config/agents", s.getAgentConfig).Methods("GET")
	api.HandleFunc("/config/agents", s.updateAgentConfig).Methods("PUT")

	// LLM Backend Status and Testing
	api.HandleFunc("/llm/status", s.getLLMStatus).Methods("GET")
	api.HandleFunc("/llm/test", s.testLLMGeneration).Methods("POST")
	api.HandleFunc("/llm/vllm-info", s.getVLLMCompatibilityInfo).Methods("GET")

	// Workflow builder
	s.setupWorkflowBuilderRoutes(api)

	// Temporal schedules CRUD (list/create/get/update/delete + pause /
	// unpause / trigger). Reads namespace-wide regardless of workflow
	// kind; create accepts a delegation preset or an advanced
	// workflow+task-queue+args payload. Requires a live temporalClient
	// — handlers 503 if pickScheduler() returns nil.
	s.setupScheduleRoutes(api)

	// Delegator (read-only quota + task views; new delegations still go
	// through the MCP tool surface).
	s.setupDelegatorRoutes(api)

	// Evo (read-only runs / programs / insights views; writes happen
	// through evo-cli + evo-worker). Endpoints silently 503 when the
	// evoPool is unavailable.
	s.registerEvoEndpoints(api)

	// Skills library — CRUD over evo.skills. Same evoPool, same 503
	// behaviour when Postgres is missing. R2R sync + importers land
	// in later phases on top of this.
	s.registerSkillsEndpoints(api)

	// Memory: filesystem-backed agent memory (Claude Code, Codex,
	// Gemini, OpenCode) plus Postgres-backed insights/entities/
	// relationships. The filesystem half works regardless of
	// evoPool; the Postgres half 503s when evoPool is nil, same as
	// every other DB-bound endpoint.
	s.registerMemoryFileEndpoints(api)
	s.registerMemoryDBEndpoints(api)

	// Skills workflow status fan-out (sync / import / check-source).
	// Reads Temporal directly via DescribeWorkflowExecution /
	// QueryWorkflow — no DB state of its own.
	s.registerSkillsStatusEndpoints(api)

	// Distributed worker registry + dispatch ledger. /workers lists
	// self-registered worker rows from evo.workers; /jobs is the
	// dispatch surface — POST /jobs/submit starts a DispatchWorkflow
	// which routes the job onto a capable worker's task queue.
	s.registerWorkerEndpoints(api)
	s.registerRacksEndpoint(api)
	s.registerNodesEndpoints(api)
	s.registerJobsEndpoints(api)

	// Dataset manager — read views over evo_datasets + register/import
	// POSTs. The dataset-worker (evolve/dataset-manager) owns the
	// version lifecycle; this just reads + kicks Temporal workflows.
	s.registerDatasetsEndpoints(api)

	// Projects registry — CRUD over evo.projects, the backing store for
	// the dashboard "Projects" sidebar (file-tree, persistent terminals,
	// per-project Kanban land on top in later phases). Same evoPool, same
	// 503-when-nil behaviour. See docs/PROJECTS_KANBAN.md.
	s.registerProjectsEndpoints(api)

	// Persistent terminals — tmux-backed shells per project, with a
	// websocket bridge to `tmux attach`. Gated behind PTY_BACKEND (tmux|
	// none; default tmux); every handler 503s when the backend is "none"
	// or the evoPool is nil. The boot reconciler + reaper are started in
	// NewAPIServer. See docs/PROJECTS_KANBAN.md.
	s.registerTerminalEndpoints(api)

	// Per-project Kanban boards — CRUD over evo.kanban_* with an atomic
	// card-claim path the delegator MCP server drives over HTTP. Same
	// evoPool, same 503-when-nil behaviour. See docs/PROJECTS_KANBAN.md.
	s.registerKanbanEndpoints(api)

	// Cross-subsystem status fan-out for the unified landing page.
	api.HandleFunc("/system/status", s.handleSystemStatus).Methods("GET")

	// Fleet health: capability/role/instance rollup backed by
	// deploy/health/topology.yml + per-host deploy-agent + per-kind
	// probes. Endpoints 503 when topology failed to load.
	s.registerHealthEndpoints(api)

	// /platform/health/{ready,live} compat shim — the SPA's existing
	// useSystemHealth() hook polls these (left over from the retired
	// Python R2R backend's URL space). Routes adapt the new
	// aggregator's output to the old {ok, deps:{postgres,redis,sidecar}}
	// shape so the widget keeps working through the cutover.
	s.registerPlatformHealthCompat()

	// Read-only proxy onto the deepresearch Pi. Browsers can't talk
	// to the Pi directly without trusting its self-signed Caddy cert
	// and dealing with CORS; the proxy handles both server-side.
	s.registerResearchProxy(api)

	// Always-public minimal liveness probe. Mounted at the TOP level
	// (outside /api/v1) so it is reachable WITHOUT an API token — an
	// orchestrator livenessProbe must keep working when api.token is set,
	// and it must not leak internals. Returns a bare {"status":"ok"}.
	// /healthz is the conventional alias. Registered before the SPA
	// fallback so it always wins; /healthz is additionally excluded from
	// the SPA fallback by isAPIPath's /health prefix check.
	s.router.HandleFunc("/livez", s.liveness).Methods("GET")
	s.router.HandleFunc("/healthz", s.liveness).Methods("GET")

	// Health check (liveness vs. readiness):
	//   /api/v1/health        — DETAILED, AUTH-GATED liveness (version, agent
	//                            count). For the unauthenticated probe use
	//                            /livez or /healthz above.
	//   /api/v1/health/ready  — readiness probe; pings workflow store, Temporal,
	//                            and the LLM endpoint and returns 503 with details
	//                            if any upstream is unreachable.
	api.HandleFunc("/health", s.healthCheck).Methods("GET")
	api.HandleFunc("/health/ready", s.healthReady).Methods("GET")

	// Inline-HTML pages of the unified suite dashboard. Registered
	// BEFORE the SPA fallback so they always win over ui/dist when
	// present. The React app's Delegator + Workflows routes still
	// catch /delegator + /workflows via the SPA fallback below;
	// when/if we replace those with inline pages too, register them
	// here.
	s.router.HandleFunc("/", s.serveLanding).Methods("GET")
	s.router.HandleFunc("/evo", s.serveEvoDashboard).Methods("GET")
	s.router.HandleFunc("/skills", s.serveSkillsPage).Methods("GET")
	s.router.HandleFunc("/research", s.serveResearchPage).Methods("GET")

	// Serve static files (UI build output). For SPA routes — anything the
	// file server can't find that isn't an /api/, /metrics, /health path —
	// fall through to index.html so React Router can handle the path on
	// the client. Without this, deep links like /delegator return 404.
	s.router.PathPrefix("/").HandlerFunc(s.serveSPA)
}

// serveSPA handles the SPA fallback. Tries to serve a file from ui/dist;
// if missing and the path isn't a known API/system prefix, returns
// index.html. API/system paths get a real 404.
func (s *APIServer) serveSPA(w http.ResponseWriter, r *http.Request) {
	const distDir = "./ui/dist"

	// Reject path traversal attempts before touching the filesystem.
	clean := filepath.Clean(r.URL.Path)
	if strings.Contains(clean, "..") {
		http.NotFound(w, r)
		return
	}

	// Don't fall back for known API/system prefixes — those should 404
	// loudly so a typo in a fetch URL surfaces as an obvious failure
	// rather than a successful index.html that the JSON parser then
	// barfs on.
	if isAPIPath(clean) {
		http.NotFound(w, r)
		return
	}

	// Try the requested file first.
	candidate := filepath.Join(distDir, clean)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		http.ServeFile(w, r, candidate)
		return
	}

	// Fall back to index.html for any other path — React Router takes over.
	indexPath := filepath.Join(distDir, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, indexPath)
}

func isAPIPath(p string) bool {
	return strings.HasPrefix(p, "/api/") ||
		p == "/metrics" || strings.HasPrefix(p, "/metrics/") ||
		strings.HasPrefix(p, "/health")
}

// getDashboardStats is intentionally a 503 stub. The previous implementation
// returned hardcoded counts (1247 totalPRs, etc.) that have no relationship
// to anything in the workflow store or Temporal — the UI showed phantom
// numbers that never moved. Until a real query against workflowStore lands,
// this endpoint reports HTTP 503 with notImplemented:true so the UI can
// render a placeholder rather than mistake fixtures for live data.
func (s *APIServer) getDashboardStats(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
		Data: map[string]interface{}{
			"notImplemented": true,
		},
		Error:   "this endpoint is not yet implemented",
		Success: false,
	})
}

// getRecentPRs is intentionally a 503 stub. The previous implementation
// returned three hand-written PR records with fake authors, repos, and
// timestamps. Until this is wired to a real source, the endpoint reports
// notImplemented so the UI does not trust the response.
func (s *APIServer) getRecentPRs(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
		Data: map[string]interface{}{
			"notImplemented": true,
		},
		Error:   "this endpoint is not yet implemented",
		Success: false,
	})
}

// getAgentStatus is intentionally a 503 stub. The accuracy and totalChecks
// numbers in the previous version were invented constants; only the Enabled
// flag was real. Until this is backed by actual stats, the endpoint reports
// notImplemented. UI consumers should fall back to /config/agents (which is
// implemented) to see which agents are enabled.
func (s *APIServer) getAgentStatus(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusServiceUnavailable, APIResponse{
		Data: map[string]interface{}{
			"notImplemented": true,
		},
		Error:   "this endpoint is not yet implemented",
		Success: false,
	})
}

func (s *APIServer) triggerWorkflow(w http.ResponseWriter, r *http.Request) {
	// Bound concurrent mutation requests so a POST flood can't exhaust
	// the Temporal client connection pool. Mirrors the webhook handler's
	// inflight pattern. Acquire before reading the body so we don't pay
	// any further cost on rejection.
	if s.inflightSem != nil {
		select {
		case s.inflightSem <- struct{}{}:
			defer func() { <-s.inflightSem }()
		default:
			s.logger.Warn("triggerWorkflow rejected: inflight limit reached")
			s.sendJSON(w, http.StatusTooManyRequests, APIResponse{
				Error:   "too many in-flight workflow trigger requests",
				Success: false,
			})
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
	var req WorkflowTriggerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error:   "Invalid request format",
			Success: false,
		})
		return
	}

	if req.PRNumber <= 0 || req.RepoOwner == "" || req.RepoName == "" {
		s.sendJSON(w, http.StatusBadRequest, APIResponse{
			Error:   "Missing required fields: prNumber, repoOwner, repoName",
			Success: false,
		})
		return
	}

	// Look up the PR head SHA before scheduling so the merge activity can
	// pin the merge against the exact commit observed at review start
	// (closes the force-push race; see PRReviewArgs.HeadSHA docs). When
	// Gitea is not configured, fall through with HeadSHA="" — the workflow
	// logs a warning and proceeds without the pin so older callers still
	// work. A Gitea fetch failure surfaces as 502 Bad Gateway.
	var headSHA string
	if pf := s.pickPRFetcher(); pf != nil {
		// Cap the lookup at 10s so a wedged Gitea doesn't pin our inflight
		// slot indefinitely. The request context still wraps it, so a client
		// hangup also cancels the lookup.
		fetchCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		pr, err := pf.GetPullRequest(fetchCtx, req.RepoOwner, req.RepoName, int64(req.PRNumber))
		cancel()
		if err != nil {
			s.logger.WithError(err).WithFields(logrus.Fields{
				"pr":    req.PRNumber,
				"owner": req.RepoOwner,
				"repo":  req.RepoName,
			}).Error("Failed to fetch PR from Gitea")
			s.sendJSON(w, http.StatusBadGateway, APIResponse{
				Error:   "Failed to fetch PR from Gitea",
				Success: false,
			})
			return
		}
		headSHA = pr.Head.Sha
	}

	// Build a deterministic workflow ID so retries / re-triggers for the
	// same PR+SHA dedupe through Temporal's reuse policy. When the head SHA
	// is unknown (GitHub-only path), fall back to a UUID prefix so the ID
	// is still unique without leaking time.Now() coupling. Mirrors the
	// webhook's pattern (cmd/gitea-webhook/main.go).
	suffix := headSHA
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		// UUID rather than time.Now() so two POSTs <1s apart don't collide.
		suffix = uuid.New().String()[:8]
	}
	workflowID := fmt.Sprintf("pr-review-%s-%s-%d-%s",
		req.RepoOwner, req.RepoName, req.PRNumber, suffix)

	// Populate per-agent enable flags from the loaded config so the
	// dashboard's Security/Performance/Const toggles actually take effect on
	// workflow execution. Configured=true tells the workflow to honor the
	// individual booleans; without it, every agent runs unconditionally
	// (the historical default for backward compatibility — see
	// workflows.AgentEnabledFlags).
	args := workflows.PRReviewArgs{
		PRNumber:    req.PRNumber,
		RepoOwner:   req.RepoOwner,
		RepoName:    req.RepoName,
		HeadSHA:     headSHA,
		MergeMethod: s.config.Workflow.MergeMethod,
		Agents: workflows.AgentEnabledFlags{
			Configured: true,
			Enabled: map[string]bool{
				string(types.AgentSecurity):        s.config.Agents.Security.Enabled,
				string(types.AgentPerformance):     s.config.Agents.Performance.Enabled,
				string(types.AgentConst):           s.config.Agents.Const.Enabled,
				string(types.AgentDocumentation):   s.config.Agents.Documentation.Enabled,
				string(types.AgentTesting):         s.config.Agents.Testing.Enabled,
				string(types.AgentArchitecture):    s.config.Agents.Architecture.Enabled,
				string(types.AgentAccessibility):   s.config.Agents.Accessibility.Enabled,
				string(types.AgentCompliance):      s.config.Agents.Compliance.Enabled,
				string(types.AgentDependencies):    s.config.Agents.Dependencies.Enabled,
				string(types.AgentStyle):           s.config.Agents.Style.Enabled,
				string(types.AgentMaintainability): s.config.Agents.Maintainability.Enabled,
				string(types.AgentBusinessLogic):   s.config.Agents.BusinessLogic.Enabled,
			},
		},
	}

	// Workflow start is fire-and-forget: do NOT couple the scheduling RPC
	// to r.Context(). A client hangup mid-POST should not cancel the
	// workflow we just decided to schedule — the work is owned by Temporal
	// once accepted, and dropping it on a closed connection just turns a
	// successful start into a 500-then-retry-storm. We still cap at 30s so
	// a wedged Temporal frontend can't pin our inflight slot.
	execCtx, execCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer execCancel()
	we, err := s.pickStarter().ExecuteWorkflow(
		execCtx,
		client.StartWorkflowOptions{
			ID:                    workflowID,
			TaskQueue:             s.config.Temporal.TaskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		},
		workflows.PRReviewWorkflow,
		args,
	)

	if err != nil {
		// Treat WorkflowExecutionAlreadyStarted as an idempotent success: the
		// caller asked us to schedule a workflow that's already running for
		// the same PR+SHA, which is exactly what the deterministic workflow
		// ID is meant to do. Returning 200 lets retry-loops settle instead
		// of escalating.
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			s.logger.WithFields(logrus.Fields{
				"workflowId": workflowID,
				"pr":         req.PRNumber,
			}).Info("Workflow already running for this PR — returning existing workflow ID")
			s.sendJSON(w, http.StatusOK, APIResponse{
				Data: map[string]interface{}{
					"workflowID":     workflowID,
					"alreadyRunning": true,
				},
				Success: true,
			})
			return
		}
		s.logger.WithError(err).Error("Failed to start workflow")
		s.sendJSON(w, http.StatusInternalServerError, APIResponse{
			Error:   "Failed to start workflow",
			Success: false,
		})
		return
	}

	metrics.WorkflowsStarted.WithLabelValues("PRReviewWorkflow", "api").Inc()

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data: map[string]interface{}{
			"workflowID": workflowID,
			"runID":      we.GetRunID(),
		},
		Success: true,
	})
}

func (s *APIServer) getWorkflowStatus(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	workflowID := vars["workflowID"]

	desc, err := s.pickDescriber().DescribeWorkflowExecution(
		r.Context(),
		workflowID,
		"",
	)

	if err != nil {
		s.logger.WithError(err).Warn("Workflow lookup failed")
		s.sendJSON(w, http.StatusNotFound, APIResponse{
			Error:   "Workflow not found",
			Success: false,
		})
		return
	}

	status := map[string]interface{}{
		"workflowID": workflowID,
		"status":     desc.WorkflowExecutionInfo.Status.String(),
		"startTime":  desc.WorkflowExecutionInfo.StartTime,
		"closeTime":  desc.WorkflowExecutionInfo.CloseTime,
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    status,
		Success: true,
	})
}

func (s *APIServer) getAgentConfig(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    s.config.Agents,
		Success: true,
	})
}

func (s *APIServer) updateAgentConfig(w http.ResponseWriter, r *http.Request) {
	s.sendJSON(w, http.StatusNotImplemented, APIResponse{
		Error:   "Agent configuration updates are not yet implemented",
		Success: false,
	})
}

// liveness is the minimal, ALWAYS-PUBLIC liveness probe. It is mounted at
// the top level (/livez, /healthz) OUTSIDE the /api/v1 auth boundary so an
// orchestrator's livenessProbe keeps working when api.token is set — an
// auth-gated liveness endpoint would otherwise 401 the probe and trigger a
// restart loop. Crucially it discloses NOTHING about internals: no version,
// no agent count, no build info. Just "the process is accepting requests".
// The detailed /api/v1/health (healthCheck) stays behind auth precisely so
// those internals aren't readable by unauthenticated callers.
func (s *APIServer) liveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// healthCheck is a cheap liveness probe. It does not contact upstreams —
// kubernetes-style livenessProbe handlers should not depend on dependencies
// because a transient downstream blip would otherwise mark the API server
// itself as dead and cause a needless restart loop. For dependency checks,
// see healthReady (mounted at /health/ready).
//
// Unlike liveness (above), this lives under /api/v1 and is auth-gated when a
// token is configured; it MAY disclose coarse internals (version, agent
// count) to authenticated callers. Public liveness lives at /livez|/healthz.
func (s *APIServer) healthCheck(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now(),
		"version":   "1.0.0",
		"agents":    12,
	}

	s.sendJSON(w, http.StatusOK, APIResponse{
		Data:    health,
		Success: true,
	})
}

// healthReady is the readiness probe. Each upstream is checked with a tight
// per-check budget so a single slow dependency does not stall the response.
// On any failure we return HTTP 503 with a per-check status map; the request
// is still well-formed JSON the operator/UI can read.
func (s *APIServer) healthReady(w http.ResponseWriter, r *http.Request) {
	checks := make(map[string]map[string]interface{})
	allHealthy := true

	// Workflow store: list-1 is enough to confirm the DB and prepared
	// statements are alive. We bypass any context deadline because the store
	// is local SQLite and is expected to respond in microseconds.
	storeOK := true
	storeErr := ""
	if s.workflowStore == nil {
		storeOK = false
		storeErr = "workflow store not initialized"
	} else if _, err := s.workflowStore.ListDefinitions(); err != nil {
		storeOK = false
		storeErr = err.Error()
	}
	checks["workflow_store"] = map[string]interface{}{"ok": storeOK}
	if !storeOK {
		checks["workflow_store"]["error"] = storeErr
		allHealthy = false
	}

	// Temporal: prefer CheckHealth (gRPC health check), fall back to
	// DescribeNamespace if needed. 3s timeout — readiness must not hang.
	temporalOK := false
	temporalErr := ""
	if s.temporalClient != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, err := s.temporalClient.CheckHealth(ctx, &client.CheckHealthRequest{})
		cancel()
		if err == nil {
			temporalOK = true
		} else {
			temporalErr = err.Error()
		}
	} else {
		temporalErr = "temporal client not initialized"
	}
	checks["temporal"] = map[string]interface{}{"ok": temporalOK}
	if !temporalOK {
		checks["temporal"]["error"] = temporalErr
		allHealthy = false
	}

	// VLLM: piggyback on the existing Health() helper which probes the
	// configured LLM endpoint. 3s timeout for the same reason as Temporal.
	llmOK := false
	llmErr := ""
	if s.vllmClient != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		err := s.vllmClient.Health(ctx)
		cancel()
		if err == nil {
			llmOK = true
		} else {
			llmErr = err.Error()
		}
	} else {
		llmErr = "vllm client not initialized"
	}
	checks["llm"] = map[string]interface{}{"ok": llmOK}
	if !llmOK {
		checks["llm"]["error"] = llmErr
		allHealthy = false
	}

	statusCode := http.StatusOK
	if !allHealthy {
		statusCode = http.StatusServiceUnavailable
	}
	s.sendJSON(w, statusCode, APIResponse{
		Data: map[string]interface{}{
			"ready":  allHealthy,
			"checks": checks,
		},
		Success: allHealthy,
	})
}

func (s *APIServer) sendJSON(w http.ResponseWriter, status int, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		s.logger.WithError(err).Error("Failed to marshal JSON response")
		http.Error(w, `{"error":"internal server error","success":false}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err = w.Write(body); err != nil {
		s.logger.WithError(err).Warn("Failed to write response body")
		return
	}
	if _, err = w.Write([]byte("\n")); err != nil {
		s.logger.WithError(err).Warn("Failed to write response newline")
	}
}

func (s *APIServer) Start(port string) error {
	// Setup CORS
	c := cors.New(cors.Options{
		AllowedOrigins: s.config.API.CORSOrigins,
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "X-API-Key"},
	})

	handler := c.Handler(s.router)
	addr := s.listenAddress(port)
	if err := s.validateListenAddress(addr); err != nil {
		return err
	}
	// Loud startup guard for the empty-token posture. validateListenAddress
	// already refused a non-loopback bind without a token; this surfaces the
	// remaining loopback-dev case where authMiddleware fails open.
	s.warnAuthPosture()
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown. ListenAndServe returns http.ErrServerClosed
	// immediately after Shutdown is called, but Shutdown itself blocks until
	// in-flight handlers drain. We must wait for that drain to complete
	// before closing the Temporal client — otherwise still-running handlers
	// (ExecuteWorkflow, DescribeWorkflowExecution, CheckHealth) lose their
	// client mid-call. The WaitGroup joins the shutdown goroutine before
	// the client.Close() at the bottom of this function.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	var shutdownWG sync.WaitGroup
	shutdownWG.Add(1)
	go func() {
		defer shutdownWG.Done()
		<-quit
		s.logger.Info("Shutting down API server...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(ctx); err != nil {
			s.logger.WithError(err).Error("Server forced to shutdown")
		}
	}()

	s.logger.WithFields(logrus.Fields{"addr": addr, "auth_enabled": s.config.API.Token != ""}).Info("Starting API server")
	err := server.ListenAndServe()
	// Wait for Shutdown to finish draining in-flight handlers before
	// closing the Temporal client. If the listener died for some other
	// reason (port bind error, etc.) the shutdown goroutine is still
	// blocked on <-quit, so we signal it explicitly so the WaitGroup
	// doesn't deadlock.
	signal.Stop(quit)
	select {
	case quit <- syscall.SIGTERM:
	default:
	}
	shutdownWG.Wait()
	s.temporalClient.Close()
	return err
}

func (s *APIServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || s.config.API.Token == "" {
			next.ServeHTTP(w, r)
			return
		}

		token := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if token == "" {
			auth := strings.TrimSpace(r.Header.Get("Authorization"))
			if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
				token = strings.TrimSpace(auth[len("Bearer "):])
			}
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(s.config.API.Token)) != 1 {
			s.sendJSON(w, http.StatusUnauthorized, APIResponse{
				Error: "unauthorized", Success: false,
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *APIServer) listenAddress(port string) string {
	if port == "" {
		// Port 8080 is banned per project policy (see CLAUDE.md): it
		// collides with too many other dev services. 9123 is the
		// project's chosen fallback.
		port = "9123"
	}
	if strings.Contains(port, ":") {
		return port
	}
	return net.JoinHostPort(s.config.API.BindAddress, port)
}

func (s *APIServer) validateListenAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid API listen address %q: %w", addr, err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	ip := net.ParseIP(host)
	if s.config.API.Token == "" && (host == "0.0.0.0" || host == "::" || (ip != nil && !ip.IsLoopback())) {
		return fmt.Errorf("API_TOKEN is required when binding API to %s", addr)
	}
	return nil
}

// allowNoAuthEnvKey is the explicit dev opt-in that acknowledges running
// with no API token. validateListenAddress already hard-fails the
// dangerous case (empty token bound to a non-loopback address); this flag
// governs the remaining gap — an empty token on a loopback bind, where
// authMiddleware fails OPEN and every writable route (/workflow/trigger,
// /schedules, /kanban/…, /datasets, …) is reachable with no credential.
const allowNoAuthEnvKey = "GOGENTS_APISERVER_ALLOW_NO_AUTH"

// warnAuthPosture emits a loud, unmissable startup warning when the API
// is about to serve writable routes with authentication disabled
// (API token empty). validateListenAddress has already guaranteed the
// bind is loopback-only by the time we get here, so this is never silent
// exposure to the network — but a developer pointing a browser at
// http://127.0.0.1:9123 still gets an unauthenticated mutation surface,
// and a misconfigured deploy that dropped its token to "" should never
// do so quietly.
//
// We deliberately do NOT fail closed on the loopback case: that would
// break the long-standing "loopback + no token" dev workflow the
// listen-address contract explicitly allows (see TestValidateListenAddress).
// Instead we make the posture impossible to miss, and steer the operator
// toward either setting api.token or explicitly acknowledging the dev
// posture via GOGENTS_APISERVER_ALLOW_NO_AUTH=1 to silence the nag down
// to a single line.
func (s *APIServer) warnAuthPosture() {
	if s.config.API.Token != "" {
		return
	}
	if os.Getenv(allowNoAuthEnvKey) != "" {
		s.logger.Warn("API auth DISABLED (no api.token); proceeding because " +
			allowNoAuthEnvKey + " is set. Writable routes are unauthenticated — dev only.")
		return
	}
	s.logger.Warn("================================================================")
	s.logger.Warn("API AUTH IS DISABLED: api.token is empty.")
	s.logger.Warn("Every writable route (workflow trigger, schedules, kanban,")
	s.logger.Warn("datasets, memory, …) is reachable WITHOUT any credential.")
	s.logger.Warn("Set api.token (API_TOKEN) for any shared/staging/prod deploy.")
	s.logger.Warn("If this is intentional local dev, set " + allowNoAuthEnvKey + "=1")
	s.logger.Warn("to acknowledge it and collapse this banner to one line.")
	s.logger.Warn("================================================================")
}

// /thearray/gogents/cmd/gitea_webhook.go
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamaton-core/config"
	"github.com/sirus20x6/adamaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamaton-core/metrics"
	"github.com/sirus20x6/adamaton-core/types"
	"github.com/sirus20x6/adamaton-platform/temporal/workflows"
)

// defaultMaxInflightWebhooks bounds the number of webhook deliveries we
// process concurrently. Gitea will happily fire 1000+ deliveries during a
// rebase storm; without this cap we'd spawn one goroutine each, exhaust the
// Temporal client connection pool, and OOM the box. Override via
// GOGENTS_WEBHOOK_MAX_INFLIGHT.
const defaultMaxInflightWebhooks = 50

// maxInflightWebhooks reads the configured semaphore size at startup so
// operators can dial it up on bigger hosts. Values <= 0 fall back to the
// default. The variable is used to size the buffered channel below.
func maxInflightWebhooks() int {
	raw := os.Getenv("GOGENTS_WEBHOOK_MAX_INFLIGHT")
	if raw == "" {
		return defaultMaxInflightWebhooks
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultMaxInflightWebhooks
	}
	return n
}

// allowUnsignedWebhooks reports whether GOGENTS_ALLOW_UNSIGNED_WEBHOOKS is set
// to a truthy value (handles "1", "true", "TRUE", "yes", etc.). Unparseable
// values are treated as false.
func allowUnsignedWebhooks() bool {
	v, _ := strconv.ParseBool(os.Getenv("GOGENTS_ALLOW_UNSIGNED_WEBHOOKS"))
	return v
}

func main() {
	if err := run(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	// Load configuration
	cfg, err := config.LoadConfig("")
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Set up logging — honor cfg.Logging.Level (was hardcoded to InfoLevel,
	// which silently ignored the configured value). Mirrors cmd/api/main.go.
	logger := logrus.New()
	level, err := logrus.ParseLevel(cfg.Logging.Level)
	if err != nil {
		// Fall back to Info on a parse error rather than failing startup —
		// the webhook is a long-lived service and operators expect it to
		// boot even with a typo'd LOG_LEVEL.
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	if cfg.Logging.Format == "json" {
		logger.SetFormatter(&logrus.JSONFormatter{})
	}

	// Validate configuration
	if cfg.Gitea.BaseURL == "" || cfg.Gitea.Token == "" {
		return fmt.Errorf("Gitea configuration missing. Please set GITEA_BASE_URL and GITEA_TOKEN")
	}

	if cfg.Gitea.WebhookSecret == "" {
		if !allowUnsignedWebhooks() {
			return fmt.Errorf("GITEA_WEBHOOK_SECRET is required. Set GOGENTS_ALLOW_UNSIGNED_WEBHOOKS=true only for local development")
		}
		logger.Warn("GITEA_WEBHOOK_SECRET is not set; unsigned webhooks are allowed for local development")
	}

	// Create Temporal client
	temporalClient, err := client.Dial(client.Options{
		HostPort:  cfg.Temporal.Address,
		Namespace: cfg.Temporal.Namespace,
	})
	if err != nil {
		return fmt.Errorf("failed to create Temporal client: %w", err)
	}
	defer temporalClient.Close()

	// Create webhook handler. The inflight semaphore is created at startup
	// because per-request creation would defeat the purpose (each request
	// would get its own one-slot channel and never block).
	semSize := maxInflightWebhooks()
	logger.WithField("max_inflight", semSize).Info("Webhook inflight semaphore configured")
	handler := &WebhookHandler{
		config:         cfg,
		logger:         logger,
		temporalClient: temporalClient,
		inflightSem:    make(chan struct{}, semSize),
	}

	// Set up HTTP routes
	router := mux.NewRouter()
	// /metrics is exposed at the top level (no prefix) so a Prometheus
	// scraper can hit it without knowing about /webhook/. Per-request HTTP
	// metrics are wired in via metrics.Middleware below; the matched-route
	// label keeps cardinality bounded.
	router.Handle("/metrics", metrics.Handler()).Methods("GET")
	router.Use(metrics.Middleware("webhook", func(r *http.Request) string {
		if route := mux.CurrentRoute(r); route != nil {
			if tmpl, err := route.GetPathTemplate(); err == nil {
				return tmpl
			}
		}
		return "unknown"
	}))
	router.HandleFunc("/webhook/gitea", handler.HandleGiteaWebhook).Methods("POST")
	router.HandleFunc("/health", handler.HandleHealth).Methods("GET")
	router.HandleFunc("/status", handler.HandleStatus).Methods("GET")

	// Start HTTP server
	port := os.Getenv("WEBHOOK_PORT")
	if port == "" {
		port = "8090"
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           router,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Register signal handler BEFORE starting the listener goroutine so we
	// don't race against an early SIGINT/SIGTERM that lands while
	// ListenAndServe is still binding.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Start server in goroutine
	errCh := make(chan error, 1)
	go func() {
		logger.WithField("port", port).Info("Starting Gitea webhook server")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Wait for shutdown signal or server error
	select {
	case <-quit:
		// graceful shutdown
	case err := <-errCh:
		return fmt.Errorf("failed to start server: %w", err)
	}

	logger.Info("Shutting down webhook server...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Errorf("Server forced to shutdown: %v", err)
	}

	logger.Info("Webhook server stopped")
	return nil
}

// temporalStarter is the narrow slice of client.Client we depend on for
// scheduling workflows. Pulling this out lets tests substitute a fake
// without standing up a real Temporal server. The real *client.Client
// satisfies this implicitly.
type temporalStarter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
}

// WebhookHandler handles Gitea webhook events.
//
// inflightSem is a buffered semaphore that bounds the number of in-flight
// webhook deliveries we'll process concurrently. The buffer length is the
// concurrency limit; a successful send means we acquired the slot, a default
// branch means the limit is exceeded and we should reject with 429.
type WebhookHandler struct {
	config         *types.Config
	logger         *logrus.Logger
	temporalClient temporalStarter
	inflightSem    chan struct{}
}

// validateSignature validates the webhook signature using HMAC-SHA256
func (h *WebhookHandler) validateSignature(signature string, body []byte) bool {
	if h.config.Gitea.WebhookSecret == "" {
		return allowUnsignedWebhooks()
	}
	if signature == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(h.config.Gitea.WebhookSecret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(signature), []byte(expected))
}

// HandleGiteaWebhook processes incoming Gitea webhook events.
//
// Concurrency is bounded by an inflight semaphore: if more than
// `GOGENTS_WEBHOOK_MAX_INFLIGHT` deliveries are in flight, additional
// requests get HTTP 429 immediately. Gitea will retry, and dropping an
// excess request is much better than letting the box OOM under a delivery
// storm. The semaphore release is deferred so it always runs, even on a
// panic recovered higher up the stack.
func (h *WebhookHandler) HandleGiteaWebhook(w http.ResponseWriter, r *http.Request) {
	select {
	case h.inflightSem <- struct{}{}:
		defer func() { <-h.inflightSem }()
	default:
		// Semaphore full — reject without consuming any further resources.
		// We don't bother reading the body; Gitea will retry the delivery.
		metrics.WebhookRejected.Inc()
		h.logger.Warn("Webhook rejected: inflight limit reached")
		http.Error(w, "too many in-flight webhooks", http.StatusTooManyRequests)
		return
	}
	metrics.WebhookInflight.Inc()
	defer metrics.WebhookInflight.Dec()

	h.logger.Info("Received Gitea webhook")

	// Read request body (limit to 1MB)
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.WithError(err).Error("Failed to read webhook body")
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	// Validate webhook signature
	if !h.validateSignature(r.Header.Get("X-Gitea-Signature"), body) {
		h.logger.Warn("Invalid webhook signature")
		http.Error(w, "Invalid signature", http.StatusUnauthorized)
		return
	}

	// Parse webhook payload
	var payload gitea.WebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.WithError(err).Error("Failed to parse webhook payload")
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	h.logger.WithFields(logrus.Fields{
		"action":     payload.Action,
		"pr":         payload.Number,
		"repository": payload.Repository.FullName,
		"sender":     payload.Sender.Username,
	}).Info("Processing Gitea webhook")

	// Only process pull request events
	eventType := r.Header.Get("X-Gitea-Event")
	if eventType == "" {
		h.logger.Warn("Missing X-Gitea-Event header")
		http.Error(w, "Missing event type header", http.StatusBadRequest)
		return
	}
	if eventType != "pull_request" {
		h.logger.WithField("event", eventType).Info("Ignoring non-PR webhook event")
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.Action == "" {
		h.logger.Info("Ignoring webhook event with no action")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Handle different PR actions. Gitea sends the imperative
	// "synchronize" (not "synchronized" — see internal/gitea/types.go), and
	// matching the wrong spelling silently dropped force-push events on the
	// floor and defeated the HeadSHA pin.
	switch payload.Action {
	case "opened", "reopened", "synchronize":
		// Reject deliveries with an empty head SHA. The deterministic
		// workflow ID falls back to "nosha" when the SHA is missing, which
		// causes legitimate later deliveries for the same PR (with a real
		// SHA) to collide on a different ID space — and worse, two
		// no-SHA deliveries would collide with each other and either dedup
		// or fail. Returning 200 keeps Gitea from retrying forever.
		if payload.PullRequest.Head.Sha == "" {
			h.logger.WithFields(logrus.Fields{
				"pr":         payload.Number,
				"repository": payload.Repository.FullName,
			}).Warn("Webhook missing head SHA — skipping (would collide on workflow ID)")
			w.WriteHeader(http.StatusOK)
			return
		}

		// Trigger review workflow. We deliberately do NOT plumb r.Context()
		// here: workflow start is fire-and-forget. Gitea's default 5s
		// delivery timeout cancels r.Context() early, which would surface
		// as context.Canceled from ExecuteWorkflow, return 500, and Gitea
		// would retry — eating semaphore slots and racing itself. Use a
		// background context so the scheduling RPC outlives the inbound
		// request. triggerReview applies its own 30s cap.
		err := h.triggerReview(context.Background(), payload)
		if err == nil {
			break
		}
		// If the same workflow ID is already running (or completed
		// successfully) for this PR+SHA, Temporal returns
		// WorkflowExecutionAlreadyStarted because of our
		// ALLOW_DUPLICATE_FAILED_ONLY reuse policy. That's actually the
		// correct outcome for a duplicate webhook delivery — we just want
		// to ack 200 so Gitea stops retrying instead of returning 500
		// (which it would interpret as transient and re-deliver forever).
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			h.logger.WithFields(logrus.Fields{
				"pr":         payload.Number,
				"repository": payload.Repository.FullName,
			}).Info("Workflow already running for this PR — skipping duplicate webhook")
			w.WriteHeader(http.StatusOK)
			return
		}
		h.logger.WithError(err).Error("Failed to trigger review")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return

	case "closed":
		// PR was closed/merged, no action needed
		h.logger.Info("PR closed, no review needed")

	default:
		h.logger.WithField("action", payload.Action).Info("Ignoring webhook action")
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Webhook processed successfully")
}

// triggerReview starts a Gitea PR review workflow.
//
// The workflow ID is deterministic — derived from the repo, PR number, and
// head SHA — so Gitea webhook retries (which can re-deliver the same event)
// do not create duplicate workflows. WorkflowIDReusePolicy is set to
// ALLOW_DUPLICATE_FAILED_ONLY so an in-flight or successfully completed
// workflow for the same head SHA is not started again, but a previously
// failed run can be re-attempted.
//
// The caller passes a background context — workflow start is
// fire-and-forget, so we do NOT couple the scheduling RPC to the inbound
// HTTP request. Gitea's 5s delivery timeout would otherwise cancel the
// scheduling call, the handler would 500, and Gitea would retry, fighting
// the request that just succeeded on the worker side. We still impose a
// hard 30s timeout because a Temporal frontend that hangs past that is
// pathological and we'd rather fail fast than block the inflight slot.
func (h *WebhookHandler) triggerReview(ctx context.Context, payload gitea.WebhookPayload) error {
	// Use head SHA prefix for idempotency across Gitea webhook retries.
	// Empty headSha is rejected at the call site (see HandleGiteaWebhook)
	// to avoid the "nosha" workflow-ID collision; we still keep the
	// fallback here as belt-and-suspenders.
	headSha := payload.PullRequest.Head.Sha
	shaPrefix := headSha
	if len(shaPrefix) > 8 {
		shaPrefix = shaPrefix[:8]
	}
	if shaPrefix == "" {
		shaPrefix = "nosha"
	}

	workflowID := fmt.Sprintf("gitea-pr-%s-%s-%d-%s",
		payload.Repository.Owner.Username,
		payload.Repository.Name,
		payload.Number,
		shaPrefix,
	)

	// TODO(force-push race): when a force-push fires a synchronize event with a
	// new SHA, the prior workflow (different SHA suffix => different workflow ID)
	// is still running and will race the new one. The old workflow's merge
	// activity will see ErrHeadSHAMismatch and surface a "please re-trigger"
	// comment, while the new workflow merges normally — confusing, but NOT
	// data-corrupting because the merge activity pins on HeadSHA and the merge
	// itself is idempotent. A clean fix requires either:
	//   (a) a stable per-PR workflow ID with a head_sha_changed signal pattern, or
	//   (b) ListWorkflow + TerminateWorkflow scoped by custom search attributes
	//       (repository, pr_number) — not configured on this cluster, so a
	//       Query-by-WorkflowType scan would be unsafe at scale.
	// We deliberately keep per-SHA workflow IDs for observability and accept the
	// race. If operators see real user confusion, escalate to (a) and add the
	// signal handler in workflows/gitea_pr_review.go.

	// Prepare workflow arguments. Populate per-agent enable flags from the
	// loaded config so the dashboard's per-agent toggles take effect on
	// webhook-triggered runs (mirrors the apiserver's triggerWorkflow path).
	// HeadSHA is plumbed through so the merge activity can pin the merge
	// against the exact commit observed at review start; without this the
	// workflow logs "merging without SHA pin — vulnerable to force-push race"
	// on every real run. The handler already rejects deliveries with an
	// empty Head.Sha further up.
	workflowArgs := workflows.PRReviewArgs{
		PRNumber:    int(payload.Number),
		RepoOwner:   payload.Repository.Owner.Username,
		RepoName:    payload.Repository.Name,
		HeadSHA:     payload.PullRequest.Head.Sha,
		MergeMethod: h.config.Workflow.MergeMethod,
		Agents: workflows.AgentEnabledFlags{
			Configured: true,
			Enabled: map[string]bool{
				string(types.AgentSecurity):        h.config.Agents.Security.Enabled,
				string(types.AgentPerformance):     h.config.Agents.Performance.Enabled,
				string(types.AgentConst):           h.config.Agents.Const.Enabled,
				string(types.AgentDocumentation):   h.config.Agents.Documentation.Enabled,
				string(types.AgentTesting):         h.config.Agents.Testing.Enabled,
				string(types.AgentArchitecture):    h.config.Agents.Architecture.Enabled,
				string(types.AgentAccessibility):   h.config.Agents.Accessibility.Enabled,
				string(types.AgentCompliance):      h.config.Agents.Compliance.Enabled,
				string(types.AgentDependencies):    h.config.Agents.Dependencies.Enabled,
				string(types.AgentStyle):           h.config.Agents.Style.Enabled,
				string(types.AgentMaintainability): h.config.Agents.Maintainability.Enabled,
				string(types.AgentBusinessLogic):   h.config.Agents.BusinessLogic.Enabled,
			},
		},
	}

	h.logger.WithField("workflowId", workflowID).Info("Starting Gitea review workflow")

	// Start the workflow. The caller passes a background context (workflow
	// start is fire-and-forget — request-bound contexts get cancelled by
	// Gitea's 5s delivery timeout and break this entirely). We still cap
	// at 30s so a wedged Temporal frontend can't pin our inflight slot.
	execCtx, execCancel := context.WithTimeout(ctx, 30*time.Second)
	defer execCancel()
	we, err := h.temporalClient.ExecuteWorkflow(
		execCtx,
		client.StartWorkflowOptions{
			ID:                    workflowID,
			TaskQueue:             h.config.Temporal.TaskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		},
		workflows.GiteaPRReviewWorkflow,
		workflowArgs,
	)
	if err != nil {
		// Don't increment the metric on failure — that would conflate
		// "started" with "attempted to start". Callers handle the
		// already-started case as a successful idempotent skip.
		return fmt.Errorf("failed to start workflow: %w", err)
	}

	metrics.WorkflowsStarted.WithLabelValues("GiteaPRReviewWorkflow", "webhook").Inc()

	h.logger.WithFields(logrus.Fields{
		"workflowId": we.GetID(),
		"runId":      we.GetRunID(),
	}).Info("Gitea review workflow started successfully")

	return nil
}

// HandleHealth provides health check endpoint
func (h *WebhookHandler) HandleHealth(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC(),
		"service":   "gitea-webhook",
	})
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal health response")
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(data); err != nil {
		h.logger.WithError(err).Warn("Failed to write health response")
	}
}

// HandleStatus provides status information
func (h *WebhookHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(map[string]interface{}{
		"status":    "running",
		"timestamp": time.Now().UTC(),
		"service":   "gitea-webhook",
	})
	if err != nil {
		h.logger.WithError(err).Error("Failed to marshal status response")
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err = w.Write(data); err != nil {
		h.logger.WithError(err).Warn("Failed to write status response")
	}
}

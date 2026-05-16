// /thearray/gogents/activities/pr_review_activities.go - Core activities for PR review workflow
//
// Configuration model:
//
// The activity package reads three pieces of upstream config — VLLMEndpoint,
// MCPServerURL, GitHubToken — plus the Gitea client config. Operators have
// two ways to configure these:
//
//  1. Call SetActivityConfig(cfg) and SetGiteaConfig(giteaCfg) at worker
//     startup BEFORE the worker is started and any workflow registers. This
//     is the recommended path because it keeps everything routed through the
//     loaded types.Config so YAML defaults, viper bindings, and env
//     overrides all flow through one place.
//  2. Skip the SetActivityConfig calls entirely and rely on the env-var
//     fallbacks declared below (VLLM_ENDPOINT, MCP_SERVER_URL, GITHUB_TOKEN
//     plus the viper-loaded defaults inside getGiteaClient). This is the
//     legacy behavior preserved for backward compatibility.
//
// SetActivityConfig is idempotent — only the first call wins, so it is safe
// to call from multiple bootstrap paths.
package activities

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamomaton-core/config"
	"github.com/sirus20x6/adamomaton-core/envutil"
	"github.com/sirus20x6/adamomaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamomaton-core/metrics"
	"github.com/sirus20x6/adamomaton-core/types"
)

// defaultVLLMMaxConcurrent caps the number of concurrent connections to the
// vLLM/MCP host so a fan-out of activities cannot trivially saturate the
// upstream. Override via VLLM_MAX_CONCURRENT.
const defaultVLLMMaxConcurrent = 20

// httpClient is the shared resty client used by every activity that calls
// out over HTTP (vLLM, MCP, etc.). Two design rules:
//
//  1. NO SetTimeout. The previous "2 * time.Minute" silently capped activity
//     durations regardless of the Temporal context deadline, causing surprise
//     truncations on long activities. Callers MUST set ScheduleToCloseTimeout
//     and/or StartToCloseTimeout in workflow ActivityOptions, and every resty
//     call in this file MUST chain `.SetContext(ctx)` so the deadline reaches
//     net/http.
//  2. The transport bounds connections per host so a sudden fan-out of
//     workflows does not exhaust the upstream's connection slots. The limit
//     is configurable via VLLM_MAX_CONCURRENT (default 20).
var httpClient = newSharedHTTPClient()

func newSharedHTTPClient() *resty.Client {
	maxConns := defaultVLLMMaxConcurrent
	if raw := strings.TrimSpace(os.Getenv("VLLM_MAX_CONCURRENT")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxConns = parsed
		}
	}
	idle := maxConns / 2
	if idle < 1 {
		idle = 1
	}
	transport := &http.Transport{
		MaxConnsPerHost:     maxConns,
		MaxIdleConnsPerHost: idle,
		IdleConnTimeout:     90 * time.Second,
	}
	// No SetTimeout — rely on activity context deadline propagated via .SetContext(ctx).
	return resty.New().SetTransport(transport)
}

// maxDiffBytes caps the size of a diff response we will accept. Mirrors the
// Gitea client's own LimitReader bound. Diffs larger than this are rejected
// instead of streamed into memory.
const maxDiffBytes = 10 * 1024 * 1024

// maxCommentRespBytes bounds upstream response bodies for comment/merge calls
// so a malicious or misbehaving server cannot stream gigabytes into RAM.
const maxCommentRespBytes = 1 * 1024 * 1024

// readLimitedBody reads at most limit+1 bytes from r and reports overflow.
// This MUST be paired with `resty.SetDoNotParseResponse(true)` on the request
// so that resty hands us the raw response body to drain ourselves; otherwise
// resty has already buffered the entire body in RAM.
func readLimitedBody(r io.ReadCloser, limit int64) ([]byte, bool, error) {
	defer r.Close()
	limited := io.LimitReader(r, limit+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > limit {
		return buf[:limit], true, nil
	}
	return buf, false, nil
}

// authHeaderRegex matches whole `authorization: ...` lines in upstream
// response bodies, which some servers helpfully echo back. We consume the
// rest of the line (including any "Bearer <token>" tail) so the redacted
// output never contains the secret.
var authHeaderRegex = regexp.MustCompile(`(?i)authorization:[^\r\n]*`)

// sanitizeRespBody redacts authorization headers from upstream response bodies
// and truncates oversized payloads before they end up in error strings or logs.
func sanitizeRespBody(body string) string {
	body = authHeaderRegex.ReplaceAllString(body, "authorization: [redacted]")
	if len(body) > 1024 {
		body = body[:1024] + "...[truncated]"
	}
	return body
}

// randomShortHex returns a hex string of n bytes (2*n hex chars). On read
// failure we fall back to a deterministic-but-still-unlikely sentinel so the
// prompt boundary is still well-formed.
func randomShortHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

// setAuth attaches a bearer token to a resty request only when the token is
// non-empty. Sending `Authorization: Bearer ` (no value) confuses some servers.
func setAuth(req *resty.Request, token string) *resty.Request {
	if token != "" {
		req.SetHeader("Authorization", "Bearer "+token)
	}
	return req
}

// looksLikePRClosed reports whether err looks like the Gitea "PR is closed /
// not open" condition. Primary check is errors.Is against gitea.ErrPRClosed;
// substring fallback covers the MCP path which returns text-only errors with
// no typed sentinel.
func looksLikePRClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gitea.ErrPRClosed) {
		return true
	}
	// Fallback for the MCP path which returns ad-hoc text errors with no
	// typed sentinel.
	s := strings.ToLower(err.Error())
	for _, kw := range []string{
		"pull request is closed",
		"pull request closed",
		"pr is closed",
		"is not open",
		"not open",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// wrapMutatingActivityErr converts a state-mutating activity error (merge,
// comment, status set) into a typed *temporal.ApplicationError that the
// workflow layer can switch on via `errors.As` + `.Type()`. The reason strings
// — "already_merged", "head_sha_mismatch", "pr_closed" — are part of the
// cross-stream contract with Stream A (workflows); changing them silently
// breaks the workflow's typed error handling.
//
// Unknown errors are returned as-is so Temporal's retry policy still applies
// (a transient 5xx should retry; an "already_merged" should not).
//
// This is the LAST place where substring detection on err.Error() is
// permitted: Stream A's workflow code is removing string fallbacks and
// relying on the typed reason. The activity boundary MUST do the conversion.
func wrapMutatingActivityErr(err error) error {
	if err == nil {
		return nil
	}
	// Typed sentinels FIRST. Go's switch-with-comma evaluates every condition
	// in a case, so mixing `errors.Is(...)` with `strings.Contains(...)` in
	// the same case can misclassify a wrapped sentinel whose body excerpt
	// happens to contain a substring belonging to a different reason. Example:
	// fmt.Errorf("%w: head out of date — branch already merged in another PR",
	// gitea.ErrHeadSHAMismatch) was being tagged "already_merged" instead of
	// "head_sha_mismatch". Typed-first ordering eliminates the ambiguity.
	switch {
	case errors.Is(err, gitea.ErrAlreadyMerged):
		return temporal.NewApplicationErrorWithCause(err.Error(), "already_merged", err)
	case errors.Is(err, gitea.ErrHeadSHAMismatch):
		return temporal.NewApplicationErrorWithCause(err.Error(), "head_sha_mismatch", err)
	case errors.Is(err, gitea.ErrPRClosed):
		return temporal.NewApplicationErrorWithCause(err.Error(), "pr_closed", err)
	}
	// Substring fallback — for the MCP path which returns text-only errors,
	// never typed sentinels. Order is most-specific keyword first.
	errStr := err.Error()
	switch {
	case strings.Contains(errStr, "already merged") || strings.Contains(errStr, "is already merged"):
		return temporal.NewApplicationErrorWithCause(err.Error(), "already_merged", err)
	case strings.Contains(errStr, "sha does not match") ||
		strings.Contains(errStr, "head out of date") ||
		strings.Contains(errStr, "head_commit_id"):
		return temporal.NewApplicationErrorWithCause(err.Error(), "head_sha_mismatch", err)
	case looksLikePRClosed(err):
		return temporal.NewApplicationErrorWithCause(err.Error(), "pr_closed", err)
	}
	return err
}

// recordHeartbeatIfActivity records a Temporal heartbeat when ctx is an
// activity context, otherwise it is a no-op. Activities that block on long
// upstream calls (vLLM, MCP, Gitea) MUST call this just before the call so
// Temporal can detect a wedged worker via HeartbeatTimeout. Tests that drive
// these functions directly with a plain context.Context get the no-op path
// without panicking.
func recordHeartbeatIfActivity(ctx context.Context, details ...interface{}) {
	defer func() {
		// activity.RecordHeartbeat can panic in odd SDK regressions; we already
		// gate entry via IsActivity, so a panic here is genuinely unexpected.
		// Don't re-panic (that would tear down the heartbeat-pump goroutine and
		// quietly stop heartbeats, which is exactly the failure mode the pump
		// exists to prevent), but DO bump a metric so operators see the
		// regression instead of having Temporal silently kill the activity.
		if r := recover(); r != nil {
			metrics.PanicRecovered.WithLabelValues("heartbeat-pump").Inc()
			_ = r
		}
	}()
	if !activity.IsActivity(ctx) {
		return
	}
	activity.RecordHeartbeat(ctx, details...)
}

// startHeartbeatPump spawns a goroutine that records a Temporal heartbeat
// every interval until the returned cancel func is invoked or ctx is done.
// The first heartbeat fires immediately so Temporal sees activity
// progress before the upstream call begins.
//
// This addresses a class of bugs where an activity records ONE heartbeat
// up-front but then blocks for longer than HeartbeatTimeout on a single
// vLLM/MCP/Gitea call, causing Temporal to kill the activity mid-flight.
// Wire it just before the long upstream call:
//
//	stop := startHeartbeatPump(ctx, 10*time.Second)
//	defer stop()
//	resp, err := longUpstreamCall(ctx, ...)
//
// In a non-activity context (unit tests using plain context.Context),
// recordHeartbeatIfActivity is a no-op so the pump just spins harmlessly
// until cancelled.
func startHeartbeatPump(ctx context.Context, interval time.Duration) func() {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	// Initial heartbeat so Temporal sees progress immediately.
	recordHeartbeatIfActivity(ctx, "heartbeat-pump-start")
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				recordHeartbeatIfActivity(ctx, "heartbeat-pump-tick")
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopCh)
			<-doneCh
		})
	}
}

// Activity argument types
type FetchDiffArgs struct {
	PRNumber  int    `json:"pr_number"`
	RepoOwner string `json:"repo_owner"`
	RepoName  string `json:"repo_name"`
}

type MergeArgs struct {
	PRNumber    int    `json:"pr_number"`
	RepoOwner   string `json:"repo_owner"`
	RepoName    string `json:"repo_name"`
	MergeMethod string `json:"merge_method,omitempty"` // "merge", "squash", "rebase", "rebase-merge"
	// HeadSHA, when non-empty, pins the merge to this commit. Gitea will
	// reject the merge with gitea.ErrHeadSHAMismatch if the PR's current
	// head no longer matches — closing the force-push race during review.
	// The workflow is responsible for populating this from the PR snapshot
	// captured at review start.
	HeadSHA string `json:"head_sha,omitempty"`
}

type CommentArgs struct {
	PRNumber       int              `json:"pr_number"`
	RepoOwner      string           `json:"repo_owner"`
	RepoName       string           `json:"repo_name"`
	FailingReasons []string         `json:"failing_reasons"`
	Metrics        *AnalysisMetrics `json:"metrics,omitempty"`
}

// CheckResult represents the result of an agent's analysis
type CheckResult struct {
	Agent     string           `json:"agent"`
	Verdict   string           `json:"verdict"` // "PASS" or "FAIL"
	Rationale string           `json:"rationale"`
	Metrics   *AnalysisMetrics `json:"metrics,omitempty"`
}

// AnalysisMetrics provides detailed analysis information
type AnalysisMetrics struct {
	AffectedFiles     []string            `json:"affected_files"`
	AffectedFunctions []string            `json:"affected_functions"`
	Languages         map[string][]string `json:"languages"`
	TestFiles         []string            `json:"test_files"`
	CriticalFiles     []string            `json:"critical_files"`
	AddedLines        int                 `json:"added_lines"`
	RemovedLines      int                 `json:"removed_lines"`
	ComplexityScore   int                 `json:"complexity_score"`
}

// Environment variables for activity configuration.
//
// These globals are seeded from env at package init for backward
// compatibility. SetActivityConfig (below) overrides them at worker startup
// when a loaded types.Config is available. The env path remains active only
// when SetActivityConfig is NOT called.
//
// TODO: Replace package globals with activity struct pattern to allow
// per-config overrides. The struct pattern would also kill the configOnce
// dance below.
var (
	VLLMEndpoint = envutil.GetEnvOrDefault("VLLM_ENDPOINT", "http://vllm.local:8000/generate")
	MCPServerURL = envutil.GetEnvOrDefault("MCP_SERVER_URL", "http://localhost:3000")
	GitHubToken  = os.Getenv("GITHUB_TOKEN")
)

// configOnce guards SetActivityConfig so multiple bootstrap callers are safe
// and operators can't accidentally swap config out from under in-flight
// activities.
var configOnce sync.Once

// SetActivityConfig overrides the package-level activity globals from a
// loaded types.Config. Should be called during worker startup BEFORE any
// workflow is registered so all activity invocations see consistent config.
//
// Idempotent: only the first call has an effect. Subsequent calls are
// silently ignored — callers should treat this as fire-once at bootstrap.
//
// Empty fields in cfg do NOT clobber existing values, so this safely
// composes with the env-driven defaults: anything cfg leaves blank keeps
// whatever VLLM_ENDPOINT / MCP_SERVER_URL / GITHUB_TOKEN already supplied.
func SetActivityConfig(cfg types.Config) {
	configOnce.Do(func() {
		// VLLMEndpoint must be a full URL ending in /generate (the package
		// default is "http://vllm.local:8000/generate"). cfg.LLM.Endpoint is
		// the bare host (default "http://localhost:8000") used by the richer
		// internal/llm client, NOT the same shape. Prefer cfg.VLLM.Endpoint
		// when set (it carries the correct full-URL contract); otherwise
		// derive the URL from cfg.LLM.Endpoint by appending "/generate" so
		// the LLM-only config path still works.
		endpoint := cfg.VLLM.Endpoint
		if endpoint == "" && cfg.LLM.Endpoint != "" {
			endpoint = strings.TrimSuffix(cfg.LLM.Endpoint, "/") + "/generate"
		}
		if endpoint != "" {
			VLLMEndpoint = endpoint
		}
		if cfg.MCP.ServerURL != "" {
			MCPServerURL = cfg.MCP.ServerURL
		}
		if cfg.GitHub.Token != "" {
			GitHubToken = cfg.GitHub.Token
		}
	})
}

// giteaConfigOverride, if non-nil, replaces the viper-loaded GiteaConfig
// inside getGiteaClient. Set via SetGiteaConfig at worker startup.
//
// Protected by giteaConfigOnce so the override race-free wins exactly once.
var (
	giteaConfigOnce     sync.Once
	giteaConfigOverride *types.GiteaConfig
)

// SetGiteaConfig pins the Gitea client to the supplied config instead of the
// viper-loaded defaults inside getGiteaClient. Should be called during
// worker startup BEFORE any workflow is registered.
//
// Idempotent: only the first call has an effect.
func SetGiteaConfig(cfg types.GiteaConfig) {
	giteaConfigOnce.Do(func() {
		c := cfg
		giteaConfigOverride = &c
	})
}

// FetchDiffActivity retrieves the PR diff via GitHub MCP
func FetchDiffActivity(ctx context.Context, args FetchDiffArgs) (string, error) {
	payload := map[string]interface{}{
		"method": "getPullRequestDiff",
		"params": map[string]interface{}{
			"owner":    args.RepoOwner,
			"repo":     args.RepoName,
			"prNumber": args.PRNumber,
		},
	}

	// SetDoNotParseResponse(true) forces resty to hand us the raw body so we
	// can bound it via io.LimitReader. Without this, resty buffers the entire
	// upstream body in RAM before our size check ever runs — a 100 GB diff
	// would OOM the worker.
	req := httpClient.R().
		SetContext(ctx).
		SetDoNotParseResponse(true).
		SetHeader("Content-Type", "application/json").
		SetBody(payload)
	setAuth(req, GitHubToken)

	recordHeartbeatIfActivity(ctx, "calling mcp fetch diff")
	// Heartbeat pump: a single heartbeat at call-start is not enough for
	// large diffs that can take longer than HeartbeatTimeout to download.
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()
	resp, err := req.Post(MCPServerURL)

	if err != nil {
		return "", fmt.Errorf("failed to call MCP server: %w", err)
	}

	rawBody := resp.RawBody()
	body, overflow, readErr := readLimitedBody(rawBody, maxDiffBytes)
	if readErr != nil {
		return "", fmt.Errorf("failed to read MCP response body: %w", readErr)
	}
	if overflow {
		return "", fmt.Errorf("MCP diff response too large (max %d bytes)", maxDiffBytes)
	}

	if resp.StatusCode() >= 400 {
		return "", fmt.Errorf("MCP server returned status %d: %s", resp.StatusCode(), sanitizeRespBody(string(body)))
	}

	var response struct {
		Result struct {
			Diff string `json:"diff"`
		} `json:"result"`
		Error interface{} `json:"error"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to parse MCP response: %w", err)
	}

	if response.Error != nil {
		return "", fmt.Errorf("MCP server error: %v", response.Error)
	}

	return response.Result.Diff, nil
}

// GiteaFetchDiffActivity retrieves the PR diff via the configured Gitea API.
func GiteaFetchDiffActivity(ctx context.Context, args FetchDiffArgs) (string, error) {
	client, err := getGiteaClient()
	if err != nil {
		return "", err
	}
	recordHeartbeatIfActivity(ctx, "calling gitea fetch diff")
	// Large diffs can take longer than HeartbeatTimeout to fetch; pump
	// heartbeats throughout so Temporal does not kill the activity.
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()
	return client.GetPullRequestDiff(ctx, args.RepoOwner, args.RepoName, int64(args.PRNumber))
}

// AnalyzeDiffEnhanced provides comprehensive diff analysis using Week 2 capabilities
func AnalyzeDiffEnhanced(diff string) *AnalysisMetrics {
	diffAnalysis := AnalyzeDiffMetrics(diff)

	// Categorize files by language
	languages := CategorizeFilesByLanguage(diffAnalysis.MentionedFiles)

	// Identify test and critical files
	var testFiles []string
	var criticalFiles []string

	for _, file := range diffAnalysis.MentionedFiles {
		if IsTestFile(file) {
			testFiles = append(testFiles, file)
		}
		if IsCriticalFile(file) {
			criticalFiles = append(criticalFiles, file)
		}
	}

	// Calculate complexity score based on various factors
	complexityScore := calculateComplexityScore(diffAnalysis)

	return &AnalysisMetrics{
		AffectedFiles:     diffAnalysis.MentionedFiles,
		AffectedFunctions: diffAnalysis.MentionedFunctions,
		Languages:         languages,
		TestFiles:         testFiles,
		CriticalFiles:     criticalFiles,
		AddedLines:        diffAnalysis.AddedLines,
		RemovedLines:      diffAnalysis.RemovedLines,
		ComplexityScore:   complexityScore,
	}
}

// calculateComplexityScore assigns a complexity score based on diff metrics
func calculateComplexityScore(analysis DiffAnalysis) int {
	score := 0

	// Base score from lines changed
	score += analysis.AddedLines + analysis.RemovedLines

	// Extra points for multiple files
	score += len(analysis.MentionedFiles) * 5

	// Extra points for function changes
	score += len(analysis.MentionedFunctions) * 10

	// Complexity tiers
	if score < 50 {
		return 1 // Low complexity
	} else if score < 150 {
		return 2 // Medium complexity
	} else if score < 300 {
		return 3 // High complexity
	} else {
		return 4 // Very high complexity
	}
}

// SecurityCheckActivity performs AI-based security vulnerability analysis with enhanced metrics
func SecurityCheckActivity(ctx context.Context, diff string) (CheckResult, error) {
	// Perform enhanced diff analysis
	metrics := AnalyzeDiffEnhanced(diff)

	// Create context-aware security prompt
	prompt := buildSecurityPrompt(diff, metrics)

	result, err := callVLLMAgent(ctx, prompt, "Security")
	if err != nil {
		return result, err
	}

	// Attach metrics to result
	result.Metrics = metrics

	return result, nil
}

// PerformanceCheckActivity performs AI-based performance analysis with enhanced metrics
func PerformanceCheckActivity(ctx context.Context, diff string) (CheckResult, error) {
	// Perform enhanced diff analysis
	metrics := AnalyzeDiffEnhanced(diff)

	// Create context-aware performance prompt
	prompt := buildPerformancePrompt(diff, metrics)

	result, err := callVLLMAgent(ctx, prompt, "Performance")
	if err != nil {
		return result, err
	}

	// Attach metrics to result
	result.Metrics = metrics

	return result, nil
}

// ConstCheckActivity performs const correctness analysis with enhanced metrics
func ConstCheckActivity(ctx context.Context, diff string) (CheckResult, error) {
	// Perform enhanced diff analysis
	metrics := AnalyzeDiffEnhanced(diff)

	// Create context-aware const correctness prompt
	prompt := buildConstPrompt(diff, metrics)

	result, err := callVLLMAgent(ctx, prompt, "Const")
	if err != nil {
		return result, err
	}

	// Attach metrics to result
	result.Metrics = metrics

	return result, nil
}

// callVLLMAgent makes a call to the vLLM endpoint and parses the response.
// ctx is propagated to resty so Temporal cancellations / heartbeats work.
func callVLLMAgent(ctx context.Context, prompt, agentType string) (CheckResult, error) {
	payload := map[string]interface{}{
		"prompt":     prompt,
		"max_tokens": 256,
	}

	// Heartbeat just before blocking on a potentially-minute-long inference
	// so a HeartbeatTimeout in workflow ActivityOptions can detect a wedged
	// worker even while we're still waiting for the response. A single
	// heartbeat is insufficient for vLLM responses that exceed
	// HeartbeatTimeout (commonly 30s); the pump ticks every 10s for the
	// duration of the call.
	recordHeartbeatIfActivity(ctx, "calling vllm "+agentType)
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()

	resp, err := httpClient.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		Post(VLLMEndpoint)

	if err != nil {
		return CheckResult{}, fmt.Errorf("failed to call vLLM: %w", err)
	}

	if resp.StatusCode() >= 400 {
		return CheckResult{}, fmt.Errorf("vLLM returned status %d: %s", resp.StatusCode(), sanitizeRespBody(resp.String()))
	}

	var response struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal(resp.Body(), &response); err != nil {
		return CheckResult{}, fmt.Errorf("failed to parse vLLM response: %w", err)
	}

	return parseAgentResponse(response.Text, agentType), nil
}

// parseAgentResponse parses the agent's response into verdict and rationale.
//
// The verdict comes from the FIRST line only, using strict prefix matching so
// stray occurrences of "PASS"/"FAIL"/"WARNING" later in the response (or in
// rationale prose like "this PASS check is bad") do not flip the verdict.
//
// The "REVIEW" fallback verdict means "the agent could not produce a clear
// PASS/FAIL/WARNING verdict from its first line" — NOT "the user must hand-
// review this PR". Downstream workflow logic (makeEnhancedDecision) is
// responsible for translating "REVIEW" into a merge/block/escalate decision;
// the parser only reports what the agent said. Empty responses still map to
// FAIL (a missing verdict is a stronger signal than an unparseable one).
//
// PARSER DRIFT WARNING (Pass 11 audit): a richer parser also exists at
// internal/llm.(*VLLMClient).parseAgentResponse, which fills the 7-field
// types.LLMCheckResult (verdict, confidence, severity, category, rationale,
// details, etc.) using the VERDICT/CONFIDENCE/SEVERITY/RATIONALE/DETAILS
// header schema. This activity-level parser only fills 4 fields and
// understands a looser format. The two are kept separate today because:
//
//   1. They return different types (CheckResult vs LLMCheckResult).
//   2. CheckResult is what activities/workflows consume on the hot path,
//      and switching the activity layer to LLMCheckResult is a multi-file
//      refactor (workflow decision logic, comment building, etc.).
//
// TODO(audit-pass-11): consolidate by either (a) making this function call
// internal/llm.parseAgentResponse and downcasting the result, or (b) lifting
// LLMCheckResult into types/ so the workflow layer can use the richer
// fields directly. Until then, any change to one parser MUST be mirrored in
// the other if it touches verdict-line semantics.
func parseAgentResponse(rawText, agentType string) CheckResult {
	trimmed := strings.TrimSpace(rawText)
	if trimmed == "" {
		return CheckResult{
			Agent:     agentType,
			Verdict:   "FAIL",
			Rationale: "Empty response from agent",
		}
	}

	lines := strings.Split(trimmed, "\n")
	firstLine := strings.TrimSpace(strings.ToUpper(lines[0]))
	verdict := "REVIEW"

	switch {
	case strings.HasPrefix(firstLine, "VERDICT: PASS"), strings.HasPrefix(firstLine, "PASS"):
		verdict = "PASS"
	case strings.HasPrefix(firstLine, "VERDICT: FAIL"), strings.HasPrefix(firstLine, "FAIL"):
		verdict = "FAIL"
	case strings.HasPrefix(firstLine, "VERDICT: WARNING"), strings.HasPrefix(firstLine, "WARNING"):
		verdict = "WARNING"
	}

	rationale := "No rationale provided"
	if len(lines) > 1 {
		rationale = strings.Join(lines[1:], " ")
		rationale = strings.TrimSpace(rationale)
	}

	return CheckResult{
		Agent:     agentType,
		Verdict:   verdict,
		Rationale: rationale,
	}
}

// MergeActivity merges the PR via GitHub MCP.
//
// When args.HeadSHA is non-empty it is forwarded to the MCP server as the
// expected commit SHA. The MCP server is responsible for refusing the merge
// if the PR's current head no longer matches.
func MergeActivity(ctx context.Context, args MergeArgs) error {
	allowedMergeMethods := map[string]bool{
		"merge":        true,
		"squash":       true,
		"rebase":       true,
		"rebase-merge": true,
	}
	if args.MergeMethod != "" && !allowedMergeMethods[args.MergeMethod] {
		return fmt.Errorf("invalid merge method: %s", args.MergeMethod)
	}
	mergeMethod := args.MergeMethod
	if mergeMethod == "" {
		mergeMethod = "squash"
	}

	params := map[string]interface{}{
		"owner":       args.RepoOwner,
		"repo":        args.RepoName,
		"prNumber":    args.PRNumber,
		"mergeMethod": mergeMethod,
	}
	if args.HeadSHA != "" {
		params["headSHA"] = args.HeadSHA
	}
	payload := map[string]interface{}{
		"method": "mergePullRequest",
		"params": params,
	}

	// SetDoNotParseResponse(true) forces resty to hand us the raw body so we
	// can bound it via io.LimitReader. Without this, resty has already
	// buffered the entire upstream response in RAM before our size check
	// can fire — a hostile MCP server could OOM the worker by streaming
	// gigabytes back in response to a merge call.
	req := httpClient.R().
		SetContext(ctx).
		SetDoNotParseResponse(true).
		SetHeader("Content-Type", "application/json").
		SetBody(payload)
	setAuth(req, GitHubToken)

	recordHeartbeatIfActivity(ctx, "calling mcp merge")
	// Merge calls can be slow if the upstream is contended; pump heartbeats
	// so HeartbeatTimeout doesn't kill the activity mid-merge.
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()
	resp, err := req.Post(MCPServerURL)

	if err != nil {
		return fmt.Errorf("failed to call MCP server for merge: %w", err)
	}

	body, overflow, readErr := readLimitedBody(resp.RawBody(), maxCommentRespBytes)
	if readErr != nil {
		return fmt.Errorf("failed to read MCP merge response body: %w", readErr)
	}
	if overflow {
		return fmt.Errorf("MCP merge response too large (max %d bytes)", maxCommentRespBytes)
	}

	if resp.StatusCode() >= 400 {
		// MCP returns text, not typed sentinels; the substring detection
		// inside wrapMutatingActivityErr is intentional here — this is the
		// boundary that converts MCP's ad-hoc error strings into typed
		// Temporal ApplicationError reasons that workflows rely on.
		statusErr := fmt.Errorf("MCP merge returned status %d: %s", resp.StatusCode(), sanitizeRespBody(string(body)))
		return wrapMutatingActivityErr(statusErr)
	}

	var response struct {
		Error interface{} `json:"error"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("failed to parse MCP merge response: %w", err)
	}

	if response.Error != nil {
		// Same boundary conversion as the status>=400 path: an MCP server
		// reporting "pull request is already merged" in its JSON `error`
		// field needs to surface as a typed `already_merged` reason for the
		// workflow's `errors.As` switch.
		mcpErr := fmt.Errorf("MCP merge error: %v", response.Error)
		return wrapMutatingActivityErr(mcpErr)
	}

	return nil
}

// GiteaMergeActivity merges a PR via the configured Gitea API.
//
// When args.HeadSHA is non-empty the merge is pinned to that commit via
// MergePullRequestWithOptions, so a force-push during review surfaces as
// gitea.ErrAlreadyMerged or gitea.ErrHeadSHAMismatch and is handled at
// the workflow layer.
func GiteaMergeActivity(ctx context.Context, args MergeArgs) error {
	allowedMergeMethods := map[string]bool{
		"merge":        true,
		"squash":       true,
		"rebase":       true,
		"rebase-merge": true,
	}
	if args.MergeMethod != "" && !allowedMergeMethods[args.MergeMethod] {
		return fmt.Errorf("invalid merge method: %s", args.MergeMethod)
	}
	mergeMethod := args.MergeMethod
	if mergeMethod == "" {
		mergeMethod = "squash"
	}
	client, err := getGiteaClient()
	if err != nil {
		return err
	}
	recordHeartbeatIfActivity(ctx, "calling gitea merge")
	// Gitea merges can be slow under contention; pump heartbeats so a
	// HeartbeatTimeout shorter than the call duration doesn't surprise-kill
	// the activity mid-merge.
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()
	var mergeErr error
	if args.HeadSHA != "" {
		mergeErr = client.MergePullRequestWithOptions(
			ctx,
			args.RepoOwner,
			args.RepoName,
			int64(args.PRNumber),
			mergeMethod,
			gitea.MergeOptions{HeadSHA: args.HeadSHA},
		)
	} else {
		mergeErr = client.MergePullRequest(ctx, args.RepoOwner, args.RepoName, int64(args.PRNumber), mergeMethod)
	}
	// Convert typed gitea sentinels (and "PR closed"-shaped errors) into
	// *temporal.ApplicationError with the cross-stream reason strings the
	// workflow layer matches on. See wrapMutatingActivityErr for the mapping.
	return wrapMutatingActivityErr(mergeErr)
}

// buildEnhancedComment creates a detailed comment with metrics for human review
func buildEnhancedComment(failingReasons []string, metrics *AnalysisMetrics) string {
	commentBody := "## 🤖 AI Code Review Results\n\n"
	commentBody += "Some automated agents flagged issues that require human attention:\n\n"

	// Add failing reasons
	for _, reason := range failingReasons {
		commentBody += "❌ " + reason + "\n"
	}

	if metrics != nil {
		commentBody += "\n## 📊 Change Analysis\n\n"

		// Basic metrics
		commentBody += fmt.Sprintf("- **Lines Changed**: +%d/-%d\n", metrics.AddedLines, metrics.RemovedLines)
		commentBody += fmt.Sprintf("- **Files Modified**: %d\n", len(metrics.AffectedFiles))
		commentBody += fmt.Sprintf("- **Functions Modified**: %d\n", len(metrics.AffectedFunctions))
		commentBody += fmt.Sprintf("- **Complexity Score**: %d/4\n\n", metrics.ComplexityScore)

		// Languages
		if len(metrics.Languages) > 0 {
			commentBody += "**Languages Detected:**\n"
			for lang, files := range metrics.Languages {
				commentBody += fmt.Sprintf("- %s: %d files\n", lang, len(files))
			}
			commentBody += "\n"
		}

		// Critical files warning
		if len(metrics.CriticalFiles) > 0 {
			commentBody += "⚠️ **Critical Files Modified:**\n"
			for _, file := range metrics.CriticalFiles {
				commentBody += fmt.Sprintf("- `%s`\n", file)
			}
			commentBody += "\n"
		}

		// Test files info
		if len(metrics.TestFiles) > 0 {
			commentBody += "🧪 **Test Files Modified:**\n"
			for _, file := range metrics.TestFiles {
				commentBody += fmt.Sprintf("- `%s`\n", file)
			}
			commentBody += "\n"
		}

		// Function list
		if len(metrics.AffectedFunctions) > 0 {
			commentBody += "**Functions Modified:**\n"
			for _, fn := range metrics.AffectedFunctions {
				commentBody += fmt.Sprintf("- `%s()`\n", fn)
			}
			commentBody += "\n"
		}
	}

	commentBody += "---\n\n"
	commentBody += "Please review the flagged issues manually. Once addressed, you can merge this PR or request the AI review to run again."

	return commentBody
}

// buildSecurityPrompt creates a context-aware security analysis prompt.
//
// The diff is wrapped in a per-call random sentinel rather than a fixed
// `<diff>` tag so that diff content containing the literal sentinel string
// (or `</diff>`) cannot escape the boundary and inject instructions.
func buildSecurityPrompt(diff string, metrics *AnalysisMetrics) string {
	languageContext := ""
	if len(metrics.Languages) > 0 {
		languageContext = "\nLanguages detected: "
		for lang := range metrics.Languages {
			languageContext += lang + " "
		}
	}

	criticalContext := ""
	if len(metrics.CriticalFiles) > 0 {
		criticalContext = fmt.Sprintf("\nCRITICAL: This change affects critical system files: %v", metrics.CriticalFiles)
	}

	complexityContext := fmt.Sprintf("\nComplexity Score: %d/4 (Lines: +%d/-%d, Files: %d, Functions: %d)",
		metrics.ComplexityScore, metrics.AddedLines, metrics.RemovedLines,
		len(metrics.AffectedFiles), len(metrics.AffectedFunctions))

	sentinel := "DIFF_BOUNDARY_" + randomShortHex(6)
	return fmt.Sprintf(`[SECURITY AGENT]
Review the following diff for security vulnerabilities including SQL injection, XSS, buffer overflows, authentication bypasses, and privilege escalation.
%s%s%s

Focus on:
- Input validation and sanitization
- Authentication and authorization changes
- Cryptographic implementations
- File system operations
- Network communication security

Respond with "PASS" or "FAIL" and a detailed rationale.

---BEGIN %s---
%s
---END %s---`,
		languageContext, criticalContext, complexityContext, sentinel, diff, sentinel)
}

// buildPerformancePrompt creates a context-aware performance analysis prompt
func buildPerformancePrompt(diff string, metrics *AnalysisMetrics) string {
	languageContext := ""
	if len(metrics.Languages) > 0 {
		languageContext = "\nLanguages detected: "
		for lang := range metrics.Languages {
			languageContext += lang + " "
		}
	}

	functionContext := ""
	if len(metrics.AffectedFunctions) > 0 {
		functionContext = fmt.Sprintf("\nFunctions modified: %v", metrics.AffectedFunctions)
	}

	complexityContext := fmt.Sprintf("\nComplexity Score: %d/4 (Lines: +%d/-%d, Files: %d)",
		metrics.ComplexityScore, metrics.AddedLines, metrics.RemovedLines, len(metrics.AffectedFiles))

	sentinel := "DIFF_BOUNDARY_" + randomShortHex(6)
	return fmt.Sprintf(`[PERFORMANCE AGENT]
Analyze this diff for performance issues and anti-patterns.
%s%s%s

Focus on:
- Algorithmic complexity (O(n²) loops, nested iterations)
- Memory allocation patterns
- Database query efficiency
- Caching opportunities
- Resource management
- Synchronization overhead

Respond with "PASS" or "FAIL" and a detailed rationale.

---BEGIN %s---
%s
---END %s---`,
		languageContext, functionContext, complexityContext, sentinel, diff, sentinel)
}

// buildConstPrompt creates a context-aware const correctness prompt
func buildConstPrompt(diff string, metrics *AnalysisMetrics) string {
	languageContext := ""
	if len(metrics.Languages) > 0 {
		languageContext = "\nLanguages detected: "
		for lang := range metrics.Languages {
			languageContext += lang + " "
		}
	}

	functionContext := ""
	if len(metrics.AffectedFunctions) > 0 {
		functionContext = fmt.Sprintf("\nFunctions to analyze: %v", metrics.AffectedFunctions)
	}

	sentinel := "DIFF_BOUNDARY_" + randomShortHex(6)
	return fmt.Sprintf(`[CONST CORRECTNESS AGENT]
Analyze const usage and immutability patterns in this diff.
%s%s

Focus on:
- Proper const qualifiers in C/C++
- Immutable data structures
- Read-only function parameters
- Const member functions
- Final/readonly keywords in other languages
- Functional programming patterns

Respond with "PASS" or "FAIL" and a detailed rationale.

---BEGIN %s---
%s
---END %s---`,
		languageContext, functionContext, sentinel, diff, sentinel)
}

// CommentForHumanReviewActivity adds a comment requesting human review with enhanced metrics
func CommentForHumanReviewActivity(ctx context.Context, args CommentArgs) error {
	commentBody := buildEnhancedComment(args.FailingReasons, args.Metrics)

	payload := map[string]interface{}{
		"method": "createComment",
		"params": map[string]interface{}{
			"owner":       args.RepoOwner,
			"repo":        args.RepoName,
			"issueNumber": args.PRNumber,
			"body":        commentBody,
		},
	}

	// Same body-size protection as FetchDiff: bound the response so a hostile
	// MCP server cannot OOM the worker by streaming gigabytes back.
	req := httpClient.R().
		SetContext(ctx).
		SetDoNotParseResponse(true).
		SetHeader("Content-Type", "application/json").
		SetBody(payload)
	setAuth(req, GitHubToken)

	recordHeartbeatIfActivity(ctx, "calling mcp comment")
	// Same heartbeat-pump pattern as FetchDiff/Merge: a single up-front
	// heartbeat is not enough for slow Gitea/MCP comment posts, and the
	// REQUEST_REVIEW retry policy is MaxAttempts=1. Without the pump,
	// HeartbeatTimeout=30s plus a 60s upstream call would silently kill the
	// activity with no retry.
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()
	resp, err := req.Post(MCPServerURL)

	if err != nil {
		return fmt.Errorf("failed to call MCP server for comment: %w", err)
	}

	body, overflow, readErr := readLimitedBody(resp.RawBody(), maxCommentRespBytes)
	if readErr != nil {
		return fmt.Errorf("failed to read MCP comment response body: %w", readErr)
	}
	if overflow {
		return fmt.Errorf("MCP comment response too large (max %d bytes)", maxCommentRespBytes)
	}

	if resp.StatusCode() >= 400 {
		// A "PR closed" reply on a comment post must surface as a typed
		// pr_closed ApplicationError so the workflow can degrade gracefully
		// instead of failing the whole REQUEST_REVIEW path.
		statusErr := fmt.Errorf("MCP comment returned status %d: %s", resp.StatusCode(), sanitizeRespBody(string(body)))
		return wrapMutatingActivityErr(statusErr)
	}

	var response struct {
		Error interface{} `json:"error"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("failed to parse MCP comment response: %w", err)
	}

	if response.Error != nil {
		mcpErr := fmt.Errorf("MCP comment error: %v", response.Error)
		return wrapMutatingActivityErr(mcpErr)
	}

	return nil
}

// GiteaCommentForHumanReviewActivity adds a review comment via Gitea.
func GiteaCommentForHumanReviewActivity(ctx context.Context, args CommentArgs) error {
	client, err := getGiteaClient()
	if err != nil {
		return err
	}
	commentBody := buildEnhancedComment(args.FailingReasons, args.Metrics)
	recordHeartbeatIfActivity(ctx, "calling gitea comment")
	// Slow Gitea servers can stretch a single CreateComment past
	// HeartbeatTimeout (30s in workflow options); pump heartbeats throughout
	// so Temporal does not surprise-kill an in-flight comment with the
	// REQUEST_REVIEW retry policy at MaxAttempts=1.
	stopHB := startHeartbeatPump(ctx, 10*time.Second)
	defer stopHB()
	_, err = client.CreateComment(ctx, args.RepoOwner, args.RepoName, int64(args.PRNumber), commentBody)
	// PR-closed during a request-review comment post must surface as a typed
	// pr_closed reason so the workflow can drop the comment and proceed
	// instead of failing the whole review.
	return wrapMutatingActivityErr(err)
}

// Cached Gitea client. The previous implementation re-loaded viper config from
// disk on every activity invocation, which is both expensive and racy. Build
// it once and reuse it; if construction fails the error is returned on every
// subsequent call so the caller can retry by restarting the worker.
var (
	giteaOnce   sync.Once
	giteaClient *gitea.GiteaClient
	giteaErr    error
)

func getGiteaClient() (*gitea.GiteaClient, error) {
	giteaOnce.Do(func() {
		// SetGiteaConfig wins if it was called before any activity ran.
		// Otherwise we fall back to the viper-loaded path so legacy
		// deployments keep working unchanged.
		if giteaConfigOverride != nil {
			gcfg := *giteaConfigOverride
			if gcfg.BaseURL == "" || gcfg.Token == "" {
				giteaErr = fmt.Errorf("gitea base URL and token are required")
				return
			}
			giteaClient = gitea.NewGiteaClient(gcfg, logrus.New())
			return
		}

		cfg, err := config.LoadConfig("")
		if err != nil {
			giteaErr = fmt.Errorf("load gitea config: %w", err)
			return
		}
		if cfg.Gitea.BaseURL == "" || cfg.Gitea.Token == "" {
			giteaErr = fmt.Errorf("gitea base URL and token are required")
			return
		}
		giteaClient = gitea.NewGiteaClient(cfg.Gitea, logrus.New())
	})
	return giteaClient, giteaErr
}

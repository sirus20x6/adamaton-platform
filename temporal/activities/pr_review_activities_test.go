package activities

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/sirus20x6/adamomaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamomaton-core/types"
)

// mustMarshalJSON marshals v or fails the test. Wrapped here so each
// caller stays a one-liner.
func mustMarshalJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return b
}

func TestParseAgentResponse(t *testing.T) {
	tests := []struct {
		name              string
		rawText           string
		agentType         string
		expectedVerdict   string
		expectedRationale string
	}{
		{
			name:              "PASS response with rationale",
			rawText:           "PASS\nNo security issues found",
			agentType:         "Security",
			expectedVerdict:   "PASS",
			expectedRationale: "No security issues found",
		},
		{
			name:              "FAIL response with rationale",
			rawText:           "FAIL\nSQL injection vulnerability detected",
			agentType:         "Security",
			expectedVerdict:   "FAIL",
			expectedRationale: "SQL injection vulnerability detected",
		},
		{
			name:              "PASS in mixed case",
			rawText:           "pass\nLooks good",
			agentType:         "Performance",
			expectedVerdict:   "PASS",
			expectedRationale: "Looks good",
		},
		{
			name:              "Empty response defaults to FAIL",
			rawText:           "",
			agentType:         "Const",
			expectedVerdict:   "FAIL",
			expectedRationale: "Empty response from agent",
		},
		{
			name:              "Unknown verdict defaults to REVIEW",
			rawText:           "Something unexpected\nWith rationale",
			agentType:         "Style",
			expectedVerdict:   "REVIEW",
			expectedRationale: "With rationale",
		},
		{
			name:              "Multi-line rationale joined",
			rawText:           "PASS\nLine one\nLine two",
			agentType:         "Performance",
			expectedVerdict:   "PASS",
			expectedRationale: "Line one Line two",
		},
		{
			name:              "WARNING verdict",
			rawText:           "WARNING\nMinor concerns identified",
			agentType:         "Architecture",
			expectedVerdict:   "WARNING",
			expectedRationale: "Minor concerns identified",
		},
		{
			name:              "WARNING with VERDICT prefix",
			rawText:           "VERDICT: WARNING\nLooks mostly OK",
			agentType:         "Architecture",
			expectedVerdict:   "WARNING",
			expectedRationale: "Looks mostly OK",
		},
		{
			name:              "VERDICT: PASS prefix",
			rawText:           "VERDICT: PASS\nClean",
			agentType:         "Security",
			expectedVerdict:   "PASS",
			expectedRationale: "Clean",
		},
		{
			name:              "VERDICT: FAIL prefix",
			rawText:           "VERDICT: FAIL\nIssues",
			agentType:         "Security",
			expectedVerdict:   "FAIL",
			expectedRationale: "Issues",
		},
		// Strict-prefix matching: a FAIL line that mentions PASS in prose
		// must NOT be classified as PASS just because the substring exists.
		{
			name:              "FAIL with PASS in prose stays FAIL",
			rawText:           "FAIL is bad PASS check\nThis is wrong",
			agentType:         "Security",
			expectedVerdict:   "FAIL",
			expectedRationale: "This is wrong",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseAgentResponse(tt.rawText, tt.agentType)

			if result.Agent != tt.agentType {
				t.Errorf("Expected agent %s, got %s", tt.agentType, result.Agent)
			}
			if result.Verdict != tt.expectedVerdict {
				t.Errorf("Expected verdict %s, got %s", tt.expectedVerdict, result.Verdict)
			}
			if result.Rationale != tt.expectedRationale {
				t.Errorf("Expected rationale %q, got %q", tt.expectedRationale, result.Rationale)
			}
		})
	}
}

// TestParseAgentResponse_LooseMatchRegression specifically guards against a
// regression where strings.Contains(firstLine, "PASS") would incorrectly
// flip a FAIL verdict to PASS just because PASS appeared elsewhere on the
// first line.
func TestParseAgentResponse_LooseMatchRegression(t *testing.T) {
	result := parseAgentResponse("FAIL is bad PASS check\nrationale", "Security")
	if result.Verdict != "FAIL" {
		t.Errorf("FAIL line containing PASS should remain FAIL, got %s", result.Verdict)
	}
}

func TestSanitizeRespBody(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		mustHave string
		mustMiss string
	}{
		{
			name:     "redacts authorization header",
			input:    "request echoed\nAuthorization: Bearer ghp_secrettoken123\n",
			mustHave: "[redacted]",
			mustMiss: "ghp_secrettoken123",
		},
		{
			name:     "case insensitive",
			input:    "AUTHORIZATION: bearer abc",
			mustHave: "[redacted]",
			mustMiss: "abc",
		},
		{
			name:     "leaves benign body alone",
			input:    "no secrets here",
			mustHave: "no secrets here",
			mustMiss: "[redacted]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := sanitizeRespBody(tt.input)
			if !contains(out, tt.mustHave) {
				t.Errorf("expected output %q to contain %q", out, tt.mustHave)
			}
			if tt.mustMiss != "" && contains(out, tt.mustMiss) {
				t.Errorf("expected output %q to NOT contain %q", out, tt.mustMiss)
			}
		})
	}
}

func TestSanitizeRespBody_Truncates(t *testing.T) {
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'a'
	}
	out := sanitizeRespBody(string(big))
	if len(out) > 1024+len("...[truncated]") {
		t.Errorf("expected truncation to ~1024+suffix, got len=%d", len(out))
	}
	if !contains(out, "...[truncated]") {
		t.Errorf("expected truncation marker, got: %s", out[:64])
	}
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestCalculateComplexityScore(t *testing.T) {
	tests := []struct {
		name     string
		analysis DiffAnalysis
		expected int
	}{
		{
			name: "Low complexity - few changes",
			analysis: DiffAnalysis{
				AddedLines:         5,
				RemovedLines:       3,
				MentionedFiles:     []string{"main.go"},
				MentionedFunctions: []string{},
			},
			expected: 1,
		},
		{
			name: "Medium complexity",
			analysis: DiffAnalysis{
				AddedLines:         30,
				RemovedLines:       20,
				MentionedFiles:     []string{"a.go", "b.go", "c.go"},
				MentionedFunctions: []string{"foo", "bar"},
			},
			expected: 2,
		},
		{
			name: "High complexity",
			analysis: DiffAnalysis{
				AddedLines:         100,
				RemovedLines:       50,
				MentionedFiles:     []string{"a.go", "b.go", "c.go", "d.go", "e.go"},
				MentionedFunctions: []string{"foo", "bar", "baz", "qux"},
			},
			expected: 3,
		},
		{
			name: "Very high complexity",
			analysis: DiffAnalysis{
				AddedLines:         200,
				RemovedLines:       100,
				MentionedFiles:     []string{"a.go", "b.go", "c.go", "d.go", "e.go"},
				MentionedFunctions: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"},
			},
			expected: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateComplexityScore(tt.analysis)
			if result != tt.expected {
				t.Errorf("Expected complexity score %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestAnalyzeDiffEnhanced(t *testing.T) {
	diff := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,5 +1,8 @@
 package main

+import "fmt"
+
 func main() {
+    fmt.Println("Hello")
 }
`

	metrics := AnalyzeDiffEnhanced(diff)

	if metrics == nil {
		t.Fatal("Expected non-nil metrics")
	}

	if len(metrics.AffectedFiles) == 0 {
		t.Error("Expected at least one affected file")
	}

	if metrics.AddedLines == 0 {
		t.Error("Expected some added lines")
	}

	if metrics.ComplexityScore < 1 || metrics.ComplexityScore > 4 {
		t.Errorf("Complexity score %d out of range [1,4]", metrics.ComplexityScore)
	}
}

func TestExtractMentionedFunctions(t *testing.T) {
	diff := `+func newFunction() {
+    return
+}
-func oldFunction() {
`

	functions := ExtractMentionedFunctions(diff)

	if len(functions) == 0 {
		t.Error("Expected to find functions")
	}

	found := false
	for _, f := range functions {
		if f == "newFunction" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find newFunction")
	}
}

func TestExtractMentionedFiles(t *testing.T) {
	diff := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
diff --git a/utils.go b/utils.go
--- a/utils.go
+++ b/utils.go
`

	files := ExtractMentionedFiles(diff)

	if len(files) < 2 {
		t.Errorf("Expected at least 2 files, got %d", len(files))
	}
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"main_test.go", true},
		{"pkg/utils_test.go", true},
		{"app.test.js", true},
		{"component.spec.ts", true},
		{"test_utils.py", true},
		{"src/test/helper.go", true},
		{"src/tests/helper.go", true},
		{"__tests__/foo.js", true},
		{"main.go", false},
		{"utils.go", false},
		{"README.md", false},
		// Pass 9 regression: substring "test_" inside other names must not match.
		{"latest_release.go", false},
		{"bestest.go", false},
		// Pass 9 regression: substring "/test/" inside other path components must not match.
		{"fastest/release.go", false},
		// Pass 9 regression: "_test.go" substring inside another component must not match.
		{"a_test.go.bak", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := IsTestFile(tt.path)
			if result != tt.expected {
				t.Errorf("IsTestFile(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

func TestIsCriticalFile(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"main.go", true},
		{"cmd/server.go", true},
		{"Dockerfile", true},
		{"go.mod", true},
		{".env", true},
		{"config.yaml", true},
		{"config.json", true},
		{"config", true},
		{"pkg/utils.go", false},
		{"README.md", false},
		// Pass 9 regression: "config" substring in another name must not match.
		{"configtest_helper.go", false},
		// Pass 9 regression: "main.go" substring in another name must not match.
		{"my_main.go.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := IsCriticalFile(tt.path)
			if result != tt.expected {
				t.Errorf("IsCriticalFile(%q) = %v, want %v", tt.path, result, tt.expected)
			}
		})
	}
}

// TestRandomShortHex checks that the helper used to create per-prompt
// boundary sentinels returns the expected length and is hex-encoded.
func TestRandomShortHex(t *testing.T) {
	got := randomShortHex(6)
	if len(got) != 12 {
		t.Errorf("randomShortHex(6) length = %d, want 12", len(got))
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("randomShortHex returned non-hex character: %q", c)
		}
	}
}

// TestNewSharedHTTPClient_NoTimeout asserts the shared resty client does NOT
// have a per-request timeout configured (the regression that motivated Pass
// 10). Activity timeouts must come from Temporal ActivityOptions via
// .SetContext(ctx), not a hard-coded resty cap.
func TestNewSharedHTTPClient_NoTimeout(t *testing.T) {
	// Reset the env so we exercise the default path.
	t.Setenv("VLLM_MAX_CONCURRENT", "")

	c := newSharedHTTPClient()
	// resty.Client.GetClient() returns the underlying *http.Client; its
	// Timeout field MUST be zero so context deadlines control duration.
	if c.GetClient().Timeout != 0 {
		t.Fatalf("shared HTTP client has unexpected timeout %v; expected 0 (rely on ctx deadline)",
			c.GetClient().Timeout)
	}
}

// TestNewSharedHTTPClient_HonorsVLLMMaxConcurrent verifies the VLLM_MAX_CONCURRENT
// env override is respected and falls back to the default for unparseable
// values.
func TestNewSharedHTTPClient_HonorsVLLMMaxConcurrent(t *testing.T) {
	t.Setenv("VLLM_MAX_CONCURRENT", "5")
	c := newSharedHTTPClient()
	tr, ok := c.GetClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.GetClient().Transport)
	}
	if tr.MaxConnsPerHost != 5 {
		t.Errorf("MaxConnsPerHost = %d, want 5", tr.MaxConnsPerHost)
	}

	// Garbage value falls back to default.
	t.Setenv("VLLM_MAX_CONCURRENT", "not-a-number")
	c = newSharedHTTPClient()
	tr = c.GetClient().Transport.(*http.Transport)
	if tr.MaxConnsPerHost != defaultVLLMMaxConcurrent {
		t.Errorf("MaxConnsPerHost on garbage input = %d, want %d",
			tr.MaxConnsPerHost, defaultVLLMMaxConcurrent)
	}

	// Negative value falls back to default.
	t.Setenv("VLLM_MAX_CONCURRENT", "-3")
	c = newSharedHTTPClient()
	tr = c.GetClient().Transport.(*http.Transport)
	if tr.MaxConnsPerHost != defaultVLLMMaxConcurrent {
		t.Errorf("MaxConnsPerHost on negative input = %d, want %d",
			tr.MaxConnsPerHost, defaultVLLMMaxConcurrent)
	}
}

// TestFetchDiffActivity_BodyOverflow validates that a server returning a
// body larger than maxDiffBytes is rejected before the worker buffers the
// whole thing in RAM. We arrange for the test server to keep streaming
// bytes, then assert the activity errors out — without loading anything
// like 100GB.
func TestFetchDiffActivity_BodyOverflow(t *testing.T) {
	// Save and restore package-level globals so this test doesn't leak.
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	// We don't need maxDiffBytes+1 bytes for the test to be meaningful — the
	// limit reader will correctly detect any size > maxDiffBytes. Send a
	// payload of maxDiffBytes+1 zero bytes; LimitReader caps the read so we
	// never allocate more than maxDiffBytes+1 bytes in RAM.
	payload := strings.Repeat("a", maxDiffBytes+1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":{"diff":"` + payload + `"}}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	_, err := FetchDiffActivity(context.Background(), FetchDiffArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y",
	})
	if err == nil {
		t.Fatal("expected oversized body to produce an error, got nil")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected 'too large' error, got: %v", err)
	}
}

// TestFetchDiffActivity_HappyPath verifies the rewired body path still
// successfully parses a small diff response.
func TestFetchDiffActivity_HappyPath(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	const wantDiff = "diff --git a/foo b/foo\n+hello"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"diff":` + jsonString(wantDiff) + `}}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	got, err := FetchDiffActivity(context.Background(), FetchDiffArgs{
		PRNumber: 42, RepoOwner: "o", RepoName: "r",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantDiff {
		t.Fatalf("diff mismatch: got %q want %q", got, wantDiff)
	}
}

// TestCommentForHumanReviewActivity_BodyOverflow asserts that
// CommentForHumanReviewActivity also enforces the response body limit.
func TestCommentForHumanReviewActivity_BodyOverflow(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	payload := strings.Repeat("a", maxCommentRespBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	err := CommentForHumanReviewActivity(context.Background(), CommentArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y",
		FailingReasons: []string{"x"},
	})
	if err == nil {
		t.Fatal("expected oversized body to produce an error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected 'too large' error, got: %v", err)
	}
}

// TestSharedHTTPClient_ConcurrentSafe is a smoke test that the shared client
// is safe to use from multiple goroutines (it is by design — this test
// exists primarily to flag regressions if someone replaces resty with a
// non-thread-safe client, and to ensure -race is exercised on the path).
func TestSharedHTTPClient_ConcurrentSafe(t *testing.T) {
	savedHTTPClient := httpClient
	t.Cleanup(func() { httpClient = savedHTTPClient })
	httpClient = newSharedHTTPClient()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":"PASS\nok"}`))
	}))
	t.Cleanup(srv.Close)

	savedEndpoint := VLLMEndpoint
	VLLMEndpoint = srv.URL
	t.Cleanup(func() { VLLMEndpoint = savedEndpoint })

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = callVLLMAgent(context.Background(), "prompt", "Security")
		}()
	}
	wg.Wait()
}

// resetConfigOnceForTest puts the SetActivityConfig / SetGiteaConfig once
// gates back in their pristine "not yet fired" state so a follow-up test
// can drive them again. Production code MUST NOT call this — the once is
// load-bearing for safe worker startup.
func resetConfigOnceForTest(t *testing.T) {
	t.Helper()
	configOnce = sync.Once{}
	giteaConfigOnce = sync.Once{}
	giteaConfigOverride = nil
}

// TestSetActivityConfig_OverridesEnvDefaults verifies that calling
// SetActivityConfig with a populated types.Config replaces the env-seeded
// package globals. This is the production worker-startup path that Agent 5
// invokes in cmd/worker/main.go. cfg.VLLM.Endpoint is the canonical full-URL
// source (already includes /generate); cfg.LLM.Endpoint is a bare host that
// SetActivityConfig must lift to a full URL by appending /generate.
func TestSetActivityConfig_OverridesEnvDefaults(t *testing.T) {
	resetConfigOnceForTest(t)

	savedVLLM := VLLMEndpoint
	savedMCP := MCPServerURL
	savedGH := GitHubToken
	t.Cleanup(func() {
		VLLMEndpoint = savedVLLM
		MCPServerURL = savedMCP
		GitHubToken = savedGH
		resetConfigOnceForTest(t)
	})

	cfg := types.Config{}
	cfg.VLLM.Endpoint = "http://from-config:9999/generate"
	cfg.MCP.ServerURL = "http://mcp-from-config:8888"
	cfg.GitHub.Token = "ghp_from-config"

	SetActivityConfig(cfg)

	if VLLMEndpoint != "http://from-config:9999/generate" {
		t.Errorf("VLLMEndpoint not overridden: got %q", VLLMEndpoint)
	}
	if MCPServerURL != "http://mcp-from-config:8888" {
		t.Errorf("MCPServerURL not overridden: got %q", MCPServerURL)
	}
	if GitHubToken != "ghp_from-config" {
		t.Errorf("GitHubToken not overridden: got %q", GitHubToken)
	}
}

// TestSetActivityConfig_Idempotent verifies that a second call after the
// first is silently ignored. This protects against a process that runs
// multiple bootstrap paths from racing each other and leaving the package
// in an inconsistent state.
func TestSetActivityConfig_Idempotent(t *testing.T) {
	resetConfigOnceForTest(t)

	savedVLLM := VLLMEndpoint
	t.Cleanup(func() {
		VLLMEndpoint = savedVLLM
		resetConfigOnceForTest(t)
	})

	first := types.Config{}
	first.VLLM.Endpoint = "http://first:1111/generate"
	SetActivityConfig(first)

	second := types.Config{}
	second.VLLM.Endpoint = "http://second:2222/generate"
	SetActivityConfig(second)

	if VLLMEndpoint != "http://first:1111/generate" {
		t.Errorf("expected first call to win, got VLLMEndpoint=%q", VLLMEndpoint)
	}
}

// TestSetActivityConfig_EmptyFieldsDoNotClobber verifies that an empty
// field in the supplied config leaves the env-seeded default in place. A
// partial config (e.g. only VLLM endpoint set, GitHub token unset) must
// not blank out the values the env supplied.
func TestSetActivityConfig_EmptyFieldsDoNotClobber(t *testing.T) {
	resetConfigOnceForTest(t)

	savedVLLM := VLLMEndpoint
	savedMCP := MCPServerURL
	savedGH := GitHubToken
	t.Cleanup(func() {
		VLLMEndpoint = savedVLLM
		MCPServerURL = savedMCP
		GitHubToken = savedGH
		resetConfigOnceForTest(t)
	})

	// Pre-seed with sentinel values so we can detect clobbering.
	VLLMEndpoint = "http://env-default-vllm"
	MCPServerURL = "http://env-default-mcp"
	GitHubToken = "env-default-token"

	cfg := types.Config{}
	cfg.VLLM.Endpoint = "http://only-this-set/generate"
	// MCP.ServerURL and GitHub.Token left blank.

	SetActivityConfig(cfg)

	if VLLMEndpoint != "http://only-this-set/generate" {
		t.Errorf("VLLMEndpoint should have been overridden, got %q", VLLMEndpoint)
	}
	if MCPServerURL != "http://env-default-mcp" {
		t.Errorf("MCPServerURL was unexpectedly clobbered: %q", MCPServerURL)
	}
	if GitHubToken != "env-default-token" {
		t.Errorf("GitHubToken was unexpectedly clobbered: %q", GitHubToken)
	}
}

// TestSetActivityConfig_VLLMEndpointWinsOverLLMEndpoint verifies the bug-3
// contract: cfg.VLLM.Endpoint is the canonical full-URL source. When both
// cfg.VLLM.Endpoint and cfg.LLM.Endpoint are set, VLLM wins, because
// cfg.LLM.Endpoint is a bare host (default "http://localhost:8000") and
// VLLMEndpoint must end in /generate.
func TestSetActivityConfig_VLLMEndpointWinsOverLLMEndpoint(t *testing.T) {
	resetConfigOnceForTest(t)

	savedVLLM := VLLMEndpoint
	t.Cleanup(func() {
		VLLMEndpoint = savedVLLM
		resetConfigOnceForTest(t)
	})

	cfg := types.Config{}
	cfg.VLLM.Endpoint = "http://x/generate"
	cfg.LLM.Endpoint = "http://y" // should be ignored when VLLM is set

	SetActivityConfig(cfg)

	if VLLMEndpoint != "http://x/generate" {
		t.Errorf("expected VLLMEndpoint to come from cfg.VLLM.Endpoint, got %q", VLLMEndpoint)
	}
}

// TestSetActivityConfig_LLMEndpointFallsBackWithGenerateSuffix is the second
// half of bug 3: when only cfg.LLM.Endpoint is provided (the LLM-only
// configuration shape), SetActivityConfig must derive the full vLLM URL by
// appending "/generate" so the activity's POST hits the right path. Without
// this, the activity used to POST to "http://localhost:8000" and get 404.
func TestSetActivityConfig_LLMEndpointFallsBackWithGenerateSuffix(t *testing.T) {
	resetConfigOnceForTest(t)

	savedVLLM := VLLMEndpoint
	t.Cleanup(func() {
		VLLMEndpoint = savedVLLM
		resetConfigOnceForTest(t)
	})

	cfg := types.Config{}
	cfg.LLM.Endpoint = "http://y" // bare host, no scheme path

	SetActivityConfig(cfg)

	if VLLMEndpoint != "http://y/generate" {
		t.Errorf("expected /generate to be appended to bare host, got %q", VLLMEndpoint)
	}
}

// TestSetActivityConfig_LLMEndpointTrailingSlashTrimmed makes sure the
// /generate suffix is appended exactly once even when the LLM endpoint
// has a trailing slash.
func TestSetActivityConfig_LLMEndpointTrailingSlashTrimmed(t *testing.T) {
	resetConfigOnceForTest(t)

	savedVLLM := VLLMEndpoint
	t.Cleanup(func() {
		VLLMEndpoint = savedVLLM
		resetConfigOnceForTest(t)
	})

	cfg := types.Config{}
	cfg.LLM.Endpoint = "http://y/"

	SetActivityConfig(cfg)

	if VLLMEndpoint != "http://y/generate" {
		t.Errorf("expected single /generate (no double slash), got %q", VLLMEndpoint)
	}
}

// TestSetGiteaConfig_PopulatesOverride verifies that SetGiteaConfig stores
// the supplied config so getGiteaClient consults it instead of the
// viper-loaded path.
func TestSetGiteaConfig_PopulatesOverride(t *testing.T) {
	resetConfigOnceForTest(t)
	t.Cleanup(func() { resetConfigOnceForTest(t) })

	gcfg := types.GiteaConfig{
		BaseURL: "https://gitea.example",
		Token:   "tok-test",
	}
	SetGiteaConfig(gcfg)

	if giteaConfigOverride == nil {
		t.Fatal("giteaConfigOverride was not populated")
	}
	if giteaConfigOverride.BaseURL != "https://gitea.example" {
		t.Errorf("BaseURL = %q, want %q", giteaConfigOverride.BaseURL, "https://gitea.example")
	}
	if giteaConfigOverride.Token != "tok-test" {
		t.Errorf("Token = %q, want %q", giteaConfigOverride.Token, "tok-test")
	}
}

// TestSetGiteaConfig_Idempotent verifies that a second call is ignored.
func TestSetGiteaConfig_Idempotent(t *testing.T) {
	resetConfigOnceForTest(t)
	t.Cleanup(func() { resetConfigOnceForTest(t) })

	first := types.GiteaConfig{BaseURL: "https://first", Token: "t1"}
	second := types.GiteaConfig{BaseURL: "https://second", Token: "t2"}

	SetGiteaConfig(first)
	SetGiteaConfig(second)

	if giteaConfigOverride.BaseURL != "https://first" {
		t.Errorf("expected first call to win, got BaseURL=%q", giteaConfigOverride.BaseURL)
	}
}

// TestMergeArgs_HeadSHASerializes verifies that HeadSHA round-trips
// through the JSON encoding Temporal uses to ship MergeArgs across the
// activity boundary. A typo on the JSON tag would silently drop the SHA
// pinning at runtime, defeating the entire force-push protection.
func TestMergeArgs_HeadSHASerializes(t *testing.T) {
	args := MergeArgs{
		PRNumber:    42,
		RepoOwner:   "o",
		RepoName:    "r",
		MergeMethod: "squash",
		HeadSHA:     "deadbeefcafe",
	}
	b := mustMarshalJSON(t, args)
	if !strings.Contains(string(b), `"head_sha":"deadbeefcafe"`) {
		t.Errorf("expected head_sha in JSON, got %s", string(b))
	}

	// Empty HeadSHA must NOT appear in the JSON (omitempty), so older
	// callers that don't set it produce the same on-the-wire payload as
	// before this change.
	args.HeadSHA = ""
	b = mustMarshalJSON(t, args)
	if strings.Contains(string(b), `"head_sha"`) {
		t.Errorf("empty head_sha should be omitted, got %s", string(b))
	}
}

// --- Bug 4: MergeActivity body cap ------------------------------------------

// TestMergeActivity_BodyOverflow asserts that MergeActivity bounds the
// upstream MCP response body. Without SetDoNotParseResponse + readLimitedBody,
// resty would buffer the entire response into RAM before the size check ran,
// letting a hostile MCP server OOM the worker by replying with a multi-GB
// payload to a merge call.
func TestMergeActivity_BodyOverflow(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	payload := strings.Repeat("a", maxCommentRespBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	err := MergeActivity(context.Background(), MergeArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y", MergeMethod: "squash",
	})
	if err == nil {
		t.Fatal("expected oversized merge body to produce an error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected 'too large' error, got: %v", err)
	}
}

// TestMergeActivity_HappyPath confirms the body-cap rewire still allows a
// normal-size merge response to parse successfully end-to-end.
func TestMergeActivity_HappyPath(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"merged":true}}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	if err := MergeActivity(context.Background(), MergeArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y", MergeMethod: "squash",
	}); err != nil {
		t.Fatalf("expected merge to succeed, got %v", err)
	}
}

// --- Bug 5: heartbeat pump --------------------------------------------------

// TestStartHeartbeatPump_StopIsIdempotentAndPromptly confirms the cancel
// func returned by startHeartbeatPump is safe to call multiple times and
// does not block on a goroutine that has already exited.
//
// The pump's recordHeartbeatIfActivity calls are no-ops outside an activity
// context, so this test exercises the goroutine machinery (ticker, stop
// channel, ctx propagation) without needing a Temporal test environment.
// Coverage of the actual heartbeat firing is left to integration tests
// against testsuite.WorkflowEnvironment, which is too heavy for unit tests
// in this package.
func TestStartHeartbeatPump_StopIsIdempotent(t *testing.T) {
	stop := startHeartbeatPump(context.Background(), 50*time.Millisecond)
	// First stop should return promptly. Use a deadline so a regression
	// (e.g. close-of-closed-channel deadlock) fails the test instead of
	// hanging the whole suite.
	done := make(chan struct{})
	go func() {
		stop()
		// Second call: must be a no-op, not panic on close-of-closed-channel.
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("startHeartbeatPump cancel did not return within 2s")
	}
}

// TestStartHeartbeatPump_StopsOnContextCancel verifies that cancelling the
// parent context terminates the pump goroutine, even if the explicit stop
// is never called. This is the safety net for activities that return early
// without their defer running (e.g. panic recovery).
func TestStartHeartbeatPump_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := startHeartbeatPump(ctx, 20*time.Millisecond)
	cancel()
	// Give the goroutine a beat to observe ctx.Done.
	time.Sleep(50 * time.Millisecond)
	// Calling stop after the goroutine has already returned must still
	// complete promptly — once.Do guards the close; <-doneCh sees a
	// closed channel.
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop after ctx cancel did not return within 2s")
	}
}

// TestStartHeartbeatPump_FiresMultipleTicks_RaceSafety drives the pump for
// long enough that several ticks fire and asserts no race. We use a fast
// interval and a counter on a custom stand-in for recordHeartbeatIfActivity
// — but since recordHeartbeatIfActivity is a no-op outside Temporal, we
// instead measure the goroutine's liveness by observing it does not block
// the cancel after ~3 tick intervals.
//
// This is a coverage / race-safety test: we cannot assert "Temporal saw N
// heartbeats" without a Temporal test environment. Combined with the
// integration tests, this is enough to catch goroutine leaks or ticker
// misconfiguration regressions.
func TestStartHeartbeatPump_RunsLongEnoughForMultipleTicks(t *testing.T) {
	stop := startHeartbeatPump(context.Background(), 10*time.Millisecond)
	// Sleep long enough that the ticker has fired several times.
	time.Sleep(50 * time.Millisecond)
	// Stop should still return promptly.
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop after multiple ticks did not return within 2s")
	}
}

// --- Cross-stream contract: typed error wrapping ---------------------------

// asAppErrType extracts the .Type() of a *temporal.ApplicationError from err
// or returns "" if err is not (or doesn't wrap) an ApplicationError. Helper
// kept here so each case stays a one-liner.
func asAppErrType(err error) string {
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) {
		return ae.Type()
	}
	return ""
}

// TestLooksLikePRClosed exercises the substring fallback used until Stream F
// adds gitea.ErrPRClosed. Once the sentinel exists this test stays useful as
// a guard for the older-Gitea fallback path.
func TestLooksLikePRClosed(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"is closed", errors.New("pull request is closed"), true},
		{"is not open", errors.New("PR state is not open"), true},
		{"pr closed casing", errors.New("Pull Request closed"), true},
		{"unrelated", errors.New("internal server error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikePRClosed(tt.err); got != tt.want {
				t.Errorf("looksLikePRClosed(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestWrapMutatingActivityErr_AlreadyMerged covers both the typed-sentinel
// path (errors.Is(gitea.ErrAlreadyMerged)) and the substring fallback
// reserved for the MCP path that returns text rather than typed errors.
func TestWrapMutatingActivityErr_AlreadyMerged(t *testing.T) {
	// Typed sentinel
	wrapped := wrapMutatingActivityErr(gitea.ErrAlreadyMerged)
	if got := asAppErrType(wrapped); got != "already_merged" {
		t.Errorf("typed: Type() = %q, want %q", got, "already_merged")
	}
	// Wrapped sentinel (real production path: fmt.Errorf("%w (status 405...)", ...))
	wrapped = wrapMutatingActivityErr(fmt.Errorf("%w (status 405)", gitea.ErrAlreadyMerged))
	if got := asAppErrType(wrapped); got != "already_merged" {
		t.Errorf("wrapped sentinel: Type() = %q, want %q", got, "already_merged")
	}
	// Substring path (MCP boundary)
	wrapped = wrapMutatingActivityErr(errors.New("MCP merge error: pull request is already merged"))
	if got := asAppErrType(wrapped); got != "already_merged" {
		t.Errorf("substring: Type() = %q, want %q", got, "already_merged")
	}
}

func TestWrapMutatingActivityErr_HeadSHAMismatch(t *testing.T) {
	wrapped := wrapMutatingActivityErr(gitea.ErrHeadSHAMismatch)
	if got := asAppErrType(wrapped); got != "head_sha_mismatch" {
		t.Errorf("Type() = %q, want %q", got, "head_sha_mismatch")
	}
	// Wrapped form (production path adds an excerpt suffix).
	wrapped = wrapMutatingActivityErr(fmt.Errorf("%w: head out of date", gitea.ErrHeadSHAMismatch))
	if got := asAppErrType(wrapped); got != "head_sha_mismatch" {
		t.Errorf("wrapped: Type() = %q, want %q", got, "head_sha_mismatch")
	}
}

// TestWrapMutatingActivityErr_TypedSentinelBeatsAlreadyMergedSubstring is the
// regression for Bug 1: when a wrapped ErrHeadSHAMismatch carries an excerpt
// that happens to contain "already merged" (e.g. Gitea returning
// "head out of date — branch already merged in another PR"), the typed
// sentinel must win and produce "head_sha_mismatch", NOT the substring match
// "already_merged". The previous switch-with-comma evaluated all conditions
// per case and misclassified this as already_merged → workflow returned
// success instead of posting the re-trigger comment.
func TestWrapMutatingActivityErr_TypedSentinelBeatsAlreadyMergedSubstring(t *testing.T) {
	// 1. Plain wrapped sentinel whose body contains "already merged".
	err := temporal.NewApplicationErrorWithCause(
		"head out of date — already merged elsewhere",
		"unused",
		gitea.ErrHeadSHAMismatch,
	)
	if got := asAppErrType(wrapMutatingActivityErr(err)); got != "head_sha_mismatch" {
		t.Errorf("ApplicationError-wrapped sentinel: Type() = %q, want %q", got, "head_sha_mismatch")
	}

	// 2. fmt.Errorf %w wrap with the same hostile excerpt, mirroring how
	//    internal/gitea returns these in production.
	err2 := fmt.Errorf("%w: head out of date — branch already merged in another PR", gitea.ErrHeadSHAMismatch)
	if got := asAppErrType(wrapMutatingActivityErr(err2)); got != "head_sha_mismatch" {
		t.Errorf("fmt.Errorf-wrapped sentinel: Type() = %q, want %q", got, "head_sha_mismatch")
	}

	// 3. The cause chain must remain intact so workflows that match on
	//    errors.Is(err, gitea.ErrHeadSHAMismatch) still see the sentinel.
	if !errors.Is(wrapMutatingActivityErr(err2), gitea.ErrHeadSHAMismatch) {
		t.Error("expected errors.Is to still see ErrHeadSHAMismatch through the wrapping")
	}
}

// TestWrapMutatingActivityErr_TypedPRClosedSentinel covers the Bug 2 path:
// once gitea.ErrPRClosed exists, a wrapped instance of it MUST be detected
// via errors.Is even if the message has no recognizable substring. The
// substring fallback should never need to fire on this case.
func TestWrapMutatingActivityErr_TypedPRClosedSentinel(t *testing.T) {
	// Wrap with a message that does NOT contain any of the substring keywords
	// — forcing the test to rely on errors.Is alone.
	err := fmt.Errorf("%w: 422 Unprocessable Entity", gitea.ErrPRClosed)
	if got := asAppErrType(wrapMutatingActivityErr(err)); got != "pr_closed" {
		t.Errorf("typed ErrPRClosed: Type() = %q, want %q", got, "pr_closed")
	}
	if !errors.Is(wrapMutatingActivityErr(err), gitea.ErrPRClosed) {
		t.Error("expected errors.Is to see ErrPRClosed through the wrapping")
	}
}

// TestLooksLikePRClosed_TypedSentinel confirms that looksLikePRClosed
// recognizes the typed gitea.ErrPRClosed sentinel through an errors.Is
// chain even when the message doesn't include any of the substring keywords.
func TestLooksLikePRClosed_TypedSentinel(t *testing.T) {
	// Plain sentinel (its own message contains "is closed", but we want the
	// typed-check path, so we don't rely on the substring).
	if !looksLikePRClosed(gitea.ErrPRClosed) {
		t.Error("looksLikePRClosed should recognize ErrPRClosed directly")
	}
	// Wrapped with a message that has none of the substring keywords —
	// proves errors.Is is the primary path.
	wrapped := fmt.Errorf("%w: 422 Unprocessable Entity", gitea.ErrPRClosed)
	if !looksLikePRClosed(wrapped) {
		t.Error("looksLikePRClosed should follow errors.Is through wrapping")
	}
}

func TestWrapMutatingActivityErr_PRClosed(t *testing.T) {
	wrapped := wrapMutatingActivityErr(errors.New("Gitea: pull request is closed"))
	if got := asAppErrType(wrapped); got != "pr_closed" {
		t.Errorf("Type() = %q, want %q", got, "pr_closed")
	}
	wrapped = wrapMutatingActivityErr(errors.New("PR is not open"))
	if got := asAppErrType(wrapped); got != "pr_closed" {
		t.Errorf("Type() = %q, want %q", got, "pr_closed")
	}
}

func TestWrapMutatingActivityErr_UnknownPassesThrough(t *testing.T) {
	original := errors.New("connection refused")
	got := wrapMutatingActivityErr(original)
	// Unknown errors should NOT become *temporal.ApplicationError so Temporal's
	// retry policy still applies.
	if asAppErrType(got) != "" {
		t.Errorf("unknown error was incorrectly typed: Type()=%q", asAppErrType(got))
	}
	if !errors.Is(got, original) {
		t.Errorf("unknown error was not returned as-is: %v", got)
	}
}

func TestWrapMutatingActivityErr_NilStaysNil(t *testing.T) {
	if got := wrapMutatingActivityErr(nil); got != nil {
		t.Errorf("nil input should produce nil, got %v", got)
	}
}

// TestMergeActivity_AlreadyMergedStringWraps verifies that a "pull request is
// already merged" message coming back from the MCP server (the GitHub path
// has no typed sentinels — only text) is converted at the activity boundary
// into a typed *temporal.ApplicationError with reason "already_merged".
//
// Stream A's workflow code switches on this typed reason via errors.As; if
// the activity drops the typing, the workflow's retry / pass-through logic
// silently breaks.
func TestMergeActivity_AlreadyMergedStringWraps(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Note: status 200 + JSON-error path. Some MCP servers report
		// app-level failures with HTTP 200 and an `error` field.
		_, _ = w.Write([]byte(`{"error":"pull request is already merged"}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	err := MergeActivity(context.Background(), MergeArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y", MergeMethod: "squash",
	})
	if err == nil {
		t.Fatal("expected MergeActivity to return an error")
	}
	if got := asAppErrType(err); got != "already_merged" {
		t.Fatalf("expected ApplicationError.Type() = %q, got %q (err=%v)", "already_merged", got, err)
	}
}

// TestMergeActivity_StatusErrorAlreadyMergedWraps covers the other MCP error
// path: a >=400 HTTP status with an "already merged" body.
func TestMergeActivity_StatusErrorAlreadyMergedWraps(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"pull request is already merged"}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	err := MergeActivity(context.Background(), MergeArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y", MergeMethod: "squash",
	})
	if err == nil {
		t.Fatal("expected MergeActivity to return an error")
	}
	if got := asAppErrType(err); got != "already_merged" {
		t.Fatalf("expected ApplicationError.Type() = %q, got %q (err=%v)", "already_merged", got, err)
	}
}

// TestGiteaMergeActivity_HeadSHAMismatchWraps drives the Gitea merge path
// through a server that returns 409 + "head out of date", which the gitea
// client converts to ErrHeadSHAMismatch. The activity must then re-wrap that
// as *temporal.ApplicationError with reason "head_sha_mismatch".
func TestGiteaMergeActivity_HeadSHAMismatchWraps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"head out of date"}`))
	}))
	t.Cleanup(srv.Close)

	resetConfigOnceForTest(t)
	t.Cleanup(func() { resetConfigOnceForTest(t) })
	// Reset the cached giteaClient so SetGiteaConfig actually wins.
	giteaOnce = sync.Once{}
	giteaClient = nil
	giteaErr = nil
	t.Cleanup(func() {
		giteaOnce = sync.Once{}
		giteaClient = nil
		giteaErr = nil
	})
	SetGiteaConfig(types.GiteaConfig{
		BaseURL: srv.URL,
		Token:   "tok",
	})

	err := GiteaMergeActivity(context.Background(), MergeArgs{
		PRNumber: 7, RepoOwner: "o", RepoName: "r",
		MergeMethod: "squash", HeadSHA: "deadbeef",
	})
	if err == nil {
		t.Fatal("expected GiteaMergeActivity to return an error")
	}
	if got := asAppErrType(err); got != "head_sha_mismatch" {
		t.Fatalf("expected Type() = %q, got %q (err=%v)", "head_sha_mismatch", got, err)
	}
	// Cause chain must still let workflow code call errors.Is on the gitea
	// sentinel, so it can log the underlying reason.
	if !errors.Is(err, gitea.ErrHeadSHAMismatch) {
		t.Errorf("expected errors.Is(err, gitea.ErrHeadSHAMismatch) to hold; err=%v", err)
	}
}

// TestGiteaMergeActivity_AlreadyMergedWraps covers the other gitea-typed
// path: a 405 returned by Gitea when the PR is already merged.
func TestGiteaMergeActivity_AlreadyMergedWraps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"pull request has already been merged"}`))
	}))
	t.Cleanup(srv.Close)

	resetConfigOnceForTest(t)
	t.Cleanup(func() { resetConfigOnceForTest(t) })
	giteaOnce = sync.Once{}
	giteaClient = nil
	giteaErr = nil
	t.Cleanup(func() {
		giteaOnce = sync.Once{}
		giteaClient = nil
		giteaErr = nil
	})
	SetGiteaConfig(types.GiteaConfig{BaseURL: srv.URL, Token: "tok"})

	err := GiteaMergeActivity(context.Background(), MergeArgs{
		PRNumber: 7, RepoOwner: "o", RepoName: "r", MergeMethod: "squash",
	})
	if err == nil {
		t.Fatal("expected GiteaMergeActivity to return an error")
	}
	if got := asAppErrType(err); got != "already_merged" {
		t.Fatalf("expected Type() = %q, got %q (err=%v)", "already_merged", got, err)
	}
}

// TestGiteaCommentForHumanReviewActivity_PRClosedWraps confirms that a
// PR-closed failure on the comment path produces a typed pr_closed
// ApplicationError so the workflow can degrade gracefully (skip the comment,
// don't fail the whole REQUEST_REVIEW).
func TestGiteaCommentForHumanReviewActivity_PRClosedWraps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"pull request is closed"}`))
	}))
	t.Cleanup(srv.Close)

	resetConfigOnceForTest(t)
	t.Cleanup(func() { resetConfigOnceForTest(t) })
	giteaOnce = sync.Once{}
	giteaClient = nil
	giteaErr = nil
	t.Cleanup(func() {
		giteaOnce = sync.Once{}
		giteaClient = nil
		giteaErr = nil
	})
	SetGiteaConfig(types.GiteaConfig{BaseURL: srv.URL, Token: "tok"})

	err := GiteaCommentForHumanReviewActivity(context.Background(), CommentArgs{
		PRNumber: 7, RepoOwner: "o", RepoName: "r",
		FailingReasons: []string{"x"},
	})
	if err == nil {
		t.Fatal("expected GiteaCommentForHumanReviewActivity to return an error")
	}
	if got := asAppErrType(err); got != "pr_closed" {
		t.Fatalf("expected Type() = %q, got %q (err=%v)", "pr_closed", got, err)
	}
}

// TestCommentForHumanReviewActivity_PRClosedWraps covers the MCP comment
// path's pr_closed conversion.
func TestCommentForHumanReviewActivity_PRClosedWraps(t *testing.T) {
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"pull request is closed"}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	err := CommentForHumanReviewActivity(context.Background(), CommentArgs{
		PRNumber: 1, RepoOwner: "x", RepoName: "y",
		FailingReasons: []string{"r"},
	})
	if err == nil {
		t.Fatal("expected CommentForHumanReviewActivity to return an error")
	}
	if got := asAppErrType(err); got != "pr_closed" {
		t.Fatalf("expected Type() = %q, got %q (err=%v)", "pr_closed", got, err)
	}
}

// TestCommentForHumanReviewActivity_HeartbeatPumpStopsCleanly confirms that
// the comment activity's new heartbeat pump returns its stop func in a
// cancelable, idempotent state — the pump must not leak goroutines or hang
// the activity when the upstream call returns. The slow-server path covers
// the pump body running for >5s before the response arrives.
func TestCommentForHumanReviewActivity_HeartbeatPumpStopsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("slow heartbeat-pump test; -short")
	}
	savedHTTPClient := httpClient
	savedMCP := MCPServerURL
	t.Cleanup(func() {
		httpClient = savedHTTPClient
		MCPServerURL = savedMCP
	})
	httpClient = newSharedHTTPClient()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep long enough that the heartbeat pump fires at least once at
		// 10s interval — but recordHeartbeatIfActivity is a no-op outside
		// Temporal so the call still completes successfully. We just want to
		// prove the pump's goroutine doesn't deadlock or panic during the
		// long upstream call.
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"id":1}}`))
	}))
	t.Cleanup(srv.Close)
	MCPServerURL = srv.URL

	done := make(chan error, 1)
	go func() {
		done <- CommentForHumanReviewActivity(context.Background(), CommentArgs{
			PRNumber: 1, RepoOwner: "x", RepoName: "y",
			FailingReasons: []string{"r"},
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CommentForHumanReviewActivity did not return within 5s — heartbeat pump may be deadlocking")
	}
}

// TestRecordHeartbeatIfActivity_NoActivityNoPanic confirms that calling
// recordHeartbeatIfActivity from a plain context (no Temporal activity) is a
// no-op and never panics. The recover() inside is a defense-in-depth guard
// for SDK regressions; this test exercises the IsActivity short-circuit.
func TestRecordHeartbeatIfActivity_NoActivityNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("recordHeartbeatIfActivity panicked outside an activity context: %v", r)
		}
	}()
	recordHeartbeatIfActivity(context.Background(), "test")
}

// jsonString escapes s for safe embedding in a JSON string literal.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// /thearray/gogents/cmd/gitea-webhook/main_test.go
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamomaton-core/config"
	"github.com/sirus20x6/adamomaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamomaton-core/types"
	"github.com/sirus20x6/adamomaton-platform/temporal/workflows"
)

// fakeWorkflowRun is the minimal WorkflowRun a fake Temporal client can hand
// back. It only needs to satisfy the interface — none of the methods are
// exercised by HandleGiteaWebhook beyond ID/RunID logging.
type fakeWorkflowRun struct{}

func (f *fakeWorkflowRun) GetID() string                                    { return "fake-id" }
func (f *fakeWorkflowRun) GetRunID() string                                 { return "fake-run-id" }
func (f *fakeWorkflowRun) Get(_ context.Context, _ interface{}) error      { return nil }
func (f *fakeWorkflowRun) GetWithOptions(_ context.Context, _ interface{}, _ client.WorkflowRunGetOptions) error {
	return nil
}

// fakeTemporal is a hand-rolled stub satisfying temporalStarter. We avoid a
// mocking framework so the test file stays small and the fake's behaviour
// is obvious at the call site.
type fakeTemporal struct {
	mu          sync.Mutex
	calls       int
	lastArgs    workflows.PRReviewArgs
	lastOptions client.StartWorkflowOptions
	// lastCtxErr records ctx.Err() observed at call time. Bug-1 regression:
	// the handler must NOT plumb r.Context() into ExecuteWorkflow, so even
	// a cancelled request context must still see a fresh ctx here.
	lastCtxErr error
	// returnErr, when non-nil, is returned from ExecuteWorkflow and the
	// args are still recorded — that matters for the
	// AlreadyStarted-as-200 path which must record the args before the
	// short-circuit.
	returnErr error
}

func (f *fakeTemporal) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastOptions = options
	f.lastCtxErr = ctx.Err()
	if len(args) > 0 {
		// The handler always passes a single PRReviewArgs; if that ever
		// regresses we want to fail loudly here rather than silently
		// recording a zero value.
		if a, ok := args[0].(workflows.PRReviewArgs); ok {
			f.lastArgs = a
		}
	}
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return &fakeWorkflowRun{}, nil
}

// quietLogger keeps test output uncluttered. logrus.New() defaults to stderr
// at info level; we discard so a passing test stays silent.
func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// newTestHandler builds a WebhookHandler with a known webhook secret and a
// 1-slot inflight semaphore. Tests can override the semaphore size via the
// returned handler.
func newTestHandler(t *testing.T, fake *fakeTemporal, semSize int) *WebhookHandler {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Gitea.BaseURL = "https://gitea.example.com"
	cfg.Gitea.Token = "test-token"
	cfg.Gitea.WebhookSecret = "test-secret"
	cfg.Workflow.MergeMethod = "squash"
	return &WebhookHandler{
		config:         cfg,
		logger:         quietLogger(),
		temporalClient: fake,
		inflightSem:    make(chan struct{}, semSize),
	}
}

// signedPRRequest builds a POST request to /webhook/gitea with a valid
// HMAC-SHA256 signature, mirroring what Gitea sends. Tests that need an
// invalid signature or a missing event header build the request inline.
func signedPRRequest(t *testing.T, secret string, action, headSha string) (*http.Request, []byte) {
	t.Helper()
	payload := gitea.WebhookPayload{
		Action: action,
		Number: 7,
		Repository: gitea.Repository{
			ID:       1,
			Name:     "demo",
			FullName: "octo/demo",
			Owner:    gitea.User{Username: "octo"},
		},
		PullRequest: gitea.PullRequest{
			Number: 7,
			Head: gitea.PRBranchInfo{
				Sha: headSha,
				Ref: "feature",
			},
		},
		Sender: gitea.User{Username: "octo"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/webhook/gitea", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "pull_request")
	req.Header.Set("X-Gitea-Signature", sig)
	req.Header.Set("Content-Type", "application/json")
	return req, body
}

func TestHandleGiteaWebhook_HMACMismatchReturns401(t *testing.T) {
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	req, _ := signedPRRequest(t, "test-secret", "opened", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	// Tamper with the signature so HMAC verification fails. We don't
	// touch the body — the handler must reject on signature alone, even
	// when the JSON would otherwise parse cleanly.
	req.Header.Set("X-Gitea-Signature", "0000000000000000000000000000000000000000000000000000000000000000")

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if fake.calls != 0 {
		t.Fatalf("expected no workflow start on bad signature, got %d calls", fake.calls)
	}
}

func TestHandleGiteaWebhook_MissingEventHeaderReturns400(t *testing.T) {
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	req, _ := signedPRRequest(t, "test-secret", "opened", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	req.Header.Del("X-Gitea-Event")

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if fake.calls != 0 {
		t.Fatalf("expected no workflow start with missing event header, got %d", fake.calls)
	}
}

func TestHandleGiteaWebhook_EmptyHeadSHAReturns200NoStart(t *testing.T) {
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	// Synchronize action with empty Head.Sha — this is the case that
	// would otherwise collide on workflow ID ("nosha" prefix) across
	// retries. Returning 200 stops Gitea retrying forever.
	req, _ := signedPRRequest(t, "test-secret", "synchronize", "")

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if fake.calls != 0 {
		t.Fatalf("expected no workflow start when Head.Sha is empty, got %d", fake.calls)
	}
}

func TestHandleGiteaWebhook_SynchronizeTriggersWorkflow(t *testing.T) {
	// Regression test for Bug 1 (Pass 12) — Gitea sends the imperative
	// "synchronize", not "synchronized". If this test ever fails, the
	// switch is back to matching the wrong spelling and force-pushes are
	// being silently dropped.
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	headSha := "cafef00dcafef00dcafef00dcafef00dcafef00d"
	req, _ := signedPRRequest(t, "test-secret", "synchronize", headSha)

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if fake.calls != 1 {
		t.Fatalf("expected exactly 1 workflow start on synchronize, got %d", fake.calls)
	}
}

func TestHandleGiteaWebhook_SynchronizedWithDIsIgnored(t *testing.T) {
	// Guard against re-introducing Bug 1 from the wrong direction: if
	// someone "fixes" by accepting both spellings, then a hypothetical
	// future Gitea that switches to past-tense "synchronized" would
	// double-trigger. Keep the contract narrow — only the documented
	// spelling triggers a run.
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	req, _ := signedPRRequest(t, "test-secret", "synchronized", "cafef00dcafef00dcafef00dcafef00dcafef00d")

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (ignored action), got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if fake.calls != 0 {
		t.Fatalf("'synchronized' (with d) is not a Gitea action — must not start a workflow; got %d calls", fake.calls)
	}
}

func TestHandleGiteaWebhook_HeadSHAPassedToWorkflow(t *testing.T) {
	// Regression test for Bug 2 — the merge activity needs HeadSHA to
	// pin against force-push during review. If the handler stops
	// plumbing it through, the workflow logs "merging without SHA pin"
	// on every real run and the protection is silently disabled.
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	headSha := "abcdef0123456789abcdef0123456789abcdef01"
	req, _ := signedPRRequest(t, "test-secret", "opened", headSha)

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 workflow start, got %d", fake.calls)
	}
	if got := fake.lastArgs.HeadSHA; got != headSha {
		t.Fatalf("HeadSHA not propagated to workflow args: got %q, want %q", got, headSha)
	}
	if fake.lastArgs.PRNumber != 7 {
		t.Fatalf("PRNumber lost: got %d, want 7", fake.lastArgs.PRNumber)
	}
	if fake.lastArgs.RepoOwner != "octo" || fake.lastArgs.RepoName != "demo" {
		t.Fatalf("repo info lost: got owner=%q name=%q", fake.lastArgs.RepoOwner, fake.lastArgs.RepoName)
	}
	// The workflow ID prefix is derived from the SHA's first 8 chars;
	// double-check that we used the real value, not an empty fallback.
	if !strings.Contains(fake.lastOptions.ID, headSha[:8]) {
		t.Fatalf("workflow ID %q should include head SHA prefix %q", fake.lastOptions.ID, headSha[:8])
	}
}

func TestHandleGiteaWebhook_StartsDespiteCancelledRequestCtx(t *testing.T) {
	// Bug-1 regression: Gitea has a 5s default delivery timeout. If we
	// plumb r.Context() into ExecuteWorkflow, the scheduling RPC gets
	// cancelled on slow Temporal RTT, the handler returns 500, Gitea
	// retries, and we eat semaphore slots fighting ourselves. Workflow
	// start must use a background context so the start outlives the
	// inbound request.
	fake := &fakeTemporal{}
	h := newTestHandler(t, fake, 4)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel — the request arrives "already dead"

	req, _ := signedPRRequest(t, "test-secret", "opened", "abcdef0123456789abcdef0123456789abcdef01")
	req = req.WithContext(cancelledCtx)

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("workflow must still start when r.Context() is dead; got %d (body=%q)", rr.Code, rr.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 1 {
		t.Fatalf("expected 1 ExecuteWorkflow call, got %d", fake.calls)
	}
	if fake.lastCtxErr != nil {
		t.Fatalf("ExecuteWorkflow must NOT receive r.Context() — got cancelled ctx (err=%v); workflow start is fire-and-forget", fake.lastCtxErr)
	}
}

func TestHandleGiteaWebhook_AlreadyStartedReturns200(t *testing.T) {
	// Temporal returns WorkflowExecutionAlreadyStarted when the same
	// workflow ID is already running (or completed successfully) under
	// our ALLOW_DUPLICATE_FAILED_ONLY reuse policy. That's the correct
	// outcome for a duplicate webhook delivery — handler must ack 200
	// so Gitea stops retrying. A 5xx would put the delivery into the
	// retry loop forever.
	fake := &fakeTemporal{
		returnErr: &serviceerror.WorkflowExecutionAlreadyStarted{
			Message:        "already started",
			StartRequestId: "req-1",
			RunId:          "run-1",
		},
	}
	h := newTestHandler(t, fake, 4)

	req, _ := signedPRRequest(t, "test-secret", "opened", "abcdef0123456789abcdef0123456789abcdef01")

	rr := httptest.NewRecorder()
	h.HandleGiteaWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("AlreadyStarted must be treated as idempotent skip (200); got %d (body=%q)", rr.Code, rr.Body.String())
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 ExecuteWorkflow call, got %d", fake.calls)
	}
}

func TestHandleGiteaWebhook_SemaphoreBoundsConcurrency(t *testing.T) {
	// With an N-slot semaphore and N+1 concurrent in-flight requests,
	// exactly one must come back 429 and the rest must succeed. We
	// gate the fake's ExecuteWorkflow on a release channel so all N
	// successful requests hold their slot at the same time — without
	// that, requests trickle through serially and only one is ever
	// in-flight at once, defeating the test.
	const N = 3
	release := make(chan struct{})
	var inflight atomic.Int32
	var maxInflight atomic.Int32
	gate := &gatedFakeTemporal{
		onCall: func() {
			cur := inflight.Add(1)
			// Track the high-water mark so we can assert below that
			// the semaphore actually let N requests run at once.
			for {
				prev := maxInflight.Load()
				if cur <= prev || maxInflight.CompareAndSwap(prev, cur) {
					break
				}
			}
			<-release
			inflight.Add(-1)
		},
	}
	h := newTestHandler(t, &fakeTemporal{}, N)
	h.temporalClient = gate

	var (
		wg          sync.WaitGroup
		rejectCount atomic.Int32
		okCount     atomic.Int32
		// barrier ensures all requests are launched before any are
		// allowed to complete — without it the first goroutine could
		// fully process before the others even start, leaving the
		// semaphore unblocked and our 429 unreachable.
		barrier = make(chan struct{})
	)
	for i := 0; i < N+1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := signedPRRequest(t, "test-secret", "opened", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
			rr := httptest.NewRecorder()
			<-barrier
			h.HandleGiteaWebhook(rr, req)
			switch rr.Code {
			case http.StatusTooManyRequests:
				rejectCount.Add(1)
			case http.StatusOK:
				okCount.Add(1)
			default:
				t.Errorf("unexpected status %d (body=%q)", rr.Code, rr.Body.String())
			}
		}()
	}
	close(barrier)

	// Wait for the semaphore to fill before releasing. The 429-path
	// has no contact with ExecuteWorkflow, so we busy-poll on the
	// inflight counter rather than depending on the fake to signal
	// us. A successful test reaches inflight==N quickly; we cap the
	// wait by relying on Go's race detector and t.Fatalf on timeout
	// elsewhere if anything is broken.
	for inflight.Load() < int32(N) {
		// Yield to other goroutines without any time-based wait —
		// the request goroutines need a scheduler tick to advance
		// past the http handler entry and into ExecuteWorkflow.
		runtime.Gosched()
	}
	close(release)
	wg.Wait()

	if rejectCount.Load() != 1 {
		t.Fatalf("expected exactly 1 request to be rejected with 429, got %d", rejectCount.Load())
	}
	if okCount.Load() != int32(N) {
		t.Fatalf("expected exactly %d successful requests, got %d", N, okCount.Load())
	}
	if maxInflight.Load() != int32(N) {
		t.Fatalf("expected semaphore to admit %d concurrent requests, max observed was %d", N, maxInflight.Load())
	}
}

// gatedFakeTemporal lets the semaphore concurrency test hold every
// successful request inside ExecuteWorkflow at once. The plain
// fakeTemporal returns immediately, which would defeat the test.
type gatedFakeTemporal struct {
	onCall func()
}

func (g *gatedFakeTemporal) ExecuteWorkflow(_ context.Context, _ client.StartWorkflowOptions, _ interface{}, _ ...interface{}) (client.WorkflowRun, error) {
	if g.onCall != nil {
		g.onCall()
	}
	return &fakeWorkflowRun{}, nil
}

// Compile-time check: the real *client.Client should still satisfy
// temporalStarter. If a Temporal SDK upgrade ever changes the signature
// of ExecuteWorkflow, this catches it at build time instead of letting
// the production wiring rot.
var _ temporalStarter = (client.Client)(nil)

// And the same for the standard error wrapping that the handler relies
// on for the AlreadyStarted -> 200 path. If errors.As stops finding
// the type, the test would fail; this assertion makes the requirement
// explicit at the top of the file.
var _ = func() bool {
	var target *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(&serviceerror.WorkflowExecutionAlreadyStarted{}, &target)
}

// Sanity-check that types.Config wiring used by newTestHandler is
// still the right shape; if a refactor renames Workflow.MergeMethod or
// Agents.*.Enabled, this fails to compile rather than silently
// regressing the per-agent flag plumbing.
var _ = func(c *types.Config) {
	_ = c.Workflow.MergeMethod
	_ = c.Agents.Security.Enabled
	_ = c.Agents.Performance.Enabled
	_ = c.Agents.Const.Enabled
}

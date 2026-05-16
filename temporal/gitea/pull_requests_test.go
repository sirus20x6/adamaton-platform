// /thearray/gogents/internal/gitea/pull_requests_test.go
//
// Tests for the PR-merge surface. Each test stands up an httptest server
// that pretends to be Gitea and asserts the client's typed errors
// (ErrAlreadyMerged, ErrHeadSHAMismatch, ErrPRClosed) and request encoding
// match the real API contract.
package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamaton-core/types"
)

// counterValueForLabels reads the unknown-409 counter at a specific
// {method, endpoint} label tuple. testutil.ToFloat64 only works on a
// single-value collector, so we look up the labelled child first.
func counterValueForLabels(method, endpoint string) float64 {
	return testutil.ToFloat64(GiteaUnknown409Body.WithLabelValues(method, endpoint))
}

// --- MergePullRequest happy path ------------------------------------------

func TestMergePullRequest_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Contains(t, r.URL.Path, "/api/v1/repos/owner/repo/pulls/7/merge")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	require.NoError(t, c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge"))
}

// --- already-merged detection ---------------------------------------------

func TestMergePullRequest_AlreadyMerged_405(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"The PR is already merged"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAlreadyMerged), "expected ErrAlreadyMerged on 405, got %v", err)
}

func TestMergePullRequest_AlreadyMerged_409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"This pull has already merged"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAlreadyMerged), "expected ErrAlreadyMerged on 409 with merged body, got %v", err)
}

func TestMergePullRequest_405WithUnknownBody_StillAlreadyMerged(t *testing.T) {
	// 405 from /merge with a body the matcher doesn't recognize is still
	// surfaced as ErrAlreadyMerged because old Gitea variants don't always
	// echo "already merged" — the typed sentinel covers both.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"unrelated"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAlreadyMerged), "any 405 should map to ErrAlreadyMerged, got %v", err)
}

func TestMergePullRequest_OtherErrors(t *testing.T) {
	// 500 is a generic server error — not ErrAlreadyMerged, not
	// ErrHeadSHAMismatch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal explosion`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAlreadyMerged))
	require.False(t, errors.Is(err, ErrHeadSHAMismatch))
	require.Contains(t, err.Error(), "status 500")
}

// --- HeadSHA pin behavior --------------------------------------------------

func TestMergePullRequest_HeadSHAPin_IncludedInBody(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequestWithOptions(context.Background(), "owner", "repo", 7, "merge",
		MergeOptions{HeadSHA: "abc123"})
	require.NoError(t, err)

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(capturedBody, &payload))
	require.Equal(t, "abc123", payload["head_commit_id"], "head_commit_id should be in the merge payload")
}

func TestMergePullRequest_HeadSHAEmpty_OmittedFromBody(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequestWithOptions(context.Background(), "owner", "repo", 7, "merge",
		MergeOptions{}) // no HeadSHA
	require.NoError(t, err)

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(capturedBody, &payload))
	_, hasKey := payload["head_commit_id"]
	require.False(t, hasKey, "head_commit_id key should be omitted when HeadSHA is empty")
}

func TestMergePullRequest_HeadSHAMismatch_TypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"head out of date"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequestWithOptions(context.Background(), "owner", "repo", 7, "merge",
		MergeOptions{HeadSHA: "stale-sha"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrHeadSHAMismatch), "expected ErrHeadSHAMismatch, got %v", err)
}

// --- Invalid merge methods -------------------------------------------------

func TestMergePullRequest_InvalidMethod_NoNetworkCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	for _, badMethod := range []string{"", "fastforward", "Squash", "MERGE", "auto"} {
		t.Run("method="+badMethod, func(t *testing.T) {
			err := c.MergePullRequest(context.Background(), "owner", "repo", 7, badMethod)
			require.Error(t, err, "method %q should be rejected before sending", badMethod)
			require.Contains(t, err.Error(), "invalid Gitea merge method")
			require.Contains(t, err.Error(), "merge, squash, rebase, rebase-merge")
		})
	}
	require.False(t, called, "invalid merge methods must not reach the server")
}

func TestMergePullRequest_AllValidMethods(t *testing.T) {
	var seenMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if v, ok := p["Do"].(string); ok {
			seenMethod = v
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	for _, m := range []string{"merge", "squash", "rebase", "rebase-merge"} {
		t.Run(m, func(t *testing.T) {
			require.NoError(t, c.MergePullRequest(context.Background(), "owner", "repo", 7, m))
			require.Equal(t, m, seenMethod, "Do field should match requested method")
		})
	}
}

// --- GetPullRequestDiff ---------------------------------------------------

func TestGetPullRequestDiff_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler must request text/plain (not application/json).
		require.Contains(t, r.Header.Get("Accept"), "text/plain")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	diff, err := c.GetPullRequestDiff(context.Background(), "owner", "repo", 7)
	require.NoError(t, err)
	require.Contains(t, diff, "+new")
}

func TestGetPullRequestDiff_LargeBodyCapped(t *testing.T) {
	// Server emits a body larger than the 10MB cap. The client should
	// silently truncate to 10MB and not error.
	const cap10MB = 10 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		// Write 11MB by repeatedly flushing — keeps memory bounded server-side.
		chunk := make([]byte, 1<<20) // 1MB
		for i := range chunk {
			chunk[i] = 'A'
		}
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 11; i++ {
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	diff, err := c.GetPullRequestDiff(context.Background(), "owner", "repo", 7)
	require.NoError(t, err)
	// Behavior is: silent cap at 10MB. The returned string should not exceed
	// the cap, even though the upstream produced more.
	require.LessOrEqual(t, len(diff), cap10MB, "diff body must be capped at 10MB")
	require.GreaterOrEqual(t, len(diff), cap10MB-1024, "expected close to 10MB returned")
}

func TestGetPullRequestDiff_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such PR"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.GetPullRequestDiff(context.Background(), "owner", "repo", 99)
	require.Error(t, err)
	require.Contains(t, err.Error(), "status 404")
	require.Contains(t, err.Error(), "no such PR")
}

// --- GetPullRequest --------------------------------------------------------

func TestGetPullRequest_Decodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 1,
			"number": 7,
			"title": "Test PR",
			"state": "open",
			"merged": false,
			"head": {"sha": "abcdef", "ref": "feature"},
			"base": {"sha": "0000", "ref": "main"}
		}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	pr, err := c.GetPullRequest(context.Background(), "owner", "repo", 7)
	require.NoError(t, err)
	require.NotNil(t, pr)
	require.Equal(t, int64(7), pr.Number)
	require.Equal(t, "Test PR", pr.Title)
	require.Equal(t, "abcdef", pr.Head.Sha)
}

// --- Merge methods + custom message --------------------------------------

func TestMergePullRequest_DefaultMessage(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	require.NoError(t, c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge"))
	require.Equal(t, defaultMergeMessage, got["merge_message_field"])
}

func TestMergePullRequest_CustomMessage(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	require.NoError(t, c.MergePullRequestWithOptions(context.Background(), "owner", "repo", 7, "merge",
		MergeOptions{
			MergeTitleField:   "feat: thing",
			MergeMessageField: "long-form rationale",
		},
	))
	require.Equal(t, "feat: thing", got["merge_title_field"])
	require.Equal(t, "long-form rationale", got["merge_message_field"])
}

// --- Path encoding ---------------------------------------------------------

func TestMergePullRequest_PathEscapesOwnerAndRepo(t *testing.T) {
	var seenRawPath, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenRawPath = r.URL.RequestURI() // wire-form path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	require.NoError(t, c.MergePullRequest(context.Background(), "with space", "name", 7, "merge"))
	// On the wire, "with space" should be percent-encoded so we don't open a
	// path-injection vector. Go's URL parser decodes Path automatically, but
	// RequestURI() preserves the raw form.
	require.Contains(t, seenRawPath, "with%20space",
		"raw path should contain %%20-encoded space; got path=%q rawPath=%q", seenPath, seenRawPath)
}

// --- ErrPRClosed detection -------------------------------------------------

// TestMergePullRequest_PRClosed_422 covers the canonical "Gitea returns 422
// when you try to merge a closed PR" case. Each of the prClosedKeywords body
// variants must map to ErrPRClosed.
func TestMergePullRequest_PRClosed_422(t *testing.T) {
	bodies := []string{
		`{"message":"This pull request is closed"}`,
		`{"message":"PR is not open"}`,
		`{"message":"the pull request is not open for merging"}`,
		`{"message":"this PR was already closed"}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrPRClosed),
				"expected ErrPRClosed for body %q, got %v", body, err)
		})
	}
}

// TestMergePullRequest_PRClosed_409 verifies that some Gitea forks surface the
// closed condition as 409 with the same body shape — also caught.
func TestMergePullRequest_PRClosed_409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"pull request is closed"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPRClosed),
		"expected ErrPRClosed for 409+closed body, got %v", err)
	// Crucially, must NOT be classified as already-merged or head-mismatch.
	require.False(t, errors.Is(err, ErrAlreadyMerged))
	require.False(t, errors.Is(err, ErrHeadSHAMismatch))
}

// TestMergePullRequest_422NotClosed_NotErrPRClosed defends the "be strict"
// requirement: a 422 with an UNRELATED body (e.g. validation error) must NOT
// degrade into ErrPRClosed, because that would let a workflow silently treat
// real validation failures as benign skips.
func TestMergePullRequest_422NotClosed_NotErrPRClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"merge_message_field is required"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrPRClosed),
		"422 without 'closed'/'not open' body must NOT map to ErrPRClosed, got %v", err)
	require.Contains(t, err.Error(), "status 422")
}

// TestMergePullRequest_Unknown409Increments_Metric verifies the SHA-pin-drift
// detector: a 409 whose body matches NEITHER head-mismatch keywords NOR
// closed-PR keywords increments the unknown-409 counter so operators can
// notice when Gitea changes wording.
func TestMergePullRequest_Unknown409Increments_Metric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"some new gitea wording for the merge conflict"}`))
	}))
	defer srv.Close()

	endpoint := "/api/v1/repos/{owner}/{repo}/pulls/{number}/merge"
	before := counterValueForLabels("POST", endpoint)

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequest(context.Background(), "owner", "repo", 7, "merge")
	require.Error(t, err)
	// Generic error path — neither sentinel.
	require.False(t, errors.Is(err, ErrAlreadyMerged))
	require.False(t, errors.Is(err, ErrHeadSHAMismatch))
	require.False(t, errors.Is(err, ErrPRClosed))
	require.Contains(t, err.Error(), "status 409")

	after := counterValueForLabels("POST", endpoint)
	require.Equal(t, before+1, after,
		"unknown-409 counter should increment by 1 when 409 body matches no known keyword (before=%v after=%v)", before, after)
}

// TestMergePullRequest_KnownHeadMismatch_DoesNotIncrement verifies the
// counter is only ticked for UNKNOWN bodies — a recognised "head out of date"
// body must NOT increment, otherwise the drift signal becomes useless noise.
func TestMergePullRequest_KnownHeadMismatch_DoesNotIncrement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"head out of date"}`))
	}))
	defer srv.Close()

	endpoint := "/api/v1/repos/{owner}/{repo}/pulls/{number}/merge"
	before := counterValueForLabels("POST", endpoint)

	c := newTestClient(t, srv.URL)
	err := c.MergePullRequestWithOptions(context.Background(), "owner", "repo", 7, "merge",
		MergeOptions{HeadSHA: "stale"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrHeadSHAMismatch))

	after := counterValueForLabels("POST", endpoint)
	require.Equal(t, before, after,
		"recognised head-mismatch body must NOT increment the unknown-409 counter")
}

// TestSubmitReview_PRClosed verifies the closed-PR sentinel propagates from
// the comment surface (review submission) too. Stream B's activity wrapper
// reduces ErrPRClosed to a typed pr_closed ApplicationError so the workflow
// can drop the comment and proceed.
func TestSubmitReview_PRClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"the pull request is closed"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.SubmitReview(context.Background(), "owner", "repo", 7, types.ReviewSummary{Recommendation: "MERGE"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPRClosed),
		"expected ErrPRClosed from SubmitReview against closed PR, got %v", err)
}

// TestCreateComment_PRClosed mirrors TestSubmitReview_PRClosed for the bare
// /issues/{n}/comments endpoint.
func TestCreateComment_PRClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"issue is closed"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.CreateComment(context.Background(), "owner", "repo", 7, "hello")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPRClosed),
		"expected ErrPRClosed from CreateComment against closed PR, got %v", err)
}

// TestCreateReviewComment_PRClosed mirrors the above for inline review
// comments.
func TestCreateReviewComment_PRClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"pull is closed"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.CreateReviewComment(context.Background(), "owner", "repo", 7, "abc", "x.go", 1, "nit")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPRClosed),
		"expected ErrPRClosed from CreateReviewComment against closed PR, got %v", err)
}

// TestErrPRClosed_ExportedSentinel locks down the cross-stream contract: the
// sentinel must remain a package-level var of error type, instantiable with
// errors.New, so Stream A's `errors.Is(err, gitea.ErrPRClosed)` and Stream B's
// `temporal.NewApplicationErrorWithCause(..., "pr_closed", err)` keep working.
func TestErrPRClosed_ExportedSentinel(t *testing.T) {
	require.NotNil(t, ErrPRClosed)
	require.Equal(t, "pull request is closed", ErrPRClosed.Error())
	// %w-wrapping must preserve errors.Is — that's the property both Stream A
	// and Stream B rely on. Build a wrapped error with the same shape the
	// production code uses and confirm errors.Is reaches the sentinel.
	wrappedW := fmt.Errorf("wrapped: %w", ErrPRClosed)
	require.True(t, errors.Is(wrappedW, ErrPRClosed),
		"%%w wrapping must preserve errors.Is — this is the cross-stream contract")
	// Plain string concatenation must NOT pretend to be a wrapped error.
	plain := errors.New("upstream noise: " + ErrPRClosed.Error())
	require.False(t, errors.Is(plain, ErrPRClosed),
		"plain string copy must NOT satisfy errors.Is — only %%w wrapping does")
	// Light import-keepalive — strings is used elsewhere in this file.
	require.True(t, strings.HasPrefix("pull request is closed", "pull"))
}

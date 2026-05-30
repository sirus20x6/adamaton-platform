// /thearray/gogents/internal/gitea/pull_requests.go
package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sirupsen/logrus"
	"github.com/sirus20x6/adamaton-core/types"
)

// GiteaUnknown409Body counts 409 responses whose body did NOT match any of the
// known mismatch keywords. Operators should alert on a non-zero rate of these
// because it likely means Gitea has changed the wording of its head-mismatch
// response and the keyword list in `is409HeadMismatch` is silently failing to
// catch it — i.e. the SHA-pin guard has drifted.
//
// We register the metric in this package (not in `internal/metrics`) to avoid
// stepping on Stream E's concurrent edits to that file. Cardinality is bounded
// by `method` (always "POST" today) and `endpoint` (a small set of merge/comment
// paths normalised to remove owner/repo/number).
//
// promauto.With(prometheus.DefaultRegisterer) is used (rather than a plain
// NewCounterVec + init() MustRegister) so that test harnesses that link this
// package twice — or which clear and re-create the registerer — don't trigger
// a duplicate-registration panic at init time.
var GiteaUnknown409Body = promauto.With(prometheus.DefaultRegisterer).NewCounterVec(
	prometheus.CounterOpts{
		Name: "gogents_gitea_unknown_409_body_total",
		Help: "Gitea 409 responses whose body did not match any known mismatch keyword (potential detection drift).",
	},
	[]string{"method", "endpoint"},
)

// GiteaRateLimited counts responses from the Gitea API that came back 429 Too
// Many Requests — i.e. the review/merge token's rate-limit budget was actually
// exhausted, not just running low (checkRateLimit already logs the
// running-low case from the X-RateLimit-Remaining header). Operators should
// alert on any non-zero rate here: a 429 means a review or merge call was
// rejected outright and the workflow either retried or failed.
//
// Same promauto.With(DefaultRegisterer) pattern as GiteaUnknown409Body so a
// test binary that links this package twice doesn't panic on a duplicate
// registration. The gitea-webhook server exposes it via metrics.Handler() on
// /metrics (the default registerer is what promhttp.Handler() scrapes).
var GiteaRateLimited = promauto.With(prometheus.DefaultRegisterer).NewCounter(
	prometheus.CounterOpts{
		Name: "gogents_gitea_rate_limited_total",
		Help: "Total Gitea API responses with HTTP 429 Too Many Requests.",
	},
)

// prClosedKeywords is the closed set of body fragments that Gitea (and the
// forks we've seen in the wild) use to signal "PR is not in an open state".
// The list is intentionally narrow — false positives here would let a workflow
// silently treat a real failure as a benign skip.
var prClosedKeywords = []string{
	"is closed",
	"not open",
	"is not open",
	"already closed",
}

// bodyMatchesPRClosed reports whether the body (already lower-cased) contains
// any keyword indicating the PR is in a non-open state.
func bodyMatchesPRClosed(bodyLower string) bool {
	for _, kw := range prClosedKeywords {
		if strings.Contains(bodyLower, kw) {
			return true
		}
	}
	return false
}

// headSHAMismatchKeywords is the closed set of fragments Gitea / forks emit
// when a head_commit_id pin doesn't match the PR's current head. Kept as a
// package-level slice so the unknown-409 metric can iterate the same list to
// decide whether a 409 was "expected" or drift.
var headSHAMismatchKeywords = []string{
	"head out of date",
	"sha does not match",
	"sha mismatch",
	"head_commit_id",
}

// bodyMatchesHeadMismatch reports whether the body (already lower-cased) is
// recognised as a head-SHA-mismatch error.
func bodyMatchesHeadMismatch(bodyLower string) bool {
	for _, kw := range headSHAMismatchKeywords {
		if strings.Contains(bodyLower, kw) {
			return true
		}
	}
	return false
}

// recordUnknown409 logs a warning and increments the drift counter when a 409
// response body matches none of the known mismatch keywords. The body excerpt
// is truncated to keep log lines bounded.
func (c *GiteaClient) recordUnknown409(method, endpoint string, body []byte) {
	excerpt := truncateForError(body)
	if len(excerpt) > 200 {
		excerpt = excerpt[:200]
	}
	c.logger.WithFields(logrus.Fields{
		"status":   409,
		"method":   method,
		"endpoint": endpoint,
		"body":     excerpt,
	}).Warn("unknown 409 response body — potential mismatch detection drift")
	GiteaUnknown409Body.WithLabelValues(method, endpoint).Inc()
}

// ErrAlreadyMerged is returned by MergePullRequest / MergePullRequestWithOptions
// when Gitea reports that the pull request has already been merged. Callers
// should treat this as a benign success when reconciling state — the workflow
// has already done what it intended, just on a previous run.
//
// Detection is heuristic: Gitea returns 405 ("The PR is already merged"), but
// some forks / older versions surface 409 with a body containing "already
// merged" or "merged". We match either.
var ErrAlreadyMerged = errors.New("pull request is already merged")

// ErrHeadSHAMismatch is returned when MergePullRequestWithOptions was called
// with a HeadSHA pin and Gitea rejected the merge because the current head
// has moved (e.g. force-push during review). Gitea returns 409 with body
// "head out of date".
var ErrHeadSHAMismatch = errors.New("pull request head SHA mismatch")

// ErrPRClosed is returned when Gitea reports the pull request is closed (and
// therefore not mergeable / not commentable). Detection is strict: the
// upstream must return HTTP 422 (or, on some forks, 409) AND a body that
// matches one of prClosedKeywords. False positives here would cause a
// workflow to silently treat real failures as benign skips.
//
// Stream A's workflow code uses errors.Is + ApplicationError.Type() ==
// "pr_closed" to fan out into the no-merge degraded path. Stream B's activity
// wrapper converts this typed error into the ApplicationError. Both must stay
// in sync with this sentinel.
var ErrPRClosed = errors.New("pull request is closed")

// validGiteaMergeMethods is the closed set of merge strategies Gitea understands.
// We validate against this set explicitly so that a misconfiguration surfaces
// as an error instead of silently falling back to "merge".
var validGiteaMergeMethods = map[string]bool{
	"merge":        true,
	"squash":       true,
	"rebase":       true,
	"rebase-merge": true,
}

// defaultMergeMessage is used when no override is supplied via MergeOptions.
const defaultMergeMessage = "Merged by GoGents AI Review System"

// MergeOptions lets callers override merge title/message rather than baking
// the values into the client. Zero values mean "use server defaults" or, for
// MergeMessageField specifically, defaultMergeMessage.
//
// HeadSHA pins the commit being merged: if non-empty, Gitea will refuse the
// merge (returning ErrHeadSHAMismatch) when the PR's current head commit no
// longer matches. This closes a race where a force-push during review could
// slip un-reviewed code into the merge — see E2E trace #28.
type MergeOptions struct {
	MergeTitleField   string
	MergeMessageField string
	HeadSHA           string
}

// classifyMutatingError inspects an upstream non-2xx response on a
// state-mutating endpoint (merge, comment, review) and returns the typed
// sentinel error appropriate for the status + body shape. The endpoint label
// is used for the unknown-409 drift metric and should be a low-cardinality
// route template (e.g. "/api/v1/repos/{owner}/{repo}/issues/{number}/comments"),
// not a concrete URL.
//
// This centralises the closed-PR detection so SubmitReview / CreateComment /
// CreateReviewComment all react identically — Stream B's activity wrapper
// reduces the typed error to a `pr_closed` ApplicationError, so callers don't
// need to know the wire-level wording.
//
// The function returns nil ONLY when status is 2xx; for any other status it
// returns SOME error (typed or generic) so callers can blindly propagate it.
func (c *GiteaClient) classifyMutatingError(method, endpoint string, status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	excerpt := truncateForError(body)
	bodyLower := strings.ToLower(string(body))

	switch status {
	case http.StatusUnprocessableEntity:
		if bodyMatchesPRClosed(bodyLower) {
			return fmt.Errorf("%w: %s", ErrPRClosed, excerpt)
		}
	case http.StatusConflict:
		// Comment endpoints don't trip "head out of date", but a 409 +
		// closed-shaped body has been observed against /reviews on some
		// Gitea forks. Don't increment the unknown-409 counter here unless
		// the body matched none of our keywords.
		if bodyMatchesHeadMismatch(bodyLower) {
			return fmt.Errorf("%w: %s", ErrHeadSHAMismatch, excerpt)
		}
		if bodyMatchesPRClosed(bodyLower) {
			return fmt.Errorf("%w: %s", ErrPRClosed, excerpt)
		}
		c.recordUnknown409(method, endpoint, body)
	}
	return fmt.Errorf("API request failed with status %d: %s", status, excerpt)
}

// doMutating performs a single non-2xx-aware POST/PUT/PATCH against a
// state-mutating endpoint and decodes the response into result on success.
// On a non-2xx response it routes through classifyMutatingError so callers
// receive ErrPRClosed / ErrHeadSHAMismatch / ErrAlreadyMerged where applicable.
//
// We split this from makeRequest because makeRequest is shared with read-only
// endpoints where the closed-PR detection would be inappropriate (and where
// returning a typed sentinel could surprise existing callers).
func (c *GiteaClient) doMutating(ctx context.Context, method, path, endpoint string, payload, result interface{}) error {
	req, err := c.buildRequest(ctx, method, path, payload)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	c.checkRateLimit(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if readErr != nil {
			c.logger.WithError(readErr).Warn("Failed to read mutating-endpoint error body")
		}
		c.logger.WithFields(logrus.Fields{
			"status":       resp.StatusCode,
			"method":       method,
			"path":         path,
			"body_len":     len(body),
			"body_excerpt": truncateForError(body),
		}).Error("Gitea mutating request failed")
		return c.classifyMutatingError(method, endpoint, resp.StatusCode, body)
	}

	if result != nil {
		dec := io.LimitReader(resp.Body, 1<<20) // 1MB cap on success body for comments
		if err := json.NewDecoder(dec).Decode(result); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}
	return nil
}

// GetPullRequest retrieves a pull request by number
func (c *GiteaClient) GetPullRequest(ctx context.Context, owner, repo string, number int64) (*PullRequest, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d", sanitizePathSegment(owner), sanitizePathSegment(repo), number)

	var pr PullRequest
	err := c.makeRequest(ctx, "GET", path, nil, &pr)
	if err != nil {
		return nil, fmt.Errorf("failed to get pull request: %w", err)
	}

	return &pr, nil
}

// GetPullRequestDiff retrieves the diff for a pull request. The diff endpoint
// returns plain text, so we can't go through makeRequest's JSON decoder, but
// we mirror its rate-limit and error-body capture so failures look the same
// across the client.
func (c *GiteaClient) GetPullRequestDiff(ctx context.Context, owner, repo string, number int64) (string, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d.diff", sanitizePathSegment(owner), sanitizePathSegment(repo), number)

	req, err := c.buildRequest(ctx, "GET", path, nil)
	if err != nil {
		return "", err
	}
	// The diff endpoint returns text/plain rather than JSON. Override the
	// Accept header so a strict server doesn't 406 us with the inherited
	// application/json default from buildRequest.
	req.Header.Set("Accept", "text/plain, */*")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	c.checkRateLimit(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if readErr != nil {
			c.logger.WithError(readErr).Warn("Failed to read diff error response body")
		}
		excerpt := truncateForError(body)
		c.logger.WithFields(logrus.Fields{
			"status":       resp.StatusCode,
			"path":         path,
			"body_len":     len(body),
			"body_excerpt": excerpt,
		}).Error("Gitea diff request failed")
		return "", fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, excerpt)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB limit on diff
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	return string(body), nil
}

// MergePullRequest merges a pull request using the supplied merge method.
// Unknown merge methods are rejected rather than silently falling back, so a
// configuration typo doesn't quietly turn a "rebase" request into a "merge".
func (c *GiteaClient) MergePullRequest(ctx context.Context, owner, repo string, number int64, mergeMethod string) error {
	return c.MergePullRequestWithOptions(ctx, owner, repo, number, mergeMethod, MergeOptions{})
}

// MergePullRequestWithOptions is the customizable form of MergePullRequest. The
// title/message overrides flow through the same Gitea API fields.
//
// When opts.HeadSHA is non-empty, the request includes head_commit_id so
// Gitea rejects the merge if the PR head has moved since review (returns
// ErrHeadSHAMismatch). When Gitea reports the PR is already merged (405, or
// 409 with a "merged"-shaped body), this returns ErrAlreadyMerged so callers
// can errors.Is it and treat the call as a no-op success.
func (c *GiteaClient) MergePullRequestWithOptions(ctx context.Context, owner, repo string, number int64, mergeMethod string, opts MergeOptions) error {
	if !validGiteaMergeMethods[mergeMethod] {
		return fmt.Errorf("invalid Gitea merge method %q (allowed: merge, squash, rebase, rebase-merge)", mergeMethod)
	}

	mergeMessage := opts.MergeMessageField
	if mergeMessage == "" {
		mergeMessage = defaultMergeMessage
	}

	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/merge", sanitizePathSegment(owner), sanitizePathSegment(repo), number)

	// Use snake_case JSON keys to match Gitea's documented MergePullRequestOption
	// schema. Gitea's UnmarshalJSON tolerates the legacy PascalCase form too,
	// but newer Gitea releases drive everything through the snake_case path.
	payload := map[string]interface{}{
		"Do":                  mergeMethod,
		"MergeTitleField":     opts.MergeTitleField,
		"MergeMessageField":   mergeMessage,
		"merge_title_field":   opts.MergeTitleField,
		"merge_message_field": mergeMessage,
	}
	if opts.HeadSHA != "" {
		// Gitea's MergePullRequestForm field is "head_commit_id"; if a
		// future/older Gitea variant ignores it, the merge will still
		// proceed without the pin (logged as a warning at call site).
		payload["head_commit_id"] = opts.HeadSHA
	}

	req, err := c.buildRequest(ctx, "POST", path, payload)
	if err != nil {
		return fmt.Errorf("failed to merge pull request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to merge pull request: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	c.checkRateLimit(resp)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if readErr != nil {
		c.logger.WithError(readErr).Warn("Failed to read merge error response body")
	}
	excerpt := truncateForError(body)
	bodyLower := strings.ToLower(string(body))

	c.logger.WithFields(logrus.Fields{
		"status":       resp.StatusCode,
		"path":         path,
		"body_len":     len(body),
		"body_excerpt": excerpt,
	}).Error("Gitea merge request failed")

	// 405 Method Not Allowed is Gitea's "PR is already merged" signal.
	// Some deployments surface the same condition as 409 with a body that
	// mentions "already merged" or just "merged".
	switch resp.StatusCode {
	case http.StatusMethodNotAllowed:
		if strings.Contains(bodyLower, "already merged") ||
			strings.Contains(bodyLower, "has merged") ||
			strings.Contains(bodyLower, "is already merged") {
			return ErrAlreadyMerged
		}
		// 405 from /merge with a different body almost always still means
		// "we can't merge this right now" — most plausibly already-merged
		// in older Gitea versions. Surface it as the typed sentinel rather
		// than a generic error so callers don't have to keep growing their
		// own list.
		return fmt.Errorf("%w (status 405: %s)", ErrAlreadyMerged, excerpt)
	case http.StatusUnprocessableEntity:
		// Gitea's typical "this PR is closed" response. We require BOTH the
		// 422 status AND a recognised "closed"-shaped body — a bare 422 with
		// some other error (validation, etc.) must NOT be treated as benign,
		// because that would let a workflow silently succeed on a real
		// failure.
		if bodyMatchesPRClosed(bodyLower) {
			return fmt.Errorf("%w: %s", ErrPRClosed, excerpt)
		}
	case http.StatusConflict:
		if strings.Contains(bodyLower, "already merged") || strings.Contains(bodyLower, "has merged") {
			return ErrAlreadyMerged
		}
		// 409 + "head out of date" is Gitea's response when the supplied
		// head_commit_id does not match the PR's current head — i.e. our
		// SHA pin caught a force-push. Also match the variants other Gitea
		// versions / forks have used for the same condition.
		if bodyMatchesHeadMismatch(bodyLower) {
			return fmt.Errorf("%w: %s", ErrHeadSHAMismatch, excerpt)
		}
		// Some Gitea versions return 409 (not 422) when commenting/merging
		// against a closed PR; mirror the 422 detection here too. Order
		// matters — head-mismatch keywords are checked first because they
		// are more specific.
		if bodyMatchesPRClosed(bodyLower) {
			return fmt.Errorf("%w: %s", ErrPRClosed, excerpt)
		}
		// Unknown 409 body — emit a metric so operators can detect when
		// Gitea changes wording. We log a WARN with the truncated body and
		// fall through to the generic error path. This is the SHA-pin
		// drift detector: if Gitea ever changes its mismatch wording and
		// this counter ticks, the merge would have been allowed against
		// an unreviewed commit.
		c.recordUnknown409("POST", "/api/v1/repos/{owner}/{repo}/pulls/{number}/merge", body)
	}

	return fmt.Errorf("failed to merge pull request: API request failed with status %d: %s", resp.StatusCode, excerpt)
}

// SetCommitStatus sets the commit status (for CI integration)
func (c *GiteaClient) SetCommitStatus(ctx context.Context, owner, repo, sha string, status types.ReviewSummary) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/statuses/%s", sanitizePathSegment(owner), sanitizePathSegment(repo), sanitizePathSegment(sha))

	// Map GoGents recommendation to Gitea status
	var state string
	var description string

	switch status.Recommendation {
	case "MERGE":
		state = "success"
		description = fmt.Sprintf("✅ GoGents Review Passed (Score: %.2f)", status.OverallScore)
	case "REVIEW":
		state = "pending"
		description = fmt.Sprintf("⚠️ Manual Review Required (Score: %.2f)", status.OverallScore)
	case "BLOCK":
		state = "failure"
		description = fmt.Sprintf("❌ GoGents Review Failed (Score: %.2f)", status.OverallScore)
	default:
		state = "pending"
		description = "🤖 GoGents Review In Progress"
	}

	if status.CriticalIssues > 0 {
		description += fmt.Sprintf(" - %d critical issues", status.CriticalIssues)
	}

	payload := map[string]interface{}{
		"state":       state,
		"target_url":  "", // Could link to detailed results
		"description": description,
		"context":     "gogents/review",
	}

	err := c.makeRequest(ctx, "POST", path, payload, nil)
	if err != nil {
		return fmt.Errorf("failed to set commit status: %w", err)
	}

	return nil
}

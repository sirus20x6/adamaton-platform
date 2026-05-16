// /thearray/gogents/internal/gitea/comments.go
package gitea

import (
	"context"
	"fmt"

	"github.com/sirus20x6/adamaton-core/types"
)

// CreateComment adds a comment to a pull request. State-mutating endpoint —
// routes through doMutating so Gitea's "PR is closed" 422 is surfaced as
// ErrPRClosed for the activity layer to map to a typed pr_closed
// ApplicationError.
func (c *GiteaClient) CreateComment(ctx context.Context, owner, repo string, number int64, body string) (*Comment, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/issues/%d/comments", sanitizePathSegment(owner), sanitizePathSegment(repo), number)

	payload := map[string]string{
		"body": body,
	}

	var comment Comment
	err := c.doMutating(ctx, "POST", path, "/api/v1/repos/{owner}/{repo}/issues/{number}/comments", payload, &comment)
	if err != nil {
		return nil, fmt.Errorf("failed to create comment: %w", err)
	}

	return &comment, nil
}

// SubmitReview submits a complete review with overall status
func (c *GiteaClient) SubmitReview(ctx context.Context, owner, repo string, number int64, summary types.ReviewSummary) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", sanitizePathSegment(owner), sanitizePathSegment(repo), number)

	var event string
	var body string

	switch summary.Recommendation {
	case "MERGE":
		event = "APPROVE"
		body = fmt.Sprintf("🤖 **GoGents AI Review: APPROVED**\n\n✅ Overall Score: %.2f/1.0\n\n", summary.OverallScore)
		body += fmt.Sprintf("**Results**: %d agents passed, %d failed, %d warnings\n\n",
			summary.PassedAgents, summary.FailedAgents, summary.WarningAgents)
		body += "All critical checks passed. Safe to merge! 🚀"

	case "REVIEW":
		event = "REQUEST_CHANGES"
		body = fmt.Sprintf("🤖 **GoGents AI Review: CHANGES REQUESTED**\n\n⚠️ Overall Score: %.2f/1.0\n\n", summary.OverallScore)
		body += fmt.Sprintf("**Results**: %d agents passed, %d failed, %d warnings\n",
			summary.PassedAgents, summary.FailedAgents, summary.WarningAgents)
		if summary.HighIssues > 0 {
			body += fmt.Sprintf("**Issues**: %d high priority issues found\n\n", summary.HighIssues)
		}
		body += "Please address the issues identified by the AI agents before merging."

	case "BLOCK":
		event = "REQUEST_CHANGES"
		body = fmt.Sprintf("🤖 **GoGents AI Review: BLOCKED**\n\n❌ Overall Score: %.2f/1.0\n\n", summary.OverallScore)
		body += fmt.Sprintf("**Results**: %d agents passed, %d failed, %d warnings\n",
			summary.PassedAgents, summary.FailedAgents, summary.WarningAgents)
		if summary.CriticalIssues > 0 {
			body += fmt.Sprintf("**Critical Issues**: %d found - must be resolved\n\n", summary.CriticalIssues)
		}
		body += "🚫 **This PR is blocked due to critical issues. Manual review required.**"

	default:
		event = "COMMENT"
		body = "🤖 GoGents AI Review completed with unknown status."
	}

	payload := map[string]interface{}{
		"body":  body,
		"event": event,
	}

	err := c.doMutating(ctx, "POST", path, "/api/v1/repos/{owner}/{repo}/pulls/{number}/reviews", payload, nil)
	if err != nil {
		return fmt.Errorf("failed to submit review: %w", err)
	}

	return nil
}

// CreateReviewComment creates a review comment on a specific line. Routes
// through doMutating for the same reason as SubmitReview / CreateComment: a
// closed-PR error from Gitea must reach the workflow as ErrPRClosed.
func (c *GiteaClient) CreateReviewComment(ctx context.Context, owner, repo string, number int64, sha, filename string, line int, body string) error {
	path := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", sanitizePathSegment(owner), sanitizePathSegment(repo), number)

	payload := map[string]interface{}{
		"commit_id": sha,
		"body":      body, // Top-level body used as review summary; inline comment in comments array
		"event":     "COMMENT",
		"comments": []map[string]interface{}{
			{
				"path":     filename,
				"body":     body,
				"new_line": line,
			},
		},
	}

	err := c.doMutating(ctx, "POST", path, "/api/v1/repos/{owner}/{repo}/pulls/{number}/reviews", payload, nil)
	if err != nil {
		return fmt.Errorf("failed to create review comment: %w", err)
	}

	return nil
}

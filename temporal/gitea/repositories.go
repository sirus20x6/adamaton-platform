// /thearray/gogents/internal/gitea/repositories.go
package gitea

import (
	"context"
	"fmt"
)

// ListRepositories lists repositories the user has access to
func (c *GiteaClient) ListRepositories(ctx context.Context, page, limit int) ([]Repository, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = 10
	} else if limit > 50 {
		limit = 50
	}
	path := fmt.Sprintf("/api/v1/user/repos?page=%d&limit=%d", page, limit)

	var repos []Repository
	err := c.makeRequest(ctx, "GET", path, nil, &repos)
	if err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}

	return repos, nil
}

// GetRepository gets repository information
func (c *GiteaClient) GetRepository(ctx context.Context, owner, repo string) (*Repository, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/%s", sanitizePathSegment(owner), sanitizePathSegment(repo))

	var repository Repository
	err := c.makeRequest(ctx, "GET", path, nil, &repository)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository: %w", err)
	}

	return &repository, nil
}

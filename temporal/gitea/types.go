// /thearray/gogents/internal/gitea/types.go
package gitea

import "time"

// PRBranchInfo represents the head or base branch info in a Gitea PR
type PRBranchInfo struct {
	Label  string     `json:"label"`
	Ref    string     `json:"ref"`
	Sha    string     `json:"sha"`
	RepoID int64      `json:"repo_id"`
	Repo   Repository `json:"repo"`
}

// PullRequest represents a Gitea pull request
type PullRequest struct {
	ID        int64        `json:"id"`
	Number    int64        `json:"number"`
	Title     string       `json:"title"`
	Body      string       `json:"body"`
	State     string       `json:"state"` // "open", "closed", "merged"
	Merged    bool         `json:"merged"`
	MergeBase string       `json:"merge_base"`
	Head      PRBranchInfo `json:"head"`
	Base      PRBranchInfo `json:"base"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	User      User         `json:"user"`
	Assignees []User       `json:"assignees"`
	Reviewers []User       `json:"requested_reviewers"`
	// Mergeable is *bool because Gitea returns null when mergeability has not
	// yet been computed; a plain bool would collapse null and false into the
	// same "unmergeable" reading and we'd drive merge decisions off stale data.
	Mergeable *bool  `json:"mergeable"`
	DiffURL   string `json:"diff_url"`
	PatchURL  string `json:"patch_url"`
}

// Repository represents a Gitea repository
type Repository struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	FullName    string `json:"full_name"`
	Owner       User   `json:"owner"`
	Private     bool   `json:"private"`
	Description string `json:"description"`
	CloneURL    string `json:"clone_url"`
	SSHURL      string `json:"ssh_url"`
	HTMLURL     string `json:"html_url"`
}

// User represents a Gitea user
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// Comment represents a PR comment
type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
}

// WebhookPayload represents incoming webhook data
type WebhookPayload struct {
	Action      string      `json:"action"` // "opened", "closed", "reopened", "synchronize" (Gitea sends the imperative form, not "synchronized")
	Number      int64       `json:"number"`
	Repository  Repository  `json:"repository"`
	PullRequest PullRequest `json:"pull_request"`
	Sender      User        `json:"sender"`
}

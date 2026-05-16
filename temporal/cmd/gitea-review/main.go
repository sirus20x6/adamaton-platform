// /thearray/gogents/cmd/gitea_review.go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamaton-core/config"
	"github.com/sirus20x6/adamaton-platform/temporal/gitea"
	"github.com/sirus20x6/adamaton-core/metrics"
	"github.com/sirus20x6/adamaton-core/types"
	"github.com/sirus20x6/adamaton-platform/temporal/workflows"
)

var (
	prNumber  int
	repoOwner string
	repoName  string
	reviewID  string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "gitea-review",
	Short: "GoGents Gitea Review Tool",
	Long: `GoGents Gitea Review Tool provides secure, self-hosted code review using your Gitea instance.

Features:
• Complete AI-powered review using 12 specialized agents
• Self-hosted - all data stays within your infrastructure
• GitHub-like PR review experience with Gitea
• Automatic merge capabilities based on review results
• Webhook integration for automated reviews`,
	RunE: runGiteaReview,
}

func init() {
	rootCmd.Flags().IntVarP(&prNumber, "pr", "p", 0, "Pull request number (required)")
	rootCmd.Flags().StringVarP(&repoOwner, "owner", "o", "", "Repository owner (required)")
	rootCmd.Flags().StringVarP(&repoName, "repo", "r", "", "Repository name (required)")
	rootCmd.Flags().StringVarP(&reviewID, "review-id", "i", "", "Custom review ID (optional)")

	_ = rootCmd.MarkFlagRequired("pr")
	_ = rootCmd.MarkFlagRequired("owner")
	_ = rootCmd.MarkFlagRequired("repo")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	return rootCmd.Execute()
}

func runGiteaReview(cmd *cobra.Command, args []string) error {
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
		// Fall back to Info on a parse error rather than failing the CLI —
		// a typo'd LOG_LEVEL shouldn't prevent a review run.
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	if cfg.Logging.Format == "json" {
		logger.SetFormatter(&logrus.JSONFormatter{})
	}

	logger.WithFields(logrus.Fields{
		"pr":    prNumber,
		"owner": repoOwner,
		"repo":  repoName,
	}).Info("Starting Gitea PR review")

	// Validate Gitea configuration
	if cfg.Gitea.BaseURL == "" || cfg.Gitea.Token == "" {
		return fmt.Errorf("Gitea configuration missing. Please set GITEA_BASE_URL and GITEA_TOKEN")
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

	// Fetch the PR head SHA before scheduling so the merge activity can
	// pin the merge against the exact commit observed at review start.
	// Without this, the workflow logs "merging without SHA pin —
	// vulnerable to force-push race" on every CLI-triggered run.
	giteaClient := gitea.NewGiteaClient(cfg.Gitea, logger)
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pr, err := giteaClient.GetPullRequest(fetchCtx, repoOwner, repoName, int64(prNumber))
	fetchCancel()
	if err != nil {
		return fmt.Errorf("failed to fetch PR from Gitea: %w", err)
	}
	headSHA := pr.Head.Sha

	// Generate workflow ID
	workflowID := fmt.Sprintf("gitea-pr-review-%s-%s-%d", repoOwner, repoName, prNumber)
	if reviewID != "" {
		workflowID = fmt.Sprintf("gitea-pr-review-%s", reviewID)
	}

	// Prepare workflow arguments. Populate per-agent enable flags from the
	// loaded config so the CLI honors the same agent toggles as the
	// apiserver and webhook trigger paths. HeadSHA is plumbed through so
	// the merge activity can pin against the exact commit observed at
	// review start (closes the force-push race; mirrors webhook + apiserver).
	workflowArgs := workflows.PRReviewArgs{
		PRNumber:    prNumber,
		RepoOwner:   repoOwner,
		RepoName:    repoName,
		HeadSHA:     headSHA,
		MergeMethod: cfg.Workflow.MergeMethod,
		Agents: workflows.AgentEnabledFlags{
			Configured: true,
			Enabled: map[string]bool{
				string(types.AgentSecurity):        cfg.Agents.Security.Enabled,
				string(types.AgentPerformance):     cfg.Agents.Performance.Enabled,
				string(types.AgentConst):           cfg.Agents.Const.Enabled,
				string(types.AgentDocumentation):   cfg.Agents.Documentation.Enabled,
				string(types.AgentTesting):         cfg.Agents.Testing.Enabled,
				string(types.AgentArchitecture):    cfg.Agents.Architecture.Enabled,
				string(types.AgentAccessibility):   cfg.Agents.Accessibility.Enabled,
				string(types.AgentCompliance):      cfg.Agents.Compliance.Enabled,
				string(types.AgentDependencies):    cfg.Agents.Dependencies.Enabled,
				string(types.AgentStyle):           cfg.Agents.Style.Enabled,
				string(types.AgentMaintainability): cfg.Agents.Maintainability.Enabled,
				string(types.AgentBusinessLogic):   cfg.Agents.BusinessLogic.Enabled,
			},
		},
	}

	logger.WithField("workflowId", workflowID).Info("Starting Gitea review workflow")

	// Root context that's cancelled by SIGINT/SIGTERM so the user can ^C
	// out of a long workflow wait without orphaning the local process.
	// signal.NotifyContext is the right idiom here — it correctly
	// reset()s on stop() and avoids the deferred-channel-leak you'd get
	// from rolling your own.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the workflow (timeout for the scheduling RPC, not execution)
	execCtx, execCancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer execCancel()
	we, err := temporalClient.ExecuteWorkflow(
		execCtx,
		client.StartWorkflowOptions{
			ID:        workflowID,
			TaskQueue: cfg.Temporal.TaskQueue,
		},
		workflows.GiteaPRReviewWorkflow,
		workflowArgs,
	)
	if err != nil {
		return fmt.Errorf("failed to start workflow: %w", err)
	}

	metrics.WorkflowsStarted.WithLabelValues("GiteaPRReviewWorkflow", "cli").Inc()

	logger.WithFields(logrus.Fields{
		"workflowId": we.GetID(),
		"runId":      we.GetRunID(),
	}).Info("Gitea review workflow started successfully")

	// Wait for workflow completion (with timeout to prevent indefinite hang).
	// The wait context derives from rootCtx so SIGINT cancels both the wait
	// and the workflow — the local process exits cleanly while Temporal
	// continues the run on the worker side. This is the right tradeoff:
	// killing the workflow because the operator hit Ctrl-C would lose
	// review state.
	waitStart := time.Now()
	waitCtx, waitCancel := context.WithTimeout(rootCtx, 10*time.Minute)
	defer waitCancel()
	if err := we.Get(waitCtx, nil); err != nil {
		metrics.ObserveWorkflowDuration("GiteaPRReviewWorkflow", "error", time.Since(waitStart))
		return fmt.Errorf("workflow failed: %w", err)
	}
	metrics.ObserveWorkflowDuration("GiteaPRReviewWorkflow", "success", time.Since(waitStart))

	logger.Info("Gitea PR review completed successfully")
	fmt.Printf("Gitea PR review completed for %s/%s#%d\n", repoOwner, repoName, prNumber)
	fmt.Printf("Check your Gitea instance for the review results\n")
	return nil
}

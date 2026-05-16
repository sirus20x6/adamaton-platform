// /thearray/gogents/cmd/start_workflow.go - CLI to start PR review workflow
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sirus20x6/adamomaton-core/metrics"
	"github.com/sirus20x6/adamomaton-platform/temporal/workflows"
	"go.temporal.io/sdk/client"
)

func main() {
	if err := run(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	// Command line flags
	var (
		prNumber    = flag.Int("pr", 0, "Pull request number (required)")
		owner       = flag.String("owner", "", "Repository owner (required)")
		repo        = flag.String("repo", "", "Repository name (required)")
		temporal    = flag.String("temporal", "localhost:7233", "Temporal server address")
		taskQueue   = flag.String("queue", "pr-review", "Temporal task queue")
		mergeMethod = flag.String("merge-method", "squash", "Merge method: merge, squash, or rebase")
	)
	flag.Parse()

	// Validate required arguments
	if *prNumber == 0 || *owner == "" || *repo == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s --pr <number> --owner <owner> --repo <repo>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nRequired arguments:\n")
		fmt.Fprintf(os.Stderr, "  --pr     Pull request number\n")
		fmt.Fprintf(os.Stderr, "  --owner  Repository owner/organization\n")
		fmt.Fprintf(os.Stderr, "  --repo   Repository name\n")
		fmt.Fprintf(os.Stderr, "\nOptional arguments:\n")
		fmt.Fprintf(os.Stderr, "  --temporal      Temporal server address (default: localhost:7233)\n")
		fmt.Fprintf(os.Stderr, "  --queue         Task queue name (default: pr-review)\n")
		fmt.Fprintf(os.Stderr, "  --merge-method  Merge method: merge, squash, or rebase (default: squash)\n")
		fmt.Fprintf(os.Stderr, "\nExample:\n")
		fmt.Fprintf(os.Stderr, "  %s --pr 42 --owner myorg --repo myrepo\n", os.Args[0])
		return fmt.Errorf("missing required arguments")
	}

	// Create Temporal client
	c, err := client.Dial(client.Options{
		HostPort: *temporal,
	})
	if err != nil {
		return fmt.Errorf("unable to create Temporal client: %w", err)
	}
	defer c.Close()

	// Generate unique workflow ID
	workflowID := fmt.Sprintf("pr-review-%s-%s-%d-%s", *owner, *repo, *prNumber, uuid.New().String()[:8])

	// Prepare workflow arguments. start-workflow is a manual trigger and has
	// no config loader — leave Agents.Configured=false (the zero value) so
	// the workflow's historical default kicks in and every agent runs. If a
	// future version adds --enable-* flags, populate Configured=true and
	// the booleans here.
	args := workflows.PRReviewArgs{
		PRNumber:    *prNumber,
		RepoOwner:   *owner,
		RepoName:    *repo,
		MergeMethod: *mergeMethod,
	}

	// Start the workflow
	log.Printf("Starting PR review workflow...")
	log.Printf("  PR: %s/%s#%d", *owner, *repo, *prNumber)
	log.Printf("  Workflow ID: %s", workflowID)
	log.Printf("  Task Queue: %s", *taskQueue)
	log.Printf("  Temporal Server: %s", *temporal)

	// Honor SIGINT/SIGTERM during the scheduling RPC so a stuck Temporal
	// frontend doesn't hold the process hostage when the operator ^C's.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	execCtx, execCancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer execCancel()
	we, err := c.ExecuteWorkflow(
		execCtx,
		client.StartWorkflowOptions{
			ID:        workflowID,
			TaskQueue: *taskQueue,
		},
		workflows.PRReviewWorkflow,
		args,
	)
	if err != nil {
		return fmt.Errorf("unable to execute workflow: %w", err)
	}

	metrics.WorkflowsStarted.WithLabelValues("PRReviewWorkflow", "cli").Inc()

	log.Printf("Started workflow successfully")
	log.Printf("  Workflow ID: %s", we.GetID())
	log.Printf("  Run ID: %s", we.GetRunID())

	log.Println("Workflow started. Use Temporal Web UI to monitor progress.")
	log.Printf("  Web UI: http://%s:8088", strings.Split(*temporal, ":")[0])
	return nil
}

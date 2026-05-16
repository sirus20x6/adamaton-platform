// /thearray/gogents/cmd/worker-health.go - Health check utility for enhanced PR review worker
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sirus20x6/adamaton-core/envutil"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

func main() {
	helpFlag := flag.Bool("help", false, "Show help and exit")
	requireMCP := flag.Bool("require-mcp", false, "Treat MCP server unreachable as a failure")
	requireVLLM := flag.Bool("require-vllm", false, "Treat vLLM endpoint unreachable as a failure")
	flag.Parse()

	if *helpFlag {
		printUsage()
		return
	}

	// Get configuration
	temporalAddress := envutil.GetEnvOrDefault("TEMPORAL_ADDRESS", "localhost:7233")
	namespace := envutil.GetEnvOrDefault("TEMPORAL_NAMESPACE", "default")
	taskQueue := envutil.GetEnvOrDefault("TEMPORAL_TASK_QUEUE", "pr-review")
	vllmEndpoint := envutil.GetEnvOrDefault("VLLM_ENDPOINT", "http://vllm.local:8000")
	mcpServerURL := envutil.GetEnvOrDefault("MCP_SERVER_URL", "http://localhost:3000")
	githubToken := os.Getenv("GITHUB_TOKEN")
	giteaToken := os.Getenv("GITEA_TOKEN")

	fmt.Println("Enhanced PR Review Worker Health Check")
	fmt.Println("=========================================")

	exitCode := 0

	// Check Temporal connection
	fmt.Printf("Checking Temporal connection to %s...\n", temporalAddress)
	c, err := client.Dial(client.Options{
		HostPort:  temporalAddress,
		Namespace: namespace,
	})
	if err != nil {
		fmt.Printf("FAIL: Failed to connect to Temporal: %v\n", err)
		exitCode = 1
	} else {
		defer c.Close()
		fmt.Println("OK: Temporal connection successful")

		fmt.Printf("Namespace '%s' connected\n", namespace)

		// Check task queue workers
		fmt.Printf("Checking workers on task queue '%s'...\n", taskQueue)
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()

		resp, err := c.DescribeTaskQueue(ctx2, taskQueue, enums.TASK_QUEUE_TYPE_WORKFLOW)
		if err != nil {
			fmt.Printf("FAIL: Task queue check failed: %v\n", err)
			exitCode = 1
		} else {
			if len(resp.Pollers) > 0 {
				fmt.Printf("OK: Found %d active workers\n", len(resp.Pollers))
				for i, poller := range resp.Pollers {
					fmt.Printf("  Worker %d: %s (last seen: %v)\n", i+1, poller.Identity, poller.LastAccessTime)
				}
			} else {
				fmt.Println("WARN: No active workers found")
				exitCode = 1
			}
		}
	}

	// Check forge token (GitHub or Gitea — project supports either)
	fmt.Println("Checking forge token (GitHub or Gitea)...")
	if githubToken == "" && giteaToken == "" {
		fmt.Println("FAIL: Neither GITHUB_TOKEN nor GITEA_TOKEN environment variable is set")
		exitCode = 1
	} else {
		if githubToken != "" {
			fmt.Printf("OK: GitHub token present (%s)\n", envutil.MaskToken(githubToken))
		}
		if giteaToken != "" {
			fmt.Printf("OK: Gitea token present (%s)\n", envutil.MaskToken(giteaToken))
		}
	}

	// Check vLLM endpoint
	vllmHealthURL := strings.TrimSuffix(vllmEndpoint, "/") + "/health"
	fmt.Printf("Checking vLLM endpoint at %s...\n", vllmHealthURL)
	if checkHTTPEndpoint(vllmHealthURL, 5*time.Second) {
		fmt.Println("OK: vLLM endpoint accessible")
	} else {
		if *requireVLLM {
			fmt.Println("FAIL: vLLM endpoint not accessible (--require-vllm)")
			exitCode = 1
		} else {
			fmt.Println("WARN: vLLM endpoint not accessible (non-fatal; pass --require-vllm to enforce)")
		}
	}

	// Check MCP server
	fmt.Printf("Checking MCP server at %s...\n", mcpServerURL)
	if checkHTTPEndpoint(mcpServerURL, 5*time.Second) {
		fmt.Println("OK: MCP server accessible")
	} else {
		if *requireMCP {
			fmt.Println("FAIL: MCP server not accessible (--require-mcp)")
			exitCode = 1
		} else {
			fmt.Println("WARN: MCP server not accessible (non-fatal; pass --require-mcp to enforce)")
		}
	}

	// Summary
	fmt.Println("\nHealth Check Summary")
	fmt.Println("======================")
	if exitCode == 0 {
		fmt.Println("OK: All required systems operational")
		fmt.Println("Enhanced PR Review Worker is ready to process requests")
	} else {
		fmt.Println("FAIL: Some required systems have issues")
		fmt.Println("Please check the failed components above")
	}

	// Display configuration
	fmt.Println("\nCurrent Configuration")
	fmt.Println("========================")
	fmt.Printf("Temporal Address: %s\n", temporalAddress)
	fmt.Printf("Namespace: %s\n", namespace)
	fmt.Printf("Task Queue: %s\n", taskQueue)
	fmt.Printf("vLLM Endpoint: %s\n", vllmEndpoint)
	fmt.Printf("MCP Server: %s\n", mcpServerURL)
	fmt.Printf("GitHub Token: %s\n", envutil.MaskToken(githubToken))
	fmt.Printf("Gitea Token: %s\n", envutil.MaskToken(giteaToken))

	os.Exit(exitCode)
}

func checkHTTPEndpoint(url string, timeout time.Duration) bool {
	client := &http.Client{
		Timeout: timeout,
	}

	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// Accept any 2xx or 3xx status code as "accessible"
	return resp.StatusCode < 400
}

func printUsage() {
	fmt.Println("Enhanced PR Review Worker Health Check")
	fmt.Println("")
	fmt.Println("Usage: worker-health [--help] [--require-mcp] [--require-vllm]")
	fmt.Println("")
	fmt.Println("This utility checks the health of all components required for the")
	fmt.Println("enhanced PR review worker to function properly:")
	fmt.Println("")
	fmt.Println("  - Temporal server connectivity")
	fmt.Println("  - Namespace accessibility")
	fmt.Println("  - Active worker presence")
	fmt.Println("  - Forge token configuration (GitHub or Gitea)")
	fmt.Println("  - vLLM endpoint accessibility (warn-only by default)")
	fmt.Println("  - MCP server accessibility (warn-only by default)")
	fmt.Println("")
	fmt.Println("Flags:")
	fmt.Println("  --help          Show this help and exit")
	fmt.Println("  --require-mcp   Treat MCP unreachable as a failure (default: warn)")
	fmt.Println("  --require-vllm  Treat vLLM unreachable as a failure (default: warn)")
	fmt.Println("")
	fmt.Println("Environment Variables:")
	fmt.Println("  TEMPORAL_ADDRESS    - Temporal server address (default: localhost:7233)")
	fmt.Println("  TEMPORAL_NAMESPACE  - Temporal namespace (default: default)")
	fmt.Println("  TEMPORAL_TASK_QUEUE - Task queue name (default: pr-review)")
	fmt.Println("  VLLM_ENDPOINT       - vLLM server endpoint")
	fmt.Println("  MCP_SERVER_URL      - MCP server URL")
	fmt.Println("  GITHUB_TOKEN        - GitHub access token (one of GITHUB_TOKEN/GITEA_TOKEN required)")
	fmt.Println("  GITEA_TOKEN         - Gitea access token (one of GITHUB_TOKEN/GITEA_TOKEN required)")
	fmt.Println("")
	fmt.Println("Exit Codes:")
	fmt.Println("  0 - All required systems healthy")
	fmt.Println("  1 - One or more required systems have issues")
}

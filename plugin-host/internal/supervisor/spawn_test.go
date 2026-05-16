// End-to-end spawn test for the supervisor. Builds the noop reference
// plugin binary at test setup, drops a manifest pointing at it under a
// scratch dir, then asserts the full wire: Hello → ListCollections →
// Sync stream → Shutdown, with socket cleanup verified after.
package supervisor

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
)

// stubHost is a do-nothing HostServer that lets the supervisor wire the
// per-plugin Host gRPC server without dragging in postgres.
type stubHost struct {
	pluginv1.UnimplementedHostServer
}

// buildNoop compiles the noop-plugin binary into a tmpdir and returns
// its path. The build cost is paid once per `go test` invocation.
func buildNoop(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "noop-plugin")
	cmd := exec.Command("go", "build", "-o", bin,
		"github.com/sirus20x6/adamaton-platform/plugin-host/internal/noopplugin/cmd/noop-plugin")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build noop-plugin: %v", err)
	}
	return bin
}

func TestSpawnNoopEndToEnd(t *testing.T) {
	bin := buildNoop(t)
	sockDir := t.TempDir()

	m := &manifest.Manifest{
		ID:           "noop",
		Name:         "Noop",
		Description:  "test fixture",
		Version:      "0.1.0",
		Category:     "importer",
		Capabilities: []string{"importer.list_collections", "importer.sync"},
		Command:      []string{bin},
		Transport:    "grpc-unix",
		Supervisor: manifest.SupervisorOpts{
			IdleTimeoutSeconds: 60,
			MaxRestartPerMin:   3,
		},
	}

	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(io.Discard) // chatty otherwise; flip to Stderr to debug
	sup := New(Options{
		Logger:     logger,
		Manifests:  map[string]*manifest.Manifest{"noop": m},
		HostServer: &stubHost{},
		SocketDir:  sockDir,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, mret, err := sup.EnsureRunning(ctx, "noop")
	if err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if mret.ID != "noop" {
		t.Errorf("manifest id = %q, want noop", mret.ID)
	}

	// Hello already happened inside spawn; verify Ping works.
	if _, err := client.Ping(ctx, &pluginv1.PingRequest{}); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// ListCollections.
	lc, err := client.ListCollections(ctx, &pluginv1.ListCollectionsRequest{})
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(lc.Collections) != 1 || lc.Collections[0].Id != "demo" {
		t.Errorf("ListCollections = %+v, want one collection id=demo", lc.Collections)
	}

	// Sync stream — expect one item then one summary.
	stream, err := client.Sync(ctx, &pluginv1.SyncRequest{RunId: "test-run"})
	if err != nil {
		t.Fatalf("Sync open: %v", err)
	}
	var sawItem, sawSummary bool
	for {
		evt, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Sync recv: %v", err)
		}
		switch e := evt.Event.(type) {
		case *pluginv1.SyncEvent_Item:
			sawItem = true
			if e.Item.ExternalId != "noop-item-1" {
				t.Errorf("item external_id = %q, want noop-item-1", e.Item.ExternalId)
			}
		case *pluginv1.SyncEvent_Summary:
			sawSummary = true
			if e.Summary.NewItems != 1 {
				t.Errorf("summary new_items = %d, want 1", e.Summary.NewItems)
			}
		}
	}
	if !sawItem || !sawSummary {
		t.Errorf("missing events: item=%v summary=%v", sawItem, sawSummary)
	}

	// Idempotent EnsureRunning -- second call returns the cached client.
	client2, _, err := sup.EnsureRunning(ctx, "noop")
	if err != nil {
		t.Fatalf("EnsureRunning second call: %v", err)
	}
	if client2 != client {
		t.Errorf("EnsureRunning returned a fresh client on second call")
	}

	// Shutdown tears the process down. Socket files should be gone.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := sup.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	entries, err := os.ReadDir(sockDir)
	if err != nil {
		t.Fatalf("read sockDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("leftover socket after shutdown: %s", e.Name())
	}
}

func TestEnsureRunningUnknownPluginErrors(t *testing.T) {
	sup := New(Options{
		Logger:     logrus.New(),
		Manifests:  map[string]*manifest.Manifest{},
		HostServer: &stubHost{},
		SocketDir:  t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, _, err := sup.EnsureRunning(ctx, "ghost")
	if err == nil {
		t.Fatal("expected error for unknown plugin id")
	}
}

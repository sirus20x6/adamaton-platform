// Command noop-plugin is the reference test plugin for plugin-host. It
// implements just enough of pluginv1.PluginServer to validate the wire
// end-to-end: Hello, Ping, ListCollections (one fake collection), Sync
// (one fake item then summary), Shutdown.
//
// Run by the supervisor integration test via a manifest pointing at this
// binary. Reads DR_PLUGIN_SOCK + DR_HOST_SOCK from env at startup.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	pluginv1 "github.com/sirus20x6/adamomaton-platform/plugin-host/gen/go/dr/plugin/v1"
)

type server struct {
	pluginv1.UnimplementedPluginServer
	shutdown chan struct{}
}

func (s *server) Hello(ctx context.Context, req *pluginv1.HelloRequest) (*pluginv1.HelloResponse, error) {
	return &pluginv1.HelloResponse{
		PluginVersion: "noop-0.1.0",
		Capabilities: []string{
			"importer.list_collections",
			"importer.sync",
		},
	}, nil
}

func (s *server) Ping(ctx context.Context, req *pluginv1.PingRequest) (*pluginv1.PingResponse, error) {
	return &pluginv1.PingResponse{}, nil
}

func (s *server) Shutdown(ctx context.Context, req *pluginv1.ShutdownRequest) (*pluginv1.ShutdownResponse, error) {
	go func() { close(s.shutdown) }()
	return &pluginv1.ShutdownResponse{}, nil
}

func (s *server) ListCollections(ctx context.Context, req *pluginv1.ListCollectionsRequest) (*pluginv1.ListCollectionsResponse, error) {
	return &pluginv1.ListCollectionsResponse{
		Collections: []*pluginv1.Collection{
			{Id: "demo", Name: "Demo Collection", ItemCount: 1},
		},
	}, nil
}

func (s *server) Sync(req *pluginv1.SyncRequest, stream pluginv1.Plugin_SyncServer) error {
	// One item, then a summary. Tests use this to exercise stream
	// semantics + host's per-event sink.
	if err := stream.Send(&pluginv1.SyncEvent{
		Event: &pluginv1.SyncEvent_Item{
			Item: &pluginv1.PluginItem{
				PluginId:    "noop",
				ExternalId:  "noop-item-1",
				Title:       "Noop Item",
				ExternalUrl: "https://example.invalid/noop/1",
			},
		},
	}); err != nil {
		return err
	}
	return stream.Send(&pluginv1.SyncEvent{
		Event: &pluginv1.SyncEvent_Summary{
			Summary: &pluginv1.RunSummary{Seen: 1, NewItems: 1, Fetched: 1},
		},
	})
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "noop-plugin: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	sock := os.Getenv("DR_PLUGIN_SOCK")
	if sock == "" {
		return fmt.Errorf("DR_PLUGIN_SOCK not set")
	}
	_ = os.Remove(sock) // stale from a prior crash

	lis, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	defer os.Remove(sock)

	s := &server{shutdown: make(chan struct{})}
	srv := grpc.NewServer()
	pluginv1.RegisterPluginServer(srv, s)

	go func() {
		_ = srv.Serve(lis)
	}()

	// Exit on either an explicit Shutdown RPC or a signal from the
	// supervisor's process-group SIGKILL fallback.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-s.shutdown:
	case <-sigCh:
	}
	srv.GracefulStop()
	return nil
}

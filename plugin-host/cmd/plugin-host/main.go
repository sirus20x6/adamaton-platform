// Command plugin-host supervises deepresearch plugin subprocesses and
// exposes /platform/plugins, /platform/importers, /platform/search, and
// /platform/marketplace as the frontend's single entry point.
//
// Plugins are isolated processes that speak gRPC over a Unix socket pair
// in PH_SOCKET_DIR: one socket for host->plugin calls (Plugin service)
// and one for plugin->host calls (Host service). The host loads manifests
// from PH_MANIFEST_DIR at boot and spawns each plugin's command on
// first use, idle-reaping per the manifest's supervisor knobs.
//
// Configuration is environment-driven:
//
//	PH_PORT                  — HTTP listener (default 7375)
//	PH_DATABASE_URL          — postgres DSN (required)
//	PH_MANIFEST_DIR          — manifest scan root (default /etc/deepresearch/plugins)
//	PH_PLUGIN_PAYLOADS_DIR   — bind-mounted plugin code (default /opt/dr-plugins)
//	PH_SOCKET_DIR            — socket pair location (default /run/dr-plugins)
//	PH_STAGE_DIR             — container-local ephemeral staging root
//	                          (default /run/ph-stage). Plugins write blobs
//	                          here; the host commits them to the shared
//	                          Garage blob store (bucket dr-uploads) and
//	                          deletes the local copy.
//	PLUGIN_HOST_SECRET_KEY   — base64 32-byte AES-GCM key (required, no default)
//	PH_LOG_LEVEL             — logrus level (default "info")
//	PH_LOG_FORMAT            — "text" or "json" (default "text")
//
// Blob storage (shared Garage / S3) is configured via the canonical
// BLOBSTORE_* variables read by core/blobstore.ConfigFromEnv; see that
// package for the full list. When BLOBSTORE_ENDPOINT is unset the host
// still boots, but staging operations fail soft (503 / commit no-op).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/sirus20x6/adamaton-core/blobstore"
	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/hostserver"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/persist"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/runner"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/secrets"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/stage"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/supervisor"
	"github.com/sirus20x6/adamaton-platform/plugin-host/routes"
	"github.com/sirus20x6/adamaton-platform/plugin-host/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "plugin-host: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := newLogger()

	port := envOr("PH_PORT", "7375")
	dsn := os.Getenv("PH_DATABASE_URL")
	if dsn == "" {
		return errors.New("PH_DATABASE_URL is required")
	}
	manifestDir := envOr("PH_MANIFEST_DIR", "/etc/deepresearch/plugins")
	payloadsDir := envOr("PH_PLUGIN_PAYLOADS_DIR", "/opt/dr-plugins")
	socketDir := envOr("PH_SOCKET_DIR", "/run/dr-plugins")
	stageDir := envOr("PH_STAGE_DIR", "/run/ph-stage")
	secretKey := os.Getenv("PLUGIN_HOST_SECRET_KEY")
	if secretKey == "" {
		return errors.New("PLUGIN_HOST_SECRET_KEY is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres pool ready")

	sec, err := secrets.New(pool, secretKey)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}

	// Load every manifest under PH_MANIFEST_DIR. Bad ones get logged and
	// skipped -- one rotten manifest must not stop the host from booting.
	manifests, manifestErrs := manifest.LoadAll(manifestDir)
	for path, mErr := range manifestErrs {
		logger.WithError(mErr).WithField("path", path).Warn("manifest load failed")
	}
	logger.WithFields(logrus.Fields{
		"dir":    manifestDir,
		"loaded": len(manifests),
		"errors": len(manifestErrs),
	}).Info("manifests ready")

	// Ensure the socket dir exists before the supervisor tries to bind.
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("mkdir stage dir: %w", err)
	}

	// Shared Garage / S3 blob store (bucket dr-uploads). Built from the
	// canonical BLOBSTORE_* env. When BLOBSTORE_ENDPOINT is unset we fail
	// soft: the host still boots, but the stager has no durable backend and
	// staging operations return 503 / commit no-ops. Any other config or
	// connectivity error is fatal -- a misconfigured store should not boot
	// silently and lose blobs.
	var blobs blobstore.Backend
	if os.Getenv("BLOBSTORE_ENDPOINT") != "" {
		s3, err := blobstore.NewS3(ctx, blobstore.ConfigFromEnv("dr-uploads"))
		if err != nil {
			return fmt.Errorf("blobstore: %w", err)
		}
		if err := s3.EnsureBucket(ctx); err != nil {
			return fmt.Errorf("blobstore ensure bucket: %w", err)
		}
		blobs = s3
		logger.Info("blob store ready (bucket dr-uploads)")
	} else {
		logger.Warn("BLOBSTORE_ENDPOINT unset; staging operations will fail soft (503)")
	}

	stager := stage.New(stageDir, blobs)
	store := persist.New(pool)
	hostSrv := hostserver.New(pool, logger, store, sec, stager)

	sup := supervisor.New(supervisor.Options{
		Logger:      logger,
		Pool:        pool,
		Secrets:     sec,
		Manifests:   manifests,
		HostServer:  hostSrv,
		SocketDir:   socketDir,
		PayloadsDir: payloadsDir,
	})

	// Host gRPC server on a single shared socket. The per-plugin host
	// sockets the supervisor mints are routed through the supervisor's
	// own listener once spawn-logic lands; today the shared socket is
	// what the test harness uses to exercise the RPCs.
	hostSockPath := filepath.Join(socketDir, "host.sock")
	_ = os.Remove(hostSockPath) // stale from a prior crash
	hostLis, err := net.Listen("unix", hostSockPath)
	if err != nil {
		return fmt.Errorf("listen host sock: %w", err)
	}
	grpcSrv := grpc.NewServer()
	pluginv1.RegisterHostServer(grpcSrv, hostSrv)
	go func() {
		logger.WithField("sock", hostSockPath).Info("host gRPC listening")
		if err := grpcSrv.Serve(hostLis); err != nil {
			logger.WithError(err).Warn("host gRPC server exited")
		}
	}()

	go func() {
		if err := sup.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.WithError(err).Warn("supervisor exited")
		}
	}()

	// Run-loop: drains platform.plugin_runs WHERE status='pending', spawns
	// the plugin via supervisor, streams Plugin.Sync events, persists
	// items, finalises status. One worker is enough for the Pi -- the
	// PickPendingRun uses SELECT FOR UPDATE SKIP LOCKED so scaling is a
	// configuration change, not a code change.
	runWorker := runner.New(runner.Options{
		Logger:     logger,
		Persist:    store,
		Supervisor: sup,
	})
	go func() {
		if err := runWorker.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.WithError(err).Warn("runner exited")
		}
	}()

	metricsReg := prometheus.NewRegistry()
	metricsReg.MustRegister(prometheus.NewGoCollector())
	metricsReg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	r := mux.NewRouter()
	r.HandleFunc("/health", healthHandler).Methods(http.MethodGet)
	r.HandleFunc("/ready", readyHandler(pool)).Methods(http.MethodGet)
	r.Handle("/metrics", promhttp.HandlerFor(metricsReg, promhttp.HandlerOpts{Registry: metricsReg})).Methods(http.MethodGet)

	routes.RegisterPlugins(r, pool, manifests, sec, logger)
	routes.RegisterImporters(r, manifests)
	routes.RegisterMarketplace(r, manifests)
	routes.RegisterSearch(r, sup, manifests, logger)
	routes.RegisterCompatZotero(r, pool, sec, stager, logger)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.WithField("addr", srv.Addr).Info("plugin-host listening")
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer shutCancel()

		// Stop accepting HTTP first so callers get a clean drain window.
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.WithError(err).Warn("http shutdown timed out")
		}
		// Then walk plugins -- gRPC Shutdown RPC, then SIGKILL on grace.
		if err := sup.Shutdown(shutCtx); err != nil {
			logger.WithError(err).Warn("supervisor shutdown")
		}
		// Finally the host gRPC server. GracefulStop blocks on in-flight
		// callbacks; the context above bounds it implicitly via Stop().
		stopped := make(chan struct{})
		go func() { grpcSrv.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-shutCtx.Done():
			grpcSrv.Stop()
		}
		logger.Info("plugin-host stopped")
		return nil
	}
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func readyHandler(pool interface {
	Ping(context.Context) error
}) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func newLogger() *logrus.Logger {
	lg := logrus.New()
	if lvl, err := logrus.ParseLevel(envOr("PH_LOG_LEVEL", "info")); err == nil {
		lg.SetLevel(lvl)
	}
	if envOr("PH_LOG_FORMAT", "text") == "json" {
		lg.SetFormatter(&logrus.JSONFormatter{})
	}
	return lg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

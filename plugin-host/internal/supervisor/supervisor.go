// Package supervisor owns the plugin subprocess lifecycle: spawn on
// demand from a manifest, hand callers a gRPC client, idle-kill after
// the manifest's timeout, restart on crash with a per-minute cap.
//
// Architecture (one entry per running plugin):
//
//	plugin_sock = <socket_dir>/<id>.<pid>.plugin.sock  — plugin listens, host dials
//	host_sock   = <socket_dir>/<id>.<pid>.host.sock    — host listens, plugin dials
//
// Per-spawn the supervisor stands up its own gRPC server on host_sock so
// each conn's identity is implicit in which socket the plugin connected
// through. A UnaryServerInterceptor stamps the plugin id into the RPC
// context; the central hostserver.Server reads it from the context value
// the interceptor injected.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginv1 "github.com/sirus20x6/adamaton-platform/plugin-host/gen/go/dr/plugin/v1"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/manifest"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/phmetrics"
	"github.com/sirus20x6/adamaton-platform/plugin-host/internal/secrets"
)

// PluginCtxKey is the context-value key the per-plugin gRPC interceptor
// uses to stamp the plugin id into Host RPCs. hostserver reads it via
// PluginIDFromContext. Exported so hostserver can use the same key.
type PluginCtxKey struct{}

// PluginIDFromContext returns the plugin id the supervisor's per-conn
// interceptor stamped into ctx, or "" if the call didn't come through
// a supervisor-minted listener.
func PluginIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(PluginCtxKey{}).(string)
	return id
}

// Options is the wide constructor so cmd/plugin-host wires deps in one
// shot.
type Options struct {
	Logger      *logrus.Logger
	Pool        *pgxpool.Pool
	Secrets     *secrets.Manager
	Manifests   map[string]*manifest.Manifest
	HostServer  pluginv1.HostServer
	SocketDir   string
	PayloadsDir string
}

// Supervisor is the public type.
type Supervisor struct {
	opts  Options
	mu    sync.Mutex
	insts map[string]*instance
}

// instance is what Supervisor remembers about a running plugin process.
type instance struct {
	manifest    *manifest.Manifest
	cmd         *exec.Cmd
	pluginSock  string
	hostSock    string
	conn        *grpc.ClientConn
	client      pluginv1.PluginClient
	hostGRPC    *grpc.Server
	hostLis     net.Listener
	lastUsed    time.Time
	restartHist []time.Time
}

func New(opts Options) *Supervisor {
	return &Supervisor{opts: opts, insts: map[string]*instance{}}
}

// setActiveLocked syncs the active-plugins gauge to the current instance
// count. Call it after every mutation of s.insts while holding s.mu so the
// gauge can never drift from len(insts).
func (s *Supervisor) setActiveLocked() {
	phmetrics.ActivePlugins.Set(float64(len(s.insts)))
}

// Start runs the idle reaper loop until ctx is cancelled.
func (s *Supervisor) Start(ctx context.Context) error {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.reapIdle()
		}
	}
}

// EnsureRunning returns a connected client for pluginID, spawning the
// subprocess if necessary. The returned client is goroutine-safe; do not
// close it -- the supervisor owns the lifetime.
func (s *Supervisor) EnsureRunning(ctx context.Context, pluginID string) (pluginv1.PluginClient, *manifest.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.opts.Manifests[pluginID]
	if !ok {
		return nil, nil, fmt.Errorf("unknown plugin %q", pluginID)
	}

	if inst, ok := s.insts[pluginID]; ok && inst.cmd.ProcessState == nil {
		// Process not yet reaped. Best-effort liveness via Ping; if it
		// fails we tear down and respawn.
		pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		_, perr := inst.client.Ping(pingCtx, &pluginv1.PingRequest{})
		cancel()
		if perr == nil {
			inst.lastUsed = time.Now()
			return inst.client, m, nil
		}
		s.opts.Logger.WithError(perr).WithField("plugin_id", pluginID).
			Warn("supervisor: ping failed, respawning")
		s.teardownLocked(pluginID, inst)
	}

	// Restart-budget check (per-plugin per-minute).
	if !s.allowSpawnLocked(pluginID, m) {
		phmetrics.SpawnFailures.WithLabelValues(pluginID, "budget").Inc()
		return nil, m, fmt.Errorf("supervisor: %s exceeded restart budget %d/min",
			pluginID, m.Supervisor.MaxRestartPerMin)
	}

	inst, err := s.spawnLocked(ctx, m)
	if err != nil {
		// spawnLocked stamps the specific reason on the failure counter; the
		// error is already classified there.
		return nil, m, fmt.Errorf("spawn %s: %w", pluginID, err)
	}
	s.insts[pluginID] = inst
	s.setActiveLocked()
	return inst.client, m, nil
}

// allowSpawnLocked checks the restart budget. MaxRestartPerMin <= 0
// disables the check.
func (s *Supervisor) allowSpawnLocked(pluginID string, m *manifest.Manifest) bool {
	budget := m.Supervisor.MaxRestartPerMin
	if budget <= 0 {
		return true
	}
	prev, ok := s.insts[pluginID]
	if !ok {
		return true
	}
	cutoff := time.Now().Add(-time.Minute)
	count := 0
	for _, t := range prev.restartHist {
		if t.After(cutoff) {
			count++
		}
	}
	return count < budget
}

// spawnLocked is the only place that starts a plugin process. Caller
// holds s.mu.
func (s *Supervisor) spawnLocked(ctx context.Context, m *manifest.Manifest) (*instance, error) {
	if len(m.Command) == 0 {
		phmetrics.SpawnFailures.WithLabelValues(m.ID, "config").Inc()
		return nil, errors.New("manifest.command is empty")
	}

	// We don't know the pid until exec; use a nonce so the socket pair
	// is unique even if two spawns race. pid is filled in for logging
	// after Start succeeds.
	nonce := time.Now().UnixNano()
	pluginSock := filepath.Join(s.opts.SocketDir,
		fmt.Sprintf("%s.%d.plugin.sock", m.ID, nonce))
	hostSock := filepath.Join(s.opts.SocketDir,
		fmt.Sprintf("%s.%d.host.sock", m.ID, nonce))
	_ = os.Remove(pluginSock)
	_ = os.Remove(hostSock)

	// Host-side gRPC server first -- the plugin may dial it during its
	// own startup (e.g. to GetConfig before serving its plugin socket).
	hostLis, err := net.Listen("unix", hostSock)
	if err != nil {
		phmetrics.SpawnFailures.WithLabelValues(m.ID, "listen").Inc()
		return nil, fmt.Errorf("listen host sock: %w", err)
	}
	hostGRPC := grpc.NewServer(
		grpc.UnaryInterceptor(stampPluginID(m.ID)),
		grpc.StreamInterceptor(stampPluginIDStream(m.ID)),
	)
	pluginv1.RegisterHostServer(hostGRPC, s.opts.HostServer)
	go func() {
		if err := hostGRPC.Serve(hostLis); err != nil {
			s.opts.Logger.WithError(err).WithField("plugin_id", m.ID).
				Debug("host gRPC server exited")
		}
	}()

	cmd := exec.Command(m.Command[0], m.Command[1:]...)
	cmd.Env = append(os.Environ(),
		"DR_PLUGIN_SOCK="+pluginSock,
		"DR_HOST_SOCK="+hostSock,
		"DR_PLUGIN_ID="+m.ID,
	)
	cmd.Stdout = newLogWriter(s.opts.Logger, m.ID, "stdout")
	cmd.Stderr = newLogWriter(s.opts.Logger, m.ID, "stderr")
	// Place each plugin in its own process group so we can SIGKILL the
	// whole tree if the plugin spawns children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		hostGRPC.Stop()
		_ = hostLis.Close()
		_ = os.Remove(hostSock)
		phmetrics.SpawnFailures.WithLabelValues(m.ID, "exec").Inc()
		return nil, fmt.Errorf("exec: %w", err)
	}

	// Wait for the plugin to bind its socket, then dial it. Long-running
	// startups (heavy Python imports) push this past the default 2s.
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := waitSocketReady(dialCtx, pluginSock); err != nil {
		_ = cmd.Process.Kill()
		hostGRPC.Stop()
		_ = os.Remove(hostSock)
		phmetrics.SpawnFailures.WithLabelValues(m.ID, "socket_timeout").Inc()
		return nil, fmt.Errorf("plugin socket not ready: %w", err)
	}

	conn, err := grpc.NewClient(
		"unix://"+pluginSock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		hostGRPC.Stop()
		_ = os.Remove(hostSock)
		phmetrics.SpawnFailures.WithLabelValues(m.ID, "dial").Inc()
		return nil, fmt.Errorf("dial plugin: %w", err)
	}
	client := pluginv1.NewPluginClient(conn)

	// Hello handshake. Pass the decrypted config + work_dir. We don't
	// validate the response capabilities subset here -- that's a tier-2
	// check the route handlers do per-RPC.
	helloCtx, helloCancel := context.WithTimeout(ctx, 5*time.Second)
	defer helloCancel()
	helloResp, err := client.Hello(helloCtx, &pluginv1.HelloRequest{
		HostVersion: "0.1.0",
		WorkDir:     "", // TODO(plugin-host): stage per-run; Phase C
	})
	if err != nil {
		_ = conn.Close()
		_ = cmd.Process.Kill()
		hostGRPC.Stop()
		_ = os.Remove(hostSock)
		phmetrics.SpawnFailures.WithLabelValues(m.ID, "hello").Inc()
		return nil, fmt.Errorf("hello: %w", err)
	}

	s.opts.Logger.WithFields(logrus.Fields{
		"plugin_id":      m.ID,
		"plugin_version": helloResp.PluginVersion,
		"capabilities":   helloResp.Capabilities,
		"pid":            cmd.Process.Pid,
	}).Info("supervisor: plugin spawned")

	return &instance{
		manifest:    m,
		cmd:         cmd,
		pluginSock:  pluginSock,
		hostSock:    hostSock,
		conn:        conn,
		client:      client,
		hostGRPC:    hostGRPC,
		hostLis:     hostLis,
		lastUsed:    time.Now(),
		restartHist: append([]time.Time{time.Now()}, restartHistOf(s.insts, m.ID)...),
	}, nil
}

// restartHistOf returns the prior instance's restart history if any,
// for inheritance into the new instance.
func restartHistOf(insts map[string]*instance, id string) []time.Time {
	if prev, ok := insts[id]; ok {
		return prev.restartHist
	}
	return nil
}

// Shutdown stops every running plugin via Plugin.Shutdown then SIGKILL on
// timeout.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, inst := range s.insts {
		s.teardownLocked(id, inst)
	}
	return nil
}

// teardownLocked stops one instance. Caller holds s.mu. Best-effort --
// errors are logged, not propagated, because teardown happens on the
// shutdown path where there's nobody to surface the error to.
func (s *Supervisor) teardownLocked(id string, inst *instance) {
	// Send the Shutdown RPC with a short timeout. Plugin should exit
	// on its own after this; if it doesn't we SIGTERM then SIGKILL.
	rpcCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if _, err := inst.client.Shutdown(rpcCtx, &pluginv1.ShutdownRequest{GraceSeconds: 5}); err != nil {
		s.opts.Logger.WithError(err).WithField("plugin_id", id).
			Debug("Shutdown RPC failed; falling through to signals")
	}
	cancel()

	_ = inst.conn.Close()
	// Wait briefly for the process to exit on its own, then SIGTERM,
	// then SIGKILL after the manifest's grace.
	done := make(chan struct{})
	go func() { _ = inst.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-inst.cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}

	inst.hostGRPC.Stop()
	_ = inst.hostLis.Close()
	_ = os.Remove(inst.pluginSock)
	_ = os.Remove(inst.hostSock)

	delete(s.insts, id)
	s.setActiveLocked()
}

// reapIdle walks instances and tears down any whose lastUsed exceeded
// the manifest's IdleTimeoutSeconds.
func (s *Supervisor) reapIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, inst := range s.insts {
		ttl := time.Duration(inst.manifest.Supervisor.IdleTimeoutSeconds) * time.Second
		if ttl <= 0 || now.Sub(inst.lastUsed) < ttl {
			continue
		}
		s.opts.Logger.WithField("plugin_id", id).Info("supervisor: idle reap")
		s.teardownLocked(id, inst)
	}
}

// waitSocketReady polls until the path exists + is connectable, or ctx
// expires. We can't just stat() because the plugin may create the file
// before binding the listener -- a real connect proves the server is up.
func waitSocketReady(ctx context.Context, path string) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// stampPluginID is the unary interceptor injecting the plugin id into
// every Host RPC. Bound at server-build-time so each per-plugin gRPC
// server stamps its own id with no per-call lookup.
func stampPluginID(pluginID string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(context.WithValue(ctx, PluginCtxKey{}, pluginID), req)
	}
}

// stampPluginIDStream mirrors stampPluginID for streaming RPCs. We have
// none today on the Host service but wire it for forward-compat.
func stampPluginIDStream(pluginID string) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		wrapped := &wrappedStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), PluginCtxKey{}, pluginID)}
		return handler(srv, wrapped)
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// logWriter pipes a plugin's stdout/stderr into the host logger so plugin
// output ends up in the same stream as host output. Each line becomes
// one log entry.
type logWriter struct {
	logger *logrus.Logger
	id     string
	stream string
}

func newLogWriter(logger *logrus.Logger, pluginID, stream string) *logWriter {
	return &logWriter{logger: logger, id: pluginID, stream: stream}
}

func (w *logWriter) Write(p []byte) (int, error) {
	// Trim trailing newline so logrus doesn't add a second one.
	msg := string(p)
	for len(msg) > 0 && (msg[len(msg)-1] == '\n' || msg[len(msg)-1] == '\r') {
		msg = msg[:len(msg)-1]
	}
	if msg == "" {
		return len(p), nil
	}
	lvl := logrus.InfoLevel
	if w.stream == "stderr" {
		lvl = logrus.WarnLevel
	}
	w.logger.WithFields(logrus.Fields{"plugin_id": w.id, "stream": w.stream}).Log(lvl, msg)
	return len(p), nil
}

// ErrSpawnNotImplemented is retained for backwards-compat with the route
// handler that maps to 501; now that spawn is implemented, route handlers
// can surface real errors instead. New code should not return this.
var ErrSpawnNotImplemented = errors.New("supervisor: spawn not yet implemented")

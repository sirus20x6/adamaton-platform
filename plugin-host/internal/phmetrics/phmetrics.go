// Package phmetrics holds the Prometheus collectors for plugin-host.
//
// plugin-host exposes /metrics through a *custom* registry built in
// cmd/plugin-host (prometheus.NewRegistry(), not the global default), so these
// collectors are NOT auto-registered via promauto. Instead they're constructed
// here as package-level vars and registered onto whatever Registerer the
// caller passes via MustRegister — cmd/plugin-host hands it the same custom
// registry it serves on /metrics. Registering on the default registerer would
// leave these invisible to the scraper, which is exactly the silent-miss this
// split avoids.
//
// Collectors:
//   - ActivePlugins        gauge   — currently-running plugin subprocesses.
//   - SpawnFailures        counter — supervisor spawn attempts that errored,
//     by plugin id and a low-cardinality reason.
//   - RequestErrors        counter — route-level plugin request failures, by
//     plugin id and error_type.
package phmetrics

import "github.com/prometheus/client_golang/prometheus"

// ActivePlugins is the count of plugin subprocesses the supervisor currently
// has running. A gauge because it moves up (spawn) and down (idle reap /
// teardown / crash). Use SetActivePlugins from the supervisor under its lock so
// the value tracks len(insts) exactly.
var ActivePlugins = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "plugin_host_active_plugins",
	Help: "Number of plugin subprocesses currently running under the supervisor.",
})

// SpawnFailures counts supervisor spawn attempts that did not yield a running,
// handshaked plugin. The `reason` label is a closed, low-cardinality set:
//
//	"exec"          — cmd.Start failed (bad command, missing binary).
//	"socket_timeout"— plugin never bound its socket within the dial window.
//	"dial"          — socket bound but gRPC dial failed.
//	"hello"         — Hello handshake RPC failed.
//	"budget"        — refused: the per-minute restart budget was exhausted.
//	"config"        — manifest/command was invalid (e.g. empty command).
//	"listen"        — host-side socket listen failed.
//
// Keep this set closed; new spawn failure modes must add a case here AND at the
// supervisor call site.
var SpawnFailures = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "plugin_host_spawn_failures_total",
		Help: "Total supervisor plugin spawn failures, by plugin id and reason.",
	},
	[]string{"plugin", "reason"},
)

// RequestErrors counts plugin request failures observed at the HTTP route
// layer, by plugin id and error_type. error_type is a closed set kept small to
// bound cardinality:
//
//	"spawn"   — supervisor.EnsureRunning failed for the plugin.
//	"rpc"     — the plugin's gRPC call (e.g. SearchQuery) returned an error.
//	"timeout" — the call deadline expired before a response.
//
// The plugin label is the manifest id ("zotero", "arxiv", …) — already a small
// bounded set — never a free-form query string.
var RequestErrors = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "plugin_host_plugin_request_errors_total",
		Help: "Total plugin request errors at the route layer, by plugin and error_type.",
	},
	[]string{"plugin", "error_type"},
)

// MustRegister registers every plugin-host collector on reg. Call once at
// startup with the same registry served on /metrics. Panics on a duplicate
// registration, which is the desired fail-fast if the wiring is wrong.
func MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(ActivePlugins, SpawnFailures, RequestErrors)
}

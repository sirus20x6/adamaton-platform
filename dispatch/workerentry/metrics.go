// metrics.go wires Prometheus instrumentation onto the dispatch worker's
// Temporal activities via a WorkerInterceptor. Every activity execution is
// timed (core/metrics.ActivityDuration) and failures are counted
// (dispatchActivityFailures) so operators can alert on a rising activity
// error rate without grepping worker logs.
//
// The interceptor is installed by both the standalone dispatch-worker binary
// and the umbrella's consolidated adamaton-worker via Interceptor(); keeping
// it here (next to Register) means the instrumentation travels with the
// registration logic and can't drift apart from it.
package workerentry

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"

	"github.com/sirus20x6/adamaton-core/metrics"
)

// dispatchActivityFailures counts activity executions that returned a non-nil
// error, labelled by activity name. core/metrics owns ActivityDuration (which
// already carries a result label), but a standalone failure counter makes the
// "is anything erroring right now" alert a single, cheap query rather than a
// histogram-derived ratio. Registered on the default registerer via promauto
// so test binaries that link this package twice don't panic on a duplicate
// registration.
//
// Registered in this package (not core/metrics) to keep the change scoped to
// the platform sub-repo; core stays untouched this wave.
var dispatchActivityFailures = promauto.With(prometheus.DefaultRegisterer).NewCounterVec(
	prometheus.CounterOpts{
		Name: "gogents_dispatch_activity_failures_total",
		Help: "Total dispatch-worker activity executions that returned an error, by activity name.",
	},
	[]string{"name"},
)

// Interceptor returns a Temporal WorkerInterceptor that records activity
// duration + failure metrics. Install it via worker.Options.Interceptors.
func Interceptor() interceptor.WorkerInterceptor {
	return &metricsWorkerInterceptor{}
}

type metricsWorkerInterceptor struct {
	interceptor.WorkerInterceptorBase
}

func (w *metricsWorkerInterceptor) InterceptActivity(
	ctx context.Context,
	next interceptor.ActivityInboundInterceptor,
) interceptor.ActivityInboundInterceptor {
	i := &metricsActivityInboundInterceptor{}
	i.Next = next
	return i
}

type metricsActivityInboundInterceptor struct {
	interceptor.ActivityInboundInterceptorBase
}

func (a *metricsActivityInboundInterceptor) ExecuteActivity(
	ctx context.Context,
	in *interceptor.ExecuteActivityInput,
) (interface{}, error) {
	name := activity.GetInfo(ctx).ActivityType.Name
	start := time.Now()
	res, err := a.Next.ExecuteActivity(ctx, in)
	// result label vocabulary kept low-cardinality per core/metrics docs:
	// "success" | "error". Temporal surfaces cancellation/timeout as errors
	// here too; we don't try to subdivide them (the worker logs carry the
	// detail) to keep the metric's cardinality bounded.
	result := "success"
	if err != nil {
		result = "error"
		dispatchActivityFailures.WithLabelValues(name).Inc()
	}
	metrics.ObserveActivityDuration(name, result, time.Since(start))
	return res, err
}

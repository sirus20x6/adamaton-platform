package apiserver

// Temporal task-queue depth gauge (observability card: temporal_queue
// congestion). The health aggregator's temporal_queue probes only watch
// worker HEARTBEAT staleness (evo.workers), which says "workers exist" but
// not "work is piling up". This poller asks Temporal itself, via
// DescribeTaskQueueEnhanced with ReportStats, for the approximate backlog
// of every queue declared in the health topology, and publishes it as a
// Prometheus gauge so operators can alert on queue congestion:
//
//	gogents_temporal_task_queue_depth{queue="skills", task_type="activity"}
//
// Label cardinality is bounded: `queue` is the fixed set from
// deploy/health/topology.yml, `task_type` is workflow|activity.
//
// The stats come from Temporal's DescribeTaskQueue ENHANCED mode
// (server >= 1.24; the fleet runs newer). On per-queue errors the poller
// keeps the last-published value and increments a poll-error counter
// rather than zeroing the gauge — a scrape gap is better than a false
// "queue drained" signal.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamaton-platform/dashboard/apiserver/health"
)

// TemporalQueueDepth is the approximate backlog per task queue and task
// type. See package comment in this file for label semantics.
var TemporalQueueDepth = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gogents_temporal_task_queue_depth",
		Help: "Approximate Temporal task-queue backlog (DescribeTaskQueue stats), by queue and task type (workflow|activity).",
	},
	[]string{"queue", "task_type"},
)

// TemporalQueueDepthAgeSeconds is the approximate age of the oldest
// backlogged task, same labels as TemporalQueueDepth. Depth alone can't
// distinguish "10 tasks that arrived this second" from "10 tasks stuck
// for an hour"; age can.
var TemporalQueueDepthAgeSeconds = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gogents_temporal_task_queue_backlog_age_seconds",
		Help: "Approximate age of the oldest backlogged task in the Temporal task queue, by queue and task type.",
	},
	[]string{"queue", "task_type"},
)

// TemporalQueueDepthPollErrors counts failed DescribeTaskQueue polls per
// queue. Alert on a sustained non-zero rate: it means the depth gauge is
// stale (last good value), not that the queue is unhealthy per se.
var TemporalQueueDepthPollErrors = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gogents_temporal_task_queue_depth_poll_errors_total",
		Help: "Total failed DescribeTaskQueue depth polls, by queue.",
	},
	[]string{"queue"},
)

func init() {
	prometheus.MustRegister(
		TemporalQueueDepth,
		TemporalQueueDepthAgeSeconds,
		TemporalQueueDepthPollErrors,
	)
}

// taskQueueDescriber is the narrow slice of client.Client the poller
// needs. Same testability pattern as temporalStarter/temporalDescriber in
// server.go — unit tests inject a fake without dialing Temporal.
type taskQueueDescriber interface {
	DescribeTaskQueueEnhanced(ctx context.Context, options client.DescribeTaskQueueEnhancedOptions) (client.TaskQueueDescription, error)
}

// Compile-time guard: a real client.Client must satisfy the interface.
var _ taskQueueDescriber = (client.Client)(nil)

// queueDepthTaskTypes maps the gauge's task_type label values to the SDK
// enum. Fixed vocabulary — do not add nexus etc. without a dashboard plan.
var queueDepthTaskTypes = []struct {
	label string
	typ   client.TaskQueueType
}{
	{"workflow", client.TaskQueueTypeWorkflow},
	{"activity", client.TaskQueueTypeActivity},
}

// queueDepthPoller periodically refreshes the depth gauges for a fixed
// set of queues. Owned by the apiserver; started from NewAPIServer when
// both a Temporal client and a health topology are available.
type queueDepthPoller struct {
	describer taskQueueDescriber
	queues    []string
	interval  time.Duration
	logger    *logrus.Logger
}

func newQueueDepthPoller(d taskQueueDescriber, queues []string, interval time.Duration, logger *logrus.Logger) *queueDepthPoller {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &queueDepthPoller{describer: d, queues: queues, interval: interval, logger: logger}
}

// run polls immediately, then on every tick until ctx is done. Run it on
// a goroutine; like the fleet-health refresh loop, process exit takes it
// with it.
func (p *queueDepthPoller) run(ctx context.Context) {
	p.pollOnce(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollOnce(ctx)
		}
	}
}

// pollOnce refreshes every queue's gauges. Per-queue failures are
// isolated: they bump the error counter and leave that queue's previous
// gauge values in place.
func (p *queueDepthPoller) pollOnce(ctx context.Context) {
	for _, queue := range p.queues {
		qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		desc, err := p.describer.DescribeTaskQueueEnhanced(qCtx, client.DescribeTaskQueueEnhancedOptions{
			TaskQueue: queue,
			TaskQueueTypes: []client.TaskQueueType{
				client.TaskQueueTypeWorkflow,
				client.TaskQueueTypeActivity,
			},
			ReportStats: true,
		})
		cancel()
		if err != nil {
			TemporalQueueDepthPollErrors.WithLabelValues(queue).Inc()
			if p.logger != nil {
				p.logger.WithError(err).WithField("queue", queue).
					Debug("temporal queue-depth poll failed; gauge keeps last value")
			}
			continue
		}
		for _, tt := range queueDepthTaskTypes {
			depth, age := sumQueueStats(desc, tt.typ)
			TemporalQueueDepth.WithLabelValues(queue, tt.label).Set(float64(depth))
			TemporalQueueDepthAgeSeconds.WithLabelValues(queue, tt.label).Set(age.Seconds())
		}
	}
}

// sumQueueStats totals ApproximateBacklogCount (and takes the max backlog
// age) across every Build ID version of the queue for one task type.
// Unversioned workers report under the "" Build ID; summing across all
// versions keeps the gauge meaningful if worker versioning is ever
// enabled.
func sumQueueStats(desc client.TaskQueueDescription, typ client.TaskQueueType) (int64, time.Duration) {
	var depth int64
	var maxAge time.Duration
	for _, vi := range desc.VersionsInfo {
		ti, ok := vi.TypesInfo[typ]
		if !ok || ti.Stats == nil {
			continue
		}
		depth += ti.Stats.ApproximateBacklogCount
		if ti.Stats.ApproximateBacklogAge > maxAge {
			maxAge = ti.Stats.ApproximateBacklogAge
		}
	}
	return depth, maxAge
}

// startQueueDepthPoller wires the poller off the health topology's
// declared temporal_queue roles. No-op (returns false) when there are no
// queues to watch or no Temporal client.
func (s *APIServer) startQueueDepthPoller(ctx context.Context, topo *health.Topology) bool {
	if topo == nil || s.temporalClient == nil {
		return false
	}
	queues := topo.TemporalQueues()
	if len(queues) == 0 {
		return false
	}
	p := newQueueDepthPoller(s.temporalClient, queues, 30*time.Second, s.logger)
	go p.run(ctx)
	s.logger.WithField("queues", queues).Info("temporal queue-depth poller started")
	return true
}

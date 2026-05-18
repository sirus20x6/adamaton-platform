package health

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// InstanceStatus is one concrete instance (one container on one host)
// after probing. Used by /api/v1/health/instances.
type InstanceStatus struct {
	Host      string         `json:"host"`
	Role      string         `json:"role"`
	Container string         `json:"container,omitempty"`
	Image     string         `json:"image,omitempty"`
	Status    Status         `json:"status"`
	Detail    string         `json:"detail,omitempty"`
	LatencyMS float64        `json:"latency_ms"`
	LastSeen  time.Time      `json:"last_seen"`
	Stats     map[string]any `json:"stats,omitempty"`
}

// RoleStatus is the per-role rollup. healthy = how many instances are
// "ok"; running = how many exist regardless of probe outcome. status is
// derived from MinHealthy.
type RoleStatus struct {
	Name       string   `json:"name"`
	Kind       Kind     `json:"kind"`
	MinHealthy int      `json:"min_healthy"`
	Running    int      `json:"running"`
	Healthy    int      `json:"healthy"`
	Status     Status   `json:"status"`
	Optional   bool     `json:"optional,omitempty"`
	Instances  []string `json:"instances"` // "container@host"
}

// CapabilityStatus is the user-facing rollup. status is the worst-of
// the role statuses (skipping optional roles that are offline).
type CapabilityStatus struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Status      Status   `json:"status"`
	Roles       []string `json:"roles"`
	Down        []string `json:"down,omitempty"`
}

// Snapshot is the cached fanout. Served by the GET endpoints; refreshed
// every refreshInterval in a background goroutine. StaleFor reports how
// long ago the snapshot was last refreshed; consumers can render a
// yellow "refreshing" indicator if it grows past the cache TTL.
type Snapshot struct {
	GeneratedAt  time.Time          `json:"generated_at"`
	StaleFor     string             `json:"stale_for,omitempty"`
	Instances    []InstanceStatus   `json:"instances"`
	Roles        []RoleStatus       `json:"roles"`
	Capabilities []CapabilityStatus `json:"capabilities"`
}

// Probers bundles the per-kind impls. Aggregator owns this and dispatches
// by Role.Kind. Each field can be nil — that kind's instances report
// status=unknown.
type Probers struct {
	HTTP          *HTTPProber
	TCP           TCPProber
	Redis         RedisProber
	Postgres      *PostgresProber
	TemporalQueue *TemporalQueueProber
}

// Aggregator owns the cache + refresh goroutine. Construct with
// NewAggregator, call Start to spawn the refresher, call Get to read.
type Aggregator struct {
	topology        *Topology
	fleet           *FleetClient
	probers         Probers
	refreshInterval time.Duration

	// Local docker-network DNS: when the apiserver runs on pi5, a role
	// like r2g resolves to "r2g:7373" via docker-compose DNS. When
	// probing peers, we'd need a different address. v1 only probes
	// services on the LOCAL host; the FleetClient.Status() call is
	// used for instance discovery on peers but probes don't run there.
	localHost string

	cache atomic.Pointer[Snapshot]

	mu       sync.Mutex // guards refresh-in-flight
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewAggregator wires the deps. refreshInterval is how often the cache
// is rebuilt; 15s is a sensible production default. localHost is the
// host name we're running on (e.g. "pi5"); used to decide which
// instances to actively probe vs which to just trust the deploy-agent's
// docker-compose-ps view of.
func NewAggregator(t *Topology, fleet *FleetClient, probers Probers, refreshInterval time.Duration, localHost string) *Aggregator {
	a := &Aggregator{
		topology:        t,
		fleet:           fleet,
		probers:         probers,
		refreshInterval: refreshInterval,
		localHost:       localHost,
		stopCh:          make(chan struct{}),
	}
	// Seed with an empty snapshot so reads never return nil.
	empty := &Snapshot{GeneratedAt: time.Now()}
	a.cache.Store(empty)
	return a
}

// Start spawns the refresh goroutine + does an initial fanout. Returns
// after the first refresh completes so callers can rely on Get
// returning real data immediately.
func (a *Aggregator) Start(ctx context.Context) {
	a.refresh(ctx)
	go a.loop(ctx)
}

// Stop cancels the refresh loop.
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() { close(a.stopCh) })
}

func (a *Aggregator) loop(ctx context.Context) {
	t := time.NewTicker(a.refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-t.C:
			a.refresh(ctx)
		}
	}
}

// Get returns the latest snapshot. Never nil.
func (a *Aggregator) Get() *Snapshot {
	snap := a.cache.Load()
	if snap == nil {
		return &Snapshot{GeneratedAt: time.Now()}
	}
	// Re-compute StaleFor at read time so consumers see fresh values.
	now := time.Now()
	staleness := now.Sub(snap.GeneratedAt)
	out := *snap
	if staleness > 2*a.refreshInterval {
		out.StaleFor = staleness.Truncate(time.Second).String()
	}
	return &out
}

// Refresh is the user-triggered cache-bust. Single-flight via mu.
func (a *Aggregator) Refresh(ctx context.Context) {
	a.refresh(ctx)
}

func (a *Aggregator) refresh(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	// 1. Discover instances across the fleet via deploy-agent.
	instances := a.discoverInstances(ctx)

	// 2. Probe each instance per its role.kind. Postgres + redis are
	//    cluster-singletons so we probe them once with synthetic
	//    targets when the topology declares them.
	instances = a.probeInstances(ctx, instances)

	// 3. Roll up: instances -> roles -> capabilities.
	roles := a.rollupRoles(instances)
	caps := a.rollupCapabilities(roles)

	snap := &Snapshot{
		GeneratedAt:  time.Now(),
		Instances:    instances,
		Roles:        roles,
		Capabilities: caps,
	}
	a.cache.Store(snap)
}

// discoverInstances asks each host's deploy-agent which services it
// runs, then asks /status?service=X for the role-relevant ones to get
// container/image details. For roles whose Kind is postgres, redis, or
// temporal_queue the "instance" is synthesized (no per-container view).
func (a *Aggregator) discoverInstances(ctx context.Context) []InstanceStatus {
	var (
		mu  sync.Mutex
		out []InstanceStatus
		wg  sync.WaitGroup
	)

	hosts := a.fleet.Hosts()

	// Gather the deploy-agent /services lists per host so we can decide
	// which roles are "real containers we'll probe per-host" vs which
	// are "cluster singletons we synthesize at localHost". A role
	// appears in /services on some host iff it's in that host's
	// MANIFEST.yaml — postgres / redis / temporal are NOT in the
	// manifest (they're not deploy-agent-restartable) so they fall to
	// the singleton synthesis branch below.
	type hostServices struct {
		host string
		svcs []string
	}
	servicesByHost := make([]hostServices, 0, len(hosts))
	for _, host := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			svcs, err := a.fleet.Services(ctx, h)
			if err != nil {
				return // host unreachable; roles that ran there count 0
			}
			mu.Lock()
			servicesByHost = append(servicesByHost, hostServices{host: h, svcs: svcs})
			mu.Unlock()
		}(host)
	}
	wg.Wait()

	// Union of services discovered across all hosts — used to decide
	// which roles need singleton synthesis.
	discovered := map[string]bool{}
	for _, hs := range servicesByHost {
		for _, s := range hs.svcs {
			discovered[s] = true
		}
	}

	// Singleton synthesis: any role whose name doesn't appear in any
	// host's /services gets a single InstanceStatus at localHost. This
	// catches:
	//   - postgres / redis / temporal (kind=postgres|redis|tcp,
	//     not in manifests because they're not deploy-agent-restartable)
	//   - temporal_queue roles (no docker container per queue;
	//     the role IS the thing being probed via evo.workers)
	//   - any future declared-but-not-yet-running role
	for _, name := range a.topology.RoleOrder {
		role := a.topology.Roles[name]
		// temporal_queue ALWAYS goes through the singleton path even
		// if a similarly-named docker service exists, since its probe
		// hits evo.workers not the container.
		if discovered[name] && role.Kind != KindTemporalQueue {
			continue
		}
		out = append(out, InstanceStatus{
			Host:   a.localHost,
			Role:   name,
			Status: StatusUnknown,
		})
	}

	// Per-host container instances for roles that ARE in deploy-agent
	// manifests. Fans out /status?service=X to get container details.
	for _, hs := range servicesByHost {
		wg.Add(1)
		go func(host string, svcs []string) {
			defer wg.Done()
			roleSet := map[string]bool{}
			for _, name := range a.topology.RoleOrder {
				role := a.topology.Roles[name]
				if role.Kind == KindHTTP || role.Kind == KindTCP {
					roleSet[name] = true
				}
			}
			for _, svc := range svcs {
				if !roleSet[svc] {
					continue
				}
				containers, err := a.fleet.Status(ctx, host, svc)
				if err != nil || len(containers) == 0 {
					mu.Lock()
					out = append(out, InstanceStatus{
						Host:   host,
						Role:   svc,
						Status: StatusOffline,
						Detail: "no running container",
					})
					mu.Unlock()
					continue
				}
				for _, c := range containers {
					mu.Lock()
					out = append(out, InstanceStatus{
						Host:      host,
						Role:      svc,
						Container: c.Name,
						Image:     c.Image,
						Status:    StatusUnknown,
					})
					mu.Unlock()
				}
			}
		}(hs.host, hs.svcs)
	}
	wg.Wait()
	return out
}

// probeInstances runs the per-kind probe for every InstanceStatus. For
// peers (host != a.localHost) we trust the docker-compose-ps state
// since we can't reach in-cluster DNS names from another host.
func (a *Aggregator) probeInstances(ctx context.Context, instances []InstanceStatus) []InstanceStatus {
	var wg sync.WaitGroup
	out := make([]InstanceStatus, len(instances))
	copy(out, instances)

	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst := &out[i]
			role := a.topology.Roles[inst.Role]
			// Non-local hosts: short-cut to "running" trust.
			if inst.Host != a.localHost && (role.Kind == KindHTTP || role.Kind == KindTCP) {
				inst.Status = StatusUnknown
				inst.Detail = "remote host — probe skipped, trust /services"
				if inst.Container != "" {
					inst.Status = StatusOK
				}
				inst.LastSeen = time.Now()
				return
			}
			r := a.probeOne(ctx, role, inst)
			inst.Status = r.Status
			inst.Detail = r.Detail
			inst.LatencyMS = r.LatencyMS
			inst.Stats = r.Stats
			inst.LastSeen = time.Now()
		}(i)
	}
	wg.Wait()
	return out
}

func (a *Aggregator) probeOne(ctx context.Context, role Role, inst *InstanceStatus) Result {
	target := Target{
		Host:      probeHost(role, inst),
		HostLabel: inst.Host,
		Container: inst.Container,
		Image:     inst.Image,
	}
	switch role.Kind {
	case KindHTTP:
		if a.probers.HTTP == nil {
			return Result{Status: StatusUnknown, Detail: "no http prober"}
		}
		return a.probers.HTTP.Probe(ctx, target, role.Probe)
	case KindTCP:
		return a.probers.TCP.Probe(ctx, target, role.Probe)
	case KindRedis:
		return a.probers.Redis.Probe(ctx, target, role.Probe)
	case KindPostgres:
		if a.probers.Postgres == nil {
			return Result{Status: StatusUnknown, Detail: "no postgres prober"}
		}
		return a.probers.Postgres.Probe(ctx, target, role.Probe)
	case KindTemporalQueue:
		if a.probers.TemporalQueue == nil {
			return Result{Status: StatusUnknown, Detail: "no temporal_queue prober"}
		}
		live, err := a.probers.TemporalQueue.ProbeQueue(ctx, role.Queue, role.HeartbeatMaxAge)
		if err != nil {
			return Result{Status: StatusOffline, Detail: err.Error()}
		}
		st := StatusOK
		if live.Alive == 0 && role.MinHealthy > 0 {
			st = StatusOffline
		} else if live.Alive < role.MinHealthy {
			st = StatusDegraded
		}
		return Result{
			Status: st,
			Detail: fmt.Sprintf("%d/%d workers alive (min %d)", live.Alive, live.Total, role.MinHealthy),
			Stats: map[string]any{
				"workers_alive":       live.Alive,
				"workers_total":       live.Total,
				"oldest_heartbeat_ms": live.OldestHeartbeatAge.Milliseconds(),
			},
		}
	}
	return Result{Status: StatusUnknown, Detail: "unknown kind"}
}

func probeHost(role Role, inst *InstanceStatus) string {
	if role.Probe.Host != "" {
		return role.Probe.Host
	}
	// Default: role name resolves to the compose-internal DNS name.
	// Exception: postgres/redis use "postgres"/"redis" not "postgres"
	// vs the actual service — but topology.yml lists them as the
	// service name anyway, so this is fine.
	return inst.Role
}

// rollupRoles groups instances by role name and applies MinHealthy.
func (a *Aggregator) rollupRoles(instances []InstanceStatus) []RoleStatus {
	byRole := map[string][]InstanceStatus{}
	for _, inst := range instances {
		byRole[inst.Role] = append(byRole[inst.Role], inst)
	}

	out := make([]RoleStatus, 0, len(a.topology.RoleOrder))
	for _, name := range a.topology.RoleOrder {
		role := a.topology.Roles[name]
		insts := byRole[name]
		healthy := 0
		running := len(insts)
		labels := make([]string, 0, len(insts))
		for _, inst := range insts {
			if inst.Status == StatusOK {
				healthy++
			}
			label := inst.Container
			if label == "" {
				label = name
			}
			labels = append(labels, label+"@"+inst.Host)
		}
		sort.Strings(labels)
		status := computeRoleStatus(role, running, healthy)
		out = append(out, RoleStatus{
			Name:       name,
			Kind:       role.Kind,
			MinHealthy: role.MinHealthy,
			Running:    running,
			Healthy:    healthy,
			Status:     status,
			Optional:   role.Optional,
			Instances:  labels,
		})
	}
	return out
}

func computeRoleStatus(role Role, running, healthy int) Status {
	switch {
	case healthy >= role.MinHealthy && healthy == running:
		return StatusOK
	case healthy >= role.MinHealthy && running > healthy:
		// At quorum but some replicas are sad — partial.
		return StatusDegraded
	case running == 0 && role.MinHealthy == 0:
		return StatusOK
	case running == 0:
		return StatusOffline
	case healthy < role.MinHealthy && healthy > 0:
		return StatusDegraded
	default:
		return StatusOffline
	}
}

func (a *Aggregator) rollupCapabilities(roles []RoleStatus) []CapabilityStatus {
	byName := map[string]RoleStatus{}
	for _, r := range roles {
		byName[r.Name] = r
	}

	out := make([]CapabilityStatus, 0, len(a.topology.CapabilityOrder))
	for _, name := range a.topology.CapabilityOrder {
		c := a.topology.Capabilities[name]
		worst := StatusOK
		down := []string{}
		for _, ref := range c.Roles {
			r, ok := byName[ref]
			if !ok {
				continue
			}
			if r.Optional && r.Status == StatusOffline {
				continue // optional offline doesn't drag the rollup
			}
			if r.Status != StatusOK {
				down = append(down, ref)
				worst = worseStatus(worst, r.Status)
			}
		}
		out = append(out, CapabilityStatus{
			Name:        name,
			Label:       c.Label,
			Description: c.Description,
			Status:      worst,
			Roles:       append([]string(nil), c.Roles...),
			Down:        down,
		})
	}
	return out
}

// worseStatus picks the more pessimistic of two statuses. Order:
// ok < unknown < degraded < offline.
func worseStatus(a, b Status) Status {
	rank := map[Status]int{
		StatusOK:       0,
		StatusUnknown:  1,
		StatusDegraded: 2,
		StatusOffline:  3,
	}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

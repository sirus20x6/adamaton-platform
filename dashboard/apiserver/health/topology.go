// Package health declares the fleet health model and the per-kind
// probe implementations used by the aggregator. Topology is the parsed
// shape of deploy/health/topology.yml — the authoritative graph of
// roles + capabilities the dashboard renders.
package health

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Kind discriminates how a Role is probed.
type Kind string

const (
	KindHTTP          Kind = "http"
	KindTCP           Kind = "tcp"
	KindPostgres      Kind = "postgres"
	KindRedis         Kind = "redis"
	KindTemporalQueue Kind = "temporal_queue"
)

// allKinds is the source of truth for accepted Kind values. Sorted for
// deterministic error messages.
var allKinds = []Kind{KindHTTP, KindPostgres, KindRedis, KindTCP, KindTemporalQueue}

// Probe carries the per-role probe configuration. Fields are populated
// per Kind; e.g. HTTP probes use Path + Port, TCP probes use Port only,
// temporal_queue uses none (the Role.Queue + HeartbeatMaxAge fields
// drive the probe).
type Probe struct {
	Host    string        `yaml:"host,omitempty"`
	Port    int           `yaml:"port,omitempty"`
	Path    string        `yaml:"path,omitempty"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// Role names a class of service the fleet runs. MinHealthy is the
// quorum threshold — a role's status is "ok" iff healthy>=MinHealthy.
type Role struct {
	Name        string `yaml:"-"` // populated from the map key
	Kind        Kind   `yaml:"kind"`
	Probe       Probe  `yaml:"probe,omitempty"`
	MinHealthy  int    `yaml:"min_healthy"`
	Optional    bool   `yaml:"optional,omitempty"`
	Description string `yaml:"description,omitempty"`

	// temporal_queue only.
	Queue           string        `yaml:"queue,omitempty"`
	HeartbeatMaxAge time.Duration `yaml:"heartbeat_max_age,omitempty"`
}

// Capability is a user-facing rollup of related roles. The capability's
// status is the worst-of its non-optional member roles.
type Capability struct {
	Name        string   `yaml:"-"` // populated from the map key
	Label       string   `yaml:"label"`
	Roles       []string `yaml:"roles"`
	Description string   `yaml:"description,omitempty"`
}

// Topology is the parsed deploy/health/topology.yml. Roles + Capabilities
// are maps for unmarshaling convenience; the slice forms (RoleOrder /
// CapabilityOrder) are populated by post-process for deterministic
// iteration order.
type Topology struct {
	Roles           map[string]Role       `yaml:"roles"`
	Capabilities    map[string]Capability `yaml:"capabilities"`
	RoleOrder       []string              `yaml:"-"`
	CapabilityOrder []string              `yaml:"-"`
}

// LoadTopology parses a YAML stream and validates it. Returns the
// fully populated Topology with sorted iteration orders.
func LoadTopology(r io.Reader) (*Topology, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read topology: %w", err)
	}
	var t Topology
	if err := yaml.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("parse topology yaml: %w", err)
	}
	if err := t.postProcess(); err != nil {
		return nil, err
	}
	return &t, nil
}

func (t *Topology) postProcess() error {
	if len(t.Roles) == 0 {
		return errors.New("topology: no roles defined")
	}
	if len(t.Capabilities) == 0 {
		return errors.New("topology: no capabilities defined")
	}

	t.RoleOrder = make([]string, 0, len(t.Roles))
	for name, role := range t.Roles {
		role.Name = name
		t.Roles[name] = role
		t.RoleOrder = append(t.RoleOrder, name)
	}
	sort.Strings(t.RoleOrder)

	t.CapabilityOrder = make([]string, 0, len(t.Capabilities))
	for name, cap := range t.Capabilities {
		cap.Name = name
		t.Capabilities[name] = cap
		t.CapabilityOrder = append(t.CapabilityOrder, name)
	}
	sort.Strings(t.CapabilityOrder)

	for _, name := range t.RoleOrder {
		if err := validateRole(t.Roles[name]); err != nil {
			return fmt.Errorf("role %q: %w", name, err)
		}
	}
	for _, name := range t.CapabilityOrder {
		if err := t.validateCapability(t.Capabilities[name]); err != nil {
			return fmt.Errorf("capability %q: %w", name, err)
		}
	}
	return nil
}

func validateRole(r Role) error {
	if !kindAllowed(r.Kind) {
		return fmt.Errorf("kind %q not one of %v", r.Kind, allKinds)
	}
	if r.MinHealthy < 0 {
		return fmt.Errorf("min_healthy %d < 0", r.MinHealthy)
	}
	switch r.Kind {
	case KindHTTP:
		if r.Probe.Port == 0 {
			return errors.New("http probe needs port")
		}
		if r.Probe.Path == "" {
			return errors.New("http probe needs path")
		}
	case KindTCP, KindRedis:
		if r.Probe.Port == 0 {
			return errors.New("tcp/redis probe needs port")
		}
	case KindTemporalQueue:
		if r.Queue == "" {
			return errors.New("temporal_queue needs queue name")
		}
		if r.HeartbeatMaxAge <= 0 {
			return errors.New("temporal_queue needs positive heartbeat_max_age")
		}
	case KindPostgres:
		// no probe config — pool ping comes from the apiserver's
		// existing evoPool.
	}
	return nil
}

func (t *Topology) validateCapability(c Capability) error {
	if c.Label == "" {
		return errors.New("label is required")
	}
	if len(c.Roles) == 0 {
		return errors.New("roles list is empty")
	}
	for _, ref := range c.Roles {
		if _, ok := t.Roles[ref]; !ok {
			return fmt.Errorf("references undefined role %q", ref)
		}
	}
	return nil
}

// TemporalQueues returns the deduplicated, sorted set of Temporal task
// queue names declared by temporal_queue roles. Used by the apiserver's
// queue-depth poller to know which queues to DescribeTaskQueue.
func (t *Topology) TemporalQueues() []string {
	seen := map[string]bool{}
	for _, name := range t.RoleOrder {
		role := t.Roles[name]
		if role.Kind == KindTemporalQueue && role.Queue != "" {
			seen[role.Queue] = true
		}
	}
	out := make([]string, 0, len(seen))
	for q := range seen {
		out = append(out, q)
	}
	sort.Strings(out)
	return out
}

func kindAllowed(k Kind) bool {
	for _, ok := range allKinds {
		if k == ok {
			return true
		}
	}
	return false
}

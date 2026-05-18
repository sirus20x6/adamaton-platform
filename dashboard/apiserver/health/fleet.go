package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// FleetClient talks to every host's deploy-agent. Lazily parses the
// ADAMATON_DEPLOY_AGENTS env var on first call (same convention as
// apiserver/nodes_endpoints.go).
//
// Token comes from DEPLOY_AGENT_TOKEN env if not pre-set.
type FleetClient struct {
	Token  string
	client *http.Client

	once sync.Once
	urls map[string]string // host -> "http://deploy-agent:9128" (no trailing slash)
}

// NewFleetClient builds a client with a sane default HTTP timeout.
func NewFleetClient() *FleetClient {
	return &FleetClient{
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Hosts returns the configured host list. Order is undefined.
func (c *FleetClient) Hosts() []string {
	c.ensureURLs()
	hosts := make([]string, 0, len(c.urls))
	for h := range c.urls {
		hosts = append(hosts, h)
	}
	return hosts
}

// AgentURL returns the deploy-agent base URL for a host, or "" if unknown.
func (c *FleetClient) AgentURL(host string) string {
	c.ensureURLs()
	return c.urls[host]
}

// Services calls GET /services on a host's deploy-agent. Returns the
// MANIFEST.yaml services allow-list (just names — not container
// details). Use Status() per service for the docker-ps view.
//
// Returns empty slice + nil error when the host has no agent configured,
// so callers can iterate without special-casing.
func (c *FleetClient) Services(ctx context.Context, host string) ([]string, error) {
	c.ensureURLs()
	base := c.urls[host]
	if base == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/services", nil)
	if err != nil {
		return nil, err
	}
	c.injectAuth(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("services: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("services: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Host     string   `json:"host"`
		Services []string `json:"services"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("services: decode: %w", err)
	}
	return body.Services, nil
}

// ContainerStatus is the subset of `docker compose ps --format json` we
// surface to the dashboard. The agent returns the raw docker output, so
// we parse only the fields the UI uses. Extra fields are ignored.
type ContainerStatus struct {
	Name    string `json:"Name"`
	Image   string `json:"Image"`
	State   string `json:"State"`  // "running", "exited", "restarting"
	Status  string `json:"Status"` // "Up 7 hours (healthy)"
	Health  string `json:"Health"` // "healthy" | "unhealthy" | "" | "starting"
	Service string `json:"Service"`
}

// Status calls GET /status?service=X on a host's deploy-agent. The
// agent returns `docker compose ps <svc> --format json`, which can be:
//   - a single JSON object (one container)
//   - an array (multiple replicas)
//   - newline-delimited JSON (older docker compose)
//
// We accept all three to keep the dashboard tolerant.
func (c *FleetClient) Status(ctx context.Context, host, service string) ([]ContainerStatus, error) {
	c.ensureURLs()
	base := c.urls[host]
	if base == "" {
		return nil, nil
	}
	u := base + "/status?service=" + url.QueryEscape(service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.injectAuth(req)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// agent returns 502 with {"error": ...} when compose fails;
		// treat as "no instances" rather than propagating — the
		// aggregator will report the role as offline naturally.
		return nil, nil
	}
	return parseComposeStatus(resp.Body)
}

func (c *FleetClient) ensureURLs() {
	c.once.Do(func() {
		c.urls = parseAgentURLs(os.Getenv("ADAMATON_DEPLOY_AGENTS"))
		if c.Token == "" {
			c.Token = os.Getenv("DEPLOY_AGENT_TOKEN")
		}
	})
}

func (c *FleetClient) injectAuth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// parseAgentURLs takes the raw `host=url,host=url` value of
// ADAMATON_DEPLOY_AGENTS and returns the host -> base-URL map. Mirrors
// nodes_endpoints.go:deployAgentURLs. Kept as a free function for
// testability (no env-var reach-arounds).
func parseAgentURLs(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 || eq == len(pair)-1 {
			continue
		}
		host := strings.TrimSpace(pair[:eq])
		url := strings.TrimRight(strings.TrimSpace(pair[eq+1:]), "/")
		if host != "" && url != "" {
			out[host] = url
		}
	}
	return out
}

// parseComposeStatus handles three docker compose ps output shapes:
//
//	{"Name":...}                      single object
//	[{"Name":...}, {"Name":...}]       JSON array
//	{"Name":...}\n{"Name":...}         newline-delimited (older docker)
func parseComposeStatus(r interface {
	Read(p []byte) (n int, err error)
}) ([]ContainerStatus, error) {
	dec := json.NewDecoder(r)
	// peek at the first token to decide the shape
	tok, err := dec.Token()
	if err != nil {
		return nil, nil // empty body
	}
	switch v := tok.(type) {
	case json.Delim:
		if v == '[' {
			// JSON array — re-decode the rest as []ContainerStatus.
			out := []ContainerStatus{}
			for dec.More() {
				var c ContainerStatus
				if err := dec.Decode(&c); err != nil {
					return nil, fmt.Errorf("parse compose ps array: %w", err)
				}
				out = append(out, c)
			}
			return out, nil
		}
		if v == '{' {
			// Single object (or newline-delimited; the first object
			// is the one we just opened). Re-stream from the start —
			// json.Decoder will eat consecutive top-level objects.
			// To do that we need to re-set the decoder, but we've
			// already consumed the '{'. Build the first object via a
			// temp decoder over a reconstructed reader.
			var first ContainerStatus
			if err := decodeRestOfObject(dec, &first); err != nil {
				return nil, fmt.Errorf("parse compose ps obj: %w", err)
			}
			out := []ContainerStatus{first}
			for {
				tok, err := dec.Token()
				if err != nil {
					break
				}
				if d, ok := tok.(json.Delim); ok && d == '{' {
					var c ContainerStatus
					if err := decodeRestOfObject(dec, &c); err != nil {
						return nil, fmt.Errorf("parse compose ps stream: %w", err)
					}
					out = append(out, c)
				}
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("unexpected token: %v", tok)
}

// decodeRestOfObject consumes a key/value stream into target until the
// closing '}'. Used by parseComposeStatus when we've already consumed
// the opening '{'.
func decodeRestOfObject(dec *json.Decoder, target *ContainerStatus) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok && d == '}' {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			continue
		}
		// crude reader — only pulls the fields we care about.
		var raw any
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		s, _ := raw.(string)
		switch key {
		case "Name":
			target.Name = s
		case "Image":
			target.Image = s
		case "State":
			target.State = s
		case "Status":
			target.Status = s
		case "Health":
			target.Health = s
		case "Service":
			target.Service = s
		}
	}
}

// ErrNoFleet signals that no ADAMATON_DEPLOY_AGENTS map was configured.
// The aggregator can still build a "local probe only" picture in this
// case — used for workstation dev where there's no fleet.
var ErrNoFleet = errors.New("no deploy-agent map configured")

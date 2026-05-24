// Per-host "spin up another worker" endpoints used by the Nodes page's
// scale dialog.
//
//	GET  /api/v1/nodes/{host}/scalable     headroom + per-service
//	                                       resource estimates for the
//	                                       worker types this host can
//	                                       spin up another instance of.
//	POST /api/v1/nodes/{host}/scale        proxies to the host's
//	                                       deploy-agent /scale endpoint
//	                                       with the bearer token added
//	                                       server-side so it never
//	                                       touches the browser.
//
// The estimate is derived from live evo.workers telemetry of the same
// `identity` across the entire fleet — there's no static resource
// declaration anywhere. If the only known worker of that type runs on
// this host the estimate is its current draw; if it's on a peer host
// the peer's draw is used; if no worker of that identity exists yet,
// a conservative default is returned with source="default" so the UI
// can show "(no live data — using default)".
//
// Host -> deploy-agent URL mapping is read from the ADAMATON_DEPLOY_AGENTS
// environment variable, formatted as a comma-separated host=url list:
//
//	ADAMATON_DEPLOY_AGENTS=pi5=http://deploy-agent:9128,pi5-speaker=http://pi5-speaker.local:9128
//
// (Different forms per host because the dashboard runs as a docker
// compose service on pi5, so its peer deploy-agent is reachable as the
// compose service name; other hosts need a LAN-routable URL.)
package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// scaleResponseShape lines up with the deploy-agent's /scale JSON.
type scalableEstimate struct {
	CPUPct float64 `json:"cpu_pct"`
	RAMGB  float64 `json:"ram_gb"`
	Source string  `json:"source"` // live:<id> | average | default
}

type scalableEntry struct {
	Service     string           `json:"service"`
	Running     int              `json:"running"`
	Provisioned bool             `json:"provisioned"`
	Estimate    scalableEstimate `json:"estimate"`
}

type headroomBlock struct {
	CPUPctFree float64 `json:"cpu_pct_free"`
	RAMGBFree  float64 `json:"ram_gb_free"`
	RAMGBTotal float64 `json:"ram_gb_total"`
	Source     string  `json:"source"` // live:<id> | unknown
}

type scalableResponse struct {
	Host     string          `json:"host"`
	Headroom headroomBlock   `json:"headroom"`
	Scalable []scalableEntry `json:"scalable"`
}

const (
	defaultEstimateCPUPct = 5.0
	defaultEstimateRAMGB  = 0.5
)

// isScalableService mirrors the deploy-agent's rule (must agree —
// see platform/deploy-agent/cmd/deploy-agent/main.go isScalable).
// v1: suffix "-worker"; figure-renderer / plugin-host / postgres are
// not scalable here.
func isScalableService(svc string) bool {
	return strings.HasSuffix(svc, "-worker") && svc != "deploy-agent"
}

// deployAgentURLs parses ADAMATON_DEPLOY_AGENTS once. Returns the host
// -> base URL map (no trailing slash). The dashboard process can run
// without this env var set (in which case scale endpoints return 503
// with a clear "deploy-agent map unconfigured" error) so that local
// dev doesn't have to wire it up to use the rest of the API.
var (
	deployAgentURLsOnce sync.Once
	deployAgentURLsVal  map[string]string
)

func deployAgentURLs() map[string]string {
	deployAgentURLsOnce.Do(func() {
		raw := os.Getenv("ADAMATON_DEPLOY_AGENTS")
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
		deployAgentURLsVal = out
	})
	return deployAgentURLsVal
}

// registerNodesEndpoints wires the two routes under /api/v1/nodes.
// Called from server.go alongside registerRacksEndpoint.
func (s *APIServer) registerNodesEndpoints(api *mux.Router) {
	api.HandleFunc("/nodes/{host}/scalable", s.getNodeScalable).Methods("GET")
	api.HandleFunc("/nodes/{host}/scale", s.postNodeScale).Methods("POST")
	api.HandleFunc("/nodes/{host}/provision", s.postNodeProvision).Methods("POST")
}

// fetchAgentCatalog asks the host's deploy-agent for its worker-types
// catalog (the set of worker services it knows how to provision). Used
// by getNodeScalable to compose the union of "already running" +
// "available to provision." Empty slice on any error so the existing
// MANIFEST workers still render even if the agent is unreachable.
func fetchAgentCatalog(ctx context.Context, baseURL, token string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/catalog", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Workers []string `json:"workers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	return body.Workers
}

// resolveRack looks up a rack manifest by host or alias. Returns the
// first match. nil + false means the host is not known to racks.yaml.
func resolveRack(host string) (rackManifest, bool) {
	manifests, err := loadRackManifests()
	if err != nil {
		return rackManifest{}, false
	}
	for _, m := range manifests {
		if m.Host == host {
			return m, true
		}
		for _, a := range m.Aliases {
			if a == host {
				return m, true
			}
		}
	}
	return rackManifest{}, false
}

// loadActiveWorkers reads every active worker once for the estimate +
// headroom computation. evoPool nil → empty slice (estimates fall
// through to defaults; headroom reports source=unknown).
func (s *APIServer) loadActiveWorkers(ctx context.Context) ([]Worker, error) {
	if s.evoPool == nil {
		return nil, nil
	}
	// Only count workers whose heartbeat is fresh: a crashed worker keeps
	// its stored status = 'active' (no reaper writes 'offline'), so without
	// the freshness predicate dead rows would inflate the active count and
	// the headroom estimate. The interval mirrors workerHeartbeatMaxAgeSeconds
	// / topology.yml's heartbeat_max_age (90s).
	rows, err := s.evoPool.Query(ctx, workersSelectSQL+fmt.Sprintf(`
WHERE w.status = 'active'
  AND w.last_heartbeat > NOW() - make_interval(secs => %d)
ORDER BY w.identity ASC`, workerHeartbeatMaxAgeSeconds))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Worker{}
	for rows.Next() {
		wk, scanErr := scanWorker(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, wk)
	}
	return out, nil
}

// computeHeadroom picks a representative worker for the host (matched
// by hostname OR rack alias) and reports its current CPU% / RAM-used
// vs total. Every worker on a host samples the same /proc, so any one
// of them is a valid host-wide readout. Returns "unknown" source when
// no live worker is present (e.g. blackwell has no registered worker
// yet — caller can still spin up the first one).
func computeHeadroom(host string, rack rackManifest, workers []Worker) headroomBlock {
	candidates := append([]string{rack.Host, host}, rack.Aliases...)
	for i := range workers {
		w := &workers[i]
		matched := false
		for _, alias := range candidates {
			if alias != "" && w.Hostname == alias {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		ram := 0.0
		if w.RAMGB != nil {
			ram = float64(*w.RAMGB)
		}
		used := 0.0
		if w.RAMUsedGB != nil {
			used = *w.RAMUsedGB
		}
		cpuPct := 0.0
		if w.CPUPct != nil {
			cpuPct = *w.CPUPct
		}
		return headroomBlock{
			CPUPctFree: clamp(100.0-cpuPct, 0, 100),
			RAMGBFree:  clamp(ram-used, 0, ram),
			RAMGBTotal: ram,
			Source:     "live:" + w.ID,
		}
	}
	return headroomBlock{Source: "unknown"}
}

// estimateFor produces the per-identity resource estimate the dialog
// shows. Preference order:
//  1. live readout from a worker on THIS host
//  2. average across all active workers of the same identity (fleet-wide)
//  3. conservative default constants
func estimateFor(identity string, host string, rack rackManifest, workers []Worker) (scalableEstimate, int) {
	candidates := append([]string{rack.Host, host}, rack.Aliases...)
	hostsHere := map[string]bool{}
	for _, c := range candidates {
		if c != "" {
			hostsHere[c] = true
		}
	}

	var localHit *Worker
	cpuSum, ramSum := 0.0, 0.0
	cpuCount, ramCount := 0, 0
	running := 0
	for i := range workers {
		w := &workers[i]
		if w.Identity != identity {
			continue
		}
		if hostsHere[w.Hostname] {
			if localHit == nil {
				localHit = w
			}
			running++
		}
		if w.CPUPct != nil {
			cpuSum += *w.CPUPct
			cpuCount++
		}
		if w.RAMUsedGB != nil {
			ramSum += *w.RAMUsedGB
			ramCount++
		}
	}

	if localHit != nil && localHit.CPUPct != nil && localHit.RAMUsedGB != nil {
		return scalableEstimate{
			CPUPct: *localHit.CPUPct,
			RAMGB:  *localHit.RAMUsedGB,
			Source: "live:" + localHit.ID,
		}, running
	}
	if cpuCount > 0 && ramCount > 0 {
		return scalableEstimate{
			CPUPct: cpuSum / float64(cpuCount),
			RAMGB:  ramSum / float64(ramCount),
			Source: "average",
		}, running
	}
	return scalableEstimate{
		CPUPct: defaultEstimateCPUPct,
		RAMGB:  defaultEstimateRAMGB,
		Source: "default",
	}, running
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (s *APIServer) getNodeScalable(w http.ResponseWriter, r *http.Request) {
	host := mux.Vars(r)["host"]
	rack, ok := resolveRack(host)
	if !ok {
		writeEvoErr(w, http.StatusNotFound, "unknown host: "+host)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	workers, err := s.loadActiveWorkers(ctx)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "load workers: "+err.Error())
		return
	}

	resp := scalableResponse{
		Host:     rack.Host,
		Headroom: computeHeadroom(host, rack, workers),
		Scalable: make([]scalableEntry, 0, len(rack.Workers)),
	}
	// Provisioned: workers in the rack's MANIFEST (declared in
	// docker-compose.yml at deploy time). These already have service
	// blocks; /scale handles them.
	seen := map[string]bool{}
	for _, svc := range rack.Workers {
		if !isScalableService(svc) {
			continue
		}
		seen[svc] = true
		est, running := estimateFor(svc, host, rack, workers)
		resp.Scalable = append(resp.Scalable, scalableEntry{
			Service:     svc,
			Running:     running,
			Provisioned: true,
			Estimate:    est,
		})
	}
	// Unprovisioned: worker types the host's deploy-agent advertises
	// in its catalog but that aren't in MANIFEST yet. The dialog
	// dispatches /provision (vs /scale) for these.
	if urls := deployAgentURLs(); urls[rack.Host] != "" {
		token := os.Getenv("DEPLOY_AGENT_TOKEN")
		acatalog := fetchAgentCatalog(ctx, urls[rack.Host], token)
		for _, svc := range acatalog {
			if seen[svc] || !isScalableService(svc) {
				continue
			}
			seen[svc] = true
			est, _ := estimateFor(svc, host, rack, workers)
			resp.Scalable = append(resp.Scalable, scalableEntry{
				Service:     svc,
				Running:     0,
				Provisioned: false,
				Estimate:    est,
			})
		}
	}
	writeEvoJSON(w, resp)
}

// postNodeProvision proxies to the host's deploy-agent /provision,
// which renders a compose service block from the embedded catalog
// before bringing the container up. Used by the dialog when the
// picked service has provisioned=false.
func (s *APIServer) postNodeProvision(w http.ResponseWriter, r *http.Request) {
	host := mux.Vars(r)["host"]
	rack, ok := resolveRack(host)
	if !ok {
		writeEvoErr(w, http.StatusNotFound, "unknown host: "+host)
		return
	}
	urls := deployAgentURLs()
	base, ok := urls[rack.Host]
	if !ok {
		writeEvoErr(w, http.StatusServiceUnavailable,
			"no deploy-agent URL for host "+rack.Host+" (set ADAMATON_DEPLOY_AGENTS env)")
		return
	}
	token := os.Getenv("DEPLOY_AGENT_TOKEN")
	if token == "" {
		writeEvoErr(w, http.StatusServiceUnavailable, "DEPLOY_AGENT_TOKEN not set on dashboard")
		return
	}

	svc := r.URL.Query().Get("service")
	replicas := r.URL.Query().Get("replicas")
	if svc == "" || replicas == "" {
		writeEvoErr(w, http.StatusBadRequest, "service= and replicas= are both required")
		return
	}
	if !isScalableService(svc) {
		writeEvoErr(w, http.StatusBadRequest, "service not scalable: "+svc)
		return
	}

	upstreamURL := fmt.Sprintf("%s/provision?service=%s&replicas=%s", base, svc, replicas)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, nil)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "build request: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "deploy-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "deploy-agent body: "+err.Error())
		return
	}
	ct := resp.Header.Get("Content-Type")
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	if strings.HasPrefix(ct, "application/json") {
		_, _ = w.Write(body)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":         strings.TrimSpace(string(body)),
		"upstream_code": resp.StatusCode,
		"upstream_host": rack.Host,
	})
}

// postNodeScale is the browser-facing proxy: it injects the deploy-agent
// bearer token (which lives in env on the dashboard process) and forwards
// the call. Returns the agent's JSON verbatim, preserving status codes
// so the dialog can display validation errors as-is.
func (s *APIServer) postNodeScale(w http.ResponseWriter, r *http.Request) {
	host := mux.Vars(r)["host"]
	rack, ok := resolveRack(host)
	if !ok {
		writeEvoErr(w, http.StatusNotFound, "unknown host: "+host)
		return
	}
	urls := deployAgentURLs()
	base, ok := urls[rack.Host]
	if !ok {
		writeEvoErr(w, http.StatusServiceUnavailable,
			"no deploy-agent URL for host "+rack.Host+" (set ADAMATON_DEPLOY_AGENTS env)")
		return
	}
	token := os.Getenv("DEPLOY_AGENT_TOKEN")
	if token == "" {
		writeEvoErr(w, http.StatusServiceUnavailable, "DEPLOY_AGENT_TOKEN not set on dashboard")
		return
	}

	svc := r.URL.Query().Get("service")
	replicas := r.URL.Query().Get("replicas")
	if svc == "" || replicas == "" {
		writeEvoErr(w, http.StatusBadRequest, "service= and replicas= are both required")
		return
	}
	if !isScalableService(svc) {
		writeEvoErr(w, http.StatusBadRequest, "service not scalable: "+svc)
		return
	}

	upstreamURL := fmt.Sprintf("%s/scale?service=%s&replicas=%s", base, svc, replicas)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, nil)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "build request: "+err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "deploy-agent unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "deploy-agent body: "+err.Error())
		return
	}
	// Preserve content type if the agent returned JSON; otherwise wrap
	// the agent's plain-text error into the {error: ...} shape the SPA
	// already handles.
	ct := resp.Header.Get("Content-Type")
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	if strings.HasPrefix(ct, "application/json") {
		_, _ = w.Write(body)
		return
	}
	// Re-shape non-JSON (the deploy-agent uses http.Error for validation
	// failures which returns text/plain).
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":         strings.TrimSpace(string(body)),
		"upstream_code": resp.StatusCode,
		"upstream_host": rack.Host,
	})
}

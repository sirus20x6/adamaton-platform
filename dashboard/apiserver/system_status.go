// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
package apiserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirus20x6/adamaton-core/tracectx"
)

// osGetenv is a tiny indirection that exists so tests can intercept
// env-var reads if they ever need to. Today it's just os.Getenv.
func osGetenv(key string) string { return os.Getenv(key) }

// envBool reads an env var and returns true for "1", "true", "yes",
// "on" (case-insensitive). Anything else (including unset) is false.
// Used by feature flags that must default to the secure choice.
func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// insecureTLS is read once at package init from EVO_DASHBOARD_TLS_INSECURE.
// When false (the default), the dashboard's outbound HTTPS clients
// verify the upstream certificate. Operators on a LAN with self-signed
// Caddy certs can opt in by setting EVO_DASHBOARD_TLS_INSECURE=1.
var insecureTLS = envBool("EVO_DASHBOARD_TLS_INSECURE")

// SubsystemStatus is the per-tool health blob. Status is one of
// "ok", "degraded", "offline"; the UI uses it to colour the pill.
// Stats is a free-form map so each subsystem can attach whatever
// counters are useful (recent runs, quota %, etc.) without
// constraining the shape.
type SubsystemStatus struct {
	Name      string                 `json:"name"`
	Status    string                 `json:"status"`
	Detail    string                 `json:"detail,omitempty"`
	URL       string                 `json:"url,omitempty"`
	LatencyMS float64                `json:"latency_ms"`
	Stats     map[string]interface{} `json:"stats,omitempty"`
}

// SystemStatus is the top-level response of /api/v1/system/status.
type SystemStatus struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Subsystems  []SubsystemStatus `json:"subsystems"`
}

// handleSystemStatus fans out per-subsystem checks in parallel with a
// 2-second per-check timeout. The whole call is bounded to ~3s so the
// landing page's auto-refresh never stalls. Each check builds its own
// SubsystemStatus independently — one slow probe doesn't poison the
// others.
func (s *APIServer) handleSystemStatus(w http.ResponseWriter, r *http.Request) {
	// Parent fan-out budget. Every sub-check now runs with a 2s ceiling,
	// so 5s is enough headroom for goroutine schedule + JSON-encode.
	parentCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var (
		wg      sync.WaitGroup
		results = make([]SubsystemStatus, 6)
	)

	wg.Add(6)
	go func() { defer wg.Done(); results[0] = s.checkDelegator(parentCtx) }()
	go func() { defer wg.Done(); results[1] = s.checkSkills(parentCtx) }()
	go func() { defer wg.Done(); results[2] = s.checkEvo(parentCtx) }()
	go func() { defer wg.Done(); results[3] = s.checkWorkflows(parentCtx) }()
	go func() { defer wg.Done(); results[4] = s.checkDeepResearch(parentCtx) }()
	go func() { defer wg.Done(); results[5] = s.checkVLLM(parentCtx) }()
	wg.Wait()

	out := SystemStatus{
		GeneratedAt: time.Now(),
		Subsystems:  results,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// checkDelegator: count of tasks in the delegator store. Status is
// ok when the store responds, offline when it's nil or errors.
func (s *APIServer) checkDelegator(ctx context.Context) SubsystemStatus {
	start := time.Now()
	st := SubsystemStatus{Name: "delegator", URL: dashboardHref("/delegator")}
	if s.delegatorStore == nil {
		st.Status = "offline"
		st.Detail = "delegator store not configured"
		st.LatencyMS = ms(start)
		return st
	}
	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if s.evoPool == nil {
		// Without evoPool we don't have a fallback pool to count
		// directly — rely on the store's own queryability as the
		// health signal.
		_ = subCtx // store List doesn't accept ctx; passthrough symmetry.
		tasks := s.delegatorStore.List("", "")
		st.Status = "ok"
		st.Stats = map[string]interface{}{"recent_tasks": len(tasks)}
		st.LatencyMS = ms(start)
		return st
	}
	// Prefer a direct count via evoPool — single fast query, no
	// per-row decoding overhead.
	var n int
	err := s.evoPool.QueryRow(subCtx, `SELECT count(*) FROM delegator.tasks`).Scan(&n)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	st.Status = "ok"
	st.Stats = map[string]interface{}{"task_count": n}
	st.LatencyMS = ms(start)
	return st
}

// skillsCountsTTL bounds the staleness of the cached skill-library
// aggregates. The landing page auto-refreshes every ~2s; without a cache
// each refresh runs four full-scan COUNTs over evo.skills / evo.skill_usages
// (a bottleneck past 100k usages). A 15s TTL means at most one of every
// ~7 landing-page hits actually touches Postgres, and the displayed
// numbers are never more than 15s stale.
const skillsCountsTTL = 15 * time.Second

// skillsCounts is the cached result of the four-subquery aggregate.
type skillsCounts struct {
	skills      int
	communities int
	usages      int
	recentTasks int
}

// skillsCountsCache is a process-wide single-entry cache for checkSkills.
// It is keyed only by time (there is exactly one skills aggregate), so a
// single mutex-guarded slot suffices. A singleflight-style refreshing flag
// keeps a thundering herd of concurrent /system/status calls from all
// issuing the COUNT query at once when the entry expires; late arrivals
// serve the (slightly) stale value instead of piling onto Postgres.
var skillsCountsCache struct {
	mu         sync.Mutex
	val        skillsCounts
	fetchedAt  time.Time
	valid      bool
	refreshing bool
}

// skillsCountsCacheTestReset clears the cache so tests can assert on a
// clean miss→hit→refresh sequence without cross-test bleed.
func skillsCountsCacheTestReset() {
	skillsCountsCache.mu.Lock()
	skillsCountsCache.valid = false
	skillsCountsCache.refreshing = false
	skillsCountsCache.fetchedAt = time.Time{}
	skillsCountsCache.val = skillsCounts{}
	skillsCountsCache.mu.Unlock()
}

// querySkillsCounts runs the four-subquery aggregate. Split out from
// checkSkills so the cache can call it directly.
func (s *APIServer) querySkillsCounts(ctx context.Context) (skillsCounts, error) {
	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var c skillsCounts
	err := s.evoPool.QueryRow(subCtx, `
		SELECT
			(SELECT count(*) FROM evo.skills),
			(SELECT count(DISTINCT community) FROM evo.skills WHERE community IS NOT NULL),
			(SELECT count(*) FROM evo.skill_usages),
			(SELECT count(DISTINCT task_id) FROM evo.skill_usages WHERE used_at > NOW() - INTERVAL '24 hours')
	`).Scan(&c.skills, &c.communities, &c.usages, &c.recentTasks)
	return c, err
}

// cachedSkillsCounts returns the aggregate, refreshing through Postgres at
// most once per skillsCountsTTL. The bool reports whether the value was
// served from cache (true) vs freshly fetched (false) — surfaced in the
// status stats as "cached" so operators (and the cache-hit test) can see
// the cache working. On a query error with NO prior cached value, the
// error propagates so checkSkills can mark the subsystem offline; on a
// query error WITH a stale value present, the stale value is returned
// (degraded-but-available beats a spurious offline pill).
func (s *APIServer) cachedSkillsCounts(ctx context.Context) (skillsCounts, bool, error) {
	c := &skillsCountsCache
	c.mu.Lock()
	fresh := c.valid && time.Since(c.fetchedAt) < skillsCountsTTL
	if fresh {
		val := c.val
		c.mu.Unlock()
		return val, true, nil
	}
	// Entry is missing or stale. If another goroutine is already
	// refreshing and we have *some* value, serve it stale rather than
	// dogpiling Postgres.
	if c.refreshing && c.valid {
		val := c.val
		c.mu.Unlock()
		return val, true, nil
	}
	c.refreshing = true
	c.mu.Unlock()

	val, err := s.querySkillsCounts(ctx)

	c.mu.Lock()
	c.refreshing = false
	if err != nil {
		if c.valid {
			stale := c.val
			c.mu.Unlock()
			return stale, true, nil
		}
		c.mu.Unlock()
		return skillsCounts{}, false, err
	}
	c.val = val
	c.fetchedAt = time.Now()
	c.valid = true
	c.mu.Unlock()
	return val, false, nil
}

// checkSkills: skill library summary — total skills, communities, and
// recent usage. ok when the evo.skills tables exist + Postgres responds.
// Backed by a short-TTL in-process cache (see cachedSkillsCounts) so the
// landing page's ~2s refresh doesn't run four full-scan COUNTs every hit.
func (s *APIServer) checkSkills(ctx context.Context) SubsystemStatus {
	start := time.Now()
	st := SubsystemStatus{Name: "skills", URL: dashboardHref("/skills")}
	if s.evoPool == nil {
		st.Status = "offline"
		st.Detail = "evo pool not configured"
		st.LatencyMS = ms(start)
		return st
	}
	counts, cached, err := s.cachedSkillsCounts(ctx)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	st.Status = "ok"
	st.Stats = map[string]interface{}{
		"skills":      counts.skills,
		"communities": counts.communities,
		"usages":      counts.usages,
		"tasks_today": counts.recentTasks,
		"cached":      cached,
	}
	st.LatencyMS = ms(start)
	return st
}

// checkEvo: count of runs + correctness summary from evo schema.
func (s *APIServer) checkEvo(ctx context.Context) SubsystemStatus {
	start := time.Now()
	st := SubsystemStatus{Name: "evo", URL: dashboardHref("/evo")}
	if s.evoPool == nil {
		st.Status = "offline"
		st.Detail = "evo pool not configured"
		st.LatencyMS = ms(start)
		return st
	}
	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var runs, programs, insights int
	var bestSpeedup *float64
	err := s.evoPool.QueryRow(subCtx, `
		SELECT
			(SELECT count(*) FROM evo.runs),
			(SELECT count(*) FROM evo.programs),
			(SELECT count(*) FROM evo.insights),
			(SELECT max(speedup) FROM evo.programs WHERE correct)
	`).Scan(&runs, &programs, &insights, &bestSpeedup)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	st.Status = "ok"
	stats := map[string]interface{}{
		"runs":     runs,
		"programs": programs,
		"insights": insights,
	}
	if bestSpeedup != nil {
		stats["best_speedup"] = *bestSpeedup
	}
	st.Stats = stats
	st.LatencyMS = ms(start)
	return st
}

// checkWorkflows: definition + run counts via workflow.* schema.
// Uses evoPool's Postgres connection rather than the workflowstore's
// internal pool so we don't compete with the builder for writes.
func (s *APIServer) checkWorkflows(ctx context.Context) SubsystemStatus {
	start := time.Now()
	st := SubsystemStatus{Name: "workflows", URL: dashboardHref("/workflows")}
	if s.evoPool == nil {
		st.Status = "offline"
		st.Detail = "no Postgres pool"
		st.LatencyMS = ms(start)
		return st
	}
	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var defs, runs int
	err := s.evoPool.QueryRow(subCtx, `
		SELECT
			(SELECT count(*) FROM workflow.definitions),
			(SELECT count(*) FROM workflow.runs)
	`).Scan(&defs, &runs)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	st.Status = "ok"
	st.Stats = map[string]interface{}{
		"definitions": defs,
		"runs":        runs,
	}
	st.LatencyMS = ms(start)
	return st
}

// checkDeepResearch: GET against r2g's /health endpoint. r2g is the Go
// replacement for the retired Python R2R backend (per
// docs/WHERE_DID_IT_GO.md: "platform/backend/ — Python R2R-era code;
// functionality long since reimplemented in r2g + plugin-host"); the
// DEEPRESEARCH_URL env var on the Pi now points at http://r2g:7373.
// The subsystem label stays "deepresearch" to keep continuity with the
// SPA's status pill — it semantically still means "the deepresearch /
// RAG plane is up". InsecureSkipVerify is preserved for operators who
// still point this at the Caddy-fronted https://deepresearch.local on
// hosts that don't run r2g in-cluster.
func (s *APIServer) checkDeepResearch(ctx context.Context) SubsystemStatus {
	start := time.Now()
	base := s.deepResearchURL()
	probe := joinHealthURL(base, "/health")
	st := SubsystemStatus{Name: "deepresearch", URL: probe}
	if base == "" {
		st.Status = "offline"
		st.Detail = "DEEPRESEARCH_URL not configured"
		st.LatencyMS = ms(start)
		return st
	}
	// 2s is plenty: r2g's /health is a constant-time JSON write,
	// not a backend-gated check. (The old probe targeted FastAPI's
	// /platform/health which could be tied up by reindex subprocesses
	// and needed 8s.)
	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(subCtx, http.MethodGet, probe, nil)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	resp, err := s.deepResearchHTTPClient().Do(req)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 500:
		st.Status = "offline"
		st.Detail = resp.Status
	case resp.StatusCode >= 400:
		st.Status = "degraded"
		st.Detail = resp.Status
	default:
		st.Status = "ok"
	}
	st.Stats = map[string]interface{}{"http_status": resp.StatusCode}
	st.LatencyMS = ms(start)
	return st
}

// checkVLLM probes the workstation vLLM endpoint that R2R + the evo
// memory pipeline both rely on. Returns:
//
//   - “ok“       — endpoint responds and has served at least one
//     completion since process start.
//   - “degraded“ — endpoint responds but zero completions served
//     (likely idle / just-restarted / unwired).
//   - “offline“  — /v1/models 4xx/5xx or unreachable.
//
// We sample /v1/models for liveness + /metrics for the
// “vllm:request_success_total“ counter. Stats fan out:
//
//	model_name, requests_running, requests_waiting,
//	completions_total, prompt_tokens, generation_tokens, uptime_seconds.
//
// vLLM_URL env overrides the default (http://10.0.4.37:9080).
func (s *APIServer) checkVLLM(ctx context.Context) SubsystemStatus {
	start := time.Now()
	base := vllmURL()
	probe := strings.TrimRight(base, "/") + "/v1/models"
	st := SubsystemStatus{Name: "vllm", URL: probe}

	subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := sysVLLMHTTPClient()
	req, err := http.NewRequestWithContext(subCtx, http.MethodGet, probe, nil)
	if err != nil {
		st.Status = "offline"
		st.Detail = err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	resp, err := client.Do(req)
	if err != nil {
		st.Status = "offline"
		st.Detail = "unreachable: " + err.Error()
		st.LatencyMS = ms(start)
		return st
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
	if resp.StatusCode >= 400 {
		st.Status = "offline"
		st.Detail = "models endpoint HTTP " + resp.Status
		st.LatencyMS = ms(start)
		return st
	}

	stats := map[string]interface{}{}
	var modelsParsed struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &modelsParsed); err == nil && len(modelsParsed.Data) > 0 {
		stats["model"] = modelsParsed.Data[0].ID
		if modelsParsed.Data[0].Created > 0 {
			stats["uptime_seconds"] = time.Now().Unix() - modelsParsed.Data[0].Created
		}
	}

	// Pull /metrics for the counters we actually care about.
	metricsURL := strings.TrimRight(base, "/") + "/metrics"
	metricsCtx, metricsCancel := context.WithTimeout(ctx, 2*time.Second)
	defer metricsCancel()
	if mReq, err := http.NewRequestWithContext(metricsCtx, http.MethodGet, metricsURL, nil); err == nil {
		if mResp, err := client.Do(mReq); err == nil {
			defer mResp.Body.Close()
			if mResp.StatusCode == http.StatusOK {
				mBody, _ := io.ReadAll(io.LimitReader(mResp.Body, 1<<20))
				parseVLLMMetrics(string(mBody), stats)
			}
		}
	}

	st.Stats = stats
	completions, _ := stats["completions_total"].(float64)
	if completions > 0 {
		st.Status = "ok"
	} else {
		// Endpoint is up but has served zero completions since it
		// started — could be a fresh boot OR R2R/evo aren't actually
		// calling it. Either way, flag for the operator.
		st.Status = "degraded"
		st.Detail = "vLLM responding but 0 completions served since process start"
	}
	st.LatencyMS = ms(start)
	return st
}

// parseVLLMMetrics scans Prometheus-exposition lines for the few
// counters worth surfacing on the landing page. Kept inline (no
// dependency on a full Prom parser) — the metric names are stable and
// the format is line-oriented.
func parseVLLMMetrics(body string, out map[string]interface{}) {
	type want struct {
		prefix string
		key    string
		mode   string // "set" replaces; "add" accumulates (for split-label metrics)
	}
	wants := []want{
		{"vllm:num_requests_running{", "requests_running", "set"},
		{"vllm:num_requests_waiting{", "requests_waiting", "set"},
		{"vllm:request_success_total{", "completions_total", "add"},
		{"vllm:prompt_tokens_total{", "prompt_tokens", "set"},
		{"vllm:generation_tokens_total{", "generation_tokens", "set"},
	}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		sp := strings.LastIndex(line, " ")
		if sp < 0 {
			continue
		}
		valStr := line[sp+1:]
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		for _, w := range wants {
			if strings.HasPrefix(line, w.prefix) {
				if w.mode == "add" {
					prev, _ := out[w.key].(float64)
					out[w.key] = prev + val
				} else {
					out[w.key] = val
				}
				break
			}
		}
	}
}

// vllmURL returns the OPENAI_API_BASE / VLLM_URL env override, or the
// hardcoded workstation fallback. The /v1 suffix is stripped because
// /metrics doesn't live under it.
func vllmURL() string {
	v := osGetenv("VLLM_URL")
	if v == "" {
		v = osGetenv("OPENAI_API_BASE")
	}
	if v == "" {
		v = "http://10.0.4.37:9080"
	}
	v = strings.TrimRight(v, "/")
	v = strings.TrimSuffix(v, "/v1")
	return v
}

// joinHealthURL composes a probe URL by stripping trailing slashes
// from the base and ensuring exactly one leading slash on the path.
// Tolerates both "https://host" and "https://host/" inputs.
func joinHealthURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// deepResearchURL returns the operator-configured Pi URL.
// Resolution order: explicit YAML/config → DEEPRESEARCH_URL env →
// LAN default "https://deepresearch.local". The env-var fallback
// lets operators retarget the proxy without rewriting the config
// file (e.g. when bringing up a local DR for testing).
func (s *APIServer) deepResearchURL() string {
	if s.config != nil && s.config.DeepResearch.URL != "" {
		return s.config.DeepResearch.URL
	}
	if v := osGetenv("DEEPRESEARCH_URL"); v != "" {
		return v
	}
	return "https://deepresearch.local"
}

// deepResearchHTTPClient lazily builds an HTTPS client. TLS
// verification is on by default; operators on a LAN running a Pi
// behind a self-signed Caddy cert can opt out via
// EVO_DASHBOARD_TLS_INSECURE=1. Same client is reused across status
// checks + the proxy in research_proxy.go via the package-level
// singleton.
var (
	drClientOnce sync.Once
	drClient     *http.Client
)

func (s *APIServer) deepResearchHTTPClient() *http.Client {
	drClientOnce.Do(func() {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},
		}
		drClient = &http.Client{Transport: tracectx.NewTransport(tr), Timeout: 10 * time.Second}
	})
	return drClient
}

// vllmHTTPClient is a singleton client used by checkVLLM. We don't
// want to build a fresh http.Client + Transport on every status
// refresh — that defeats keep-alive and wastes connections.
var (
	vllmClientOnce sync.Once
	vllmClient     *http.Client
)

func sysVLLMHTTPClient() *http.Client {
	vllmClientOnce.Do(func() {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecureTLS},
		}
		vllmClient = &http.Client{Transport: tr, Timeout: 2 * time.Second}
	})
	return vllmClient
}

func ms(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}

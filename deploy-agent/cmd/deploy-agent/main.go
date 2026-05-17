// Command deploy-agent is the push-deploy receiver that runs on every
// Adamaton host (pi5, pi5-speaker, blackwell). It exposes a small HTTP
// API that the workstation's `bin/adam ship` calls after pushing a
// freshly-built image to the workstation's registry:
//
//	POST /restart?service=X&tag=Y      bump ADAMATON_X_TAG to Y in
//	                                   image-tags.env and run
//	                                   `docker compose pull X && up -d X`.
//	GET  /status?service=X             docker compose ps X JSON.
//	GET  /services                     allow-list from MANIFEST.yaml.
//	GET  /health                       unauth liveness probe.
//
// Auth is a single shared bearer token (DEPLOY_AGENT_TOKEN); every
// endpoint except /health requires it. Service names + tags are
// validated against regex BEFORE going near `docker compose` so the
// query string can't shell-inject into the subprocess.
//
// One deploy at a time: a mutex serialises every compose op. A 5-minute
// timeout protects against a stuck pull.
//
// The agent runs inside docker with the host's docker socket mounted,
// which makes it root-equivalent on the host -- the bearer token IS
// the security boundary. Caddy fronts it for TLS + a stable hostname;
// the agent itself binds to :9128 in-network.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// validTag bounds what a caller may pass as ?tag=. Image tags in the
// wild are wider than this, but for our purposes (sha-abc, main, v1.2.3)
// alphanumerics + . _ - cover every case we generate. Length cap stops
// pathological payloads.
var validTag = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

// validService matches MANIFEST.yaml service names. Looser than tag
// because compose service names can have hyphens but not underscores
// in the wild; we accept both for forward compatibility.
var validService = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// maxScaleReplicas caps /scale requests. Anything beyond 16 on a single
// rack is almost certainly an operator typo; the failure mode (OOM,
// runaway Temporal pollers) is unpleasant enough that we'd rather a
// 400 than a melted Pi.
const maxScaleReplicas = 16

// manifest mirrors the YAML schema in Adamaton/deploy/<host>/MANIFEST.yaml.
// We only consume Host + Services here; ImageTag is informational.
type manifest struct {
	Host     string   `yaml:"host"`
	ImageTag string   `yaml:"image_tag"`
	Services []string `yaml:"services"`
}

type server struct {
	composeDir string   // bind-mounted /workdir; holds docker-compose.yml + image-tags.env
	manifest   manifest // parsed once at startup
	allowed    map[string]struct{}
	token      string
	mu         sync.Mutex // serialises every docker compose op
	composeBin string     // "docker" so we can `docker compose ...`
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("deploy-agent: %v", err)
	}
}

func run() error {
	token := os.Getenv("DEPLOY_AGENT_TOKEN")
	if token == "" {
		return errors.New("DEPLOY_AGENT_TOKEN is required")
	}
	expectedHost := os.Getenv("DEPLOY_AGENT_HOST")
	if expectedHost == "" {
		return errors.New("DEPLOY_AGENT_HOST is required (must match MANIFEST.yaml host)")
	}
	composeDir := envOr("DEPLOY_AGENT_COMPOSE_DIR", "/workdir")
	bind := envOr("DEPLOY_AGENT_BIND", ":9128")

	manifestPath := filepath.Join(composeDir, "MANIFEST.yaml")
	mf, err := loadManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if mf.Host != expectedHost {
		return fmt.Errorf("manifest host mismatch: env=%q manifest=%q", expectedHost, mf.Host)
	}

	allow := make(map[string]struct{}, len(mf.Services))
	for _, s := range mf.Services {
		allow[s] = struct{}{}
	}

	s := &server{
		composeDir: composeDir,
		manifest:   mf,
		allowed:    allow,
		token:      token,
		composeBin: "docker",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/services", s.requireAuth(s.handleServices))
	mux.HandleFunc("/status", s.requireAuth(s.handleStatus))
	mux.HandleFunc("/restart", s.requireAuth(s.handleRestart))
	mux.HandleFunc("/restart-all", s.requireAuth(s.handleRestartAll))
	mux.HandleFunc("/scale", s.requireAuth(s.handleScale))
	mux.HandleFunc("/provision", s.requireAuth(s.handleProvision))
	mux.HandleFunc("/catalog", s.requireAuth(s.handleCatalog))

	srv := &http.Server{
		Addr:              bind,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("deploy-agent listening on %s for host=%s services=%d", bind, mf.Host, len(mf.Services))
	return srv.ListenAndServe()
}

func loadManifest(path string) (manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return manifest{}, err
	}
	if m.Host == "" {
		return manifest{}, errors.New("manifest: host is empty")
	}
	if len(m.Services) == 0 {
		return manifest{}, errors.New("manifest: services list is empty")
	}
	return m, nil
}

// requireAuth wraps a handler with bearer-token enforcement.
// Constant-time compare so the response time doesn't leak token bytes.
func (s *server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	expected := "Bearer " + s.token
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *server) handleServices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"host":     s.manifest.Host,
		"services": s.manifest.Services,
	})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	svc := r.URL.Query().Get("service")
	if !validService.MatchString(svc) {
		http.Error(w, "invalid service name", http.StatusBadRequest)
		return
	}
	if _, ok := s.allowed[svc]; !ok {
		http.Error(w, "service not in allow-list", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := s.compose(ctx, "ps", svc, "--format", "json")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "output": string(out)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (s *server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	svc := r.URL.Query().Get("service")
	tag := r.URL.Query().Get("tag")
	if !validService.MatchString(svc) {
		http.Error(w, "invalid service name", http.StatusBadRequest)
		return
	}
	if !validTag.MatchString(tag) {
		http.Error(w, "invalid tag", http.StatusBadRequest)
		return
	}
	if _, ok := s.allowed[svc]; !ok {
		http.Error(w, "service not in allow-list", http.StatusBadRequest)
		return
	}
	// Refuse to redeploy ourselves; would cut the connection mid-call.
	if svc == "deploy-agent" {
		http.Error(w, "deploy-agent self-update must use ssh (see bin/adam ship-self)", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("restart: service=%s tag=%s", svc, tag)
	tagsPath := filepath.Join(s.composeDir, "image-tags.env")
	if err := upsertTag(tagsPath, svc, tag); err != nil {
		http.Error(w, "write image-tags.env: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	pullOut, err := s.compose(ctx, "pull", svc)
	if err != nil {
		log.Printf("restart: pull failed svc=%s tag=%s err=%v", svc, tag, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"step": "pull", "error": err.Error(), "output": string(pullOut)})
		return
	}
	// --no-deps so a single-service ship doesn't recreate the rest of the
	// stack (postgres, temporal, etc.). Without this, any drift in their
	// compose blocks since the last `compose up` triggers a full recreate
	// cascade -- which is how a ship of evo-api once cycled postgres.
	//
	// --force-recreate because `up -d` only recreates a container when the
	// service block's config hash changes. Bumping a tag in image-tags.env
	// updates the resolved image: ref but the config hash compose computes
	// is based on the service block itself (which didn't change), so the
	// old container stays running with the old image. --force-recreate
	// makes the recreate unconditional once compose is invoked at all.
	upOut, err := s.compose(ctx, "up", "-d", "--no-deps", "--force-recreate", svc)
	if err != nil {
		log.Printf("restart: up failed svc=%s tag=%s err=%v", svc, tag, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"step": "up", "error": err.Error(), "output": string(upOut)})
		return
	}
	log.Printf("restart: ok svc=%s tag=%s", svc, tag)
	writeJSON(w, http.StatusOK, map[string]any{
		"service": svc,
		"tag":     tag,
		"pull":    tail(string(pullOut), 50),
		"up":      tail(string(upOut), 50),
	})
}

// isScalable decides whether a service is allowed through /scale. v1
// rule: anything whose name ends in "-worker". The "-worker" suffix is
// the convention every Temporal worker in the fleet already follows
// (skills-worker, dispatch-worker, nano-research-worker, evo-worker,
// reindex-worker, skills-rae-worker). These share two properties that
// make horizontal scaling safe: they are stateless consumers of a
// shared queue (Temporal balances tasks across them) and they don't
// bind a host port. Non-worker services (postgres, caddy, frontend,
// nano-research, plugin-host, etc.) DO have port/state assumptions
// and would either fail to start a second replica or silently break
// the first one, so they're refused here. A future MANIFEST.yaml
// `scalable:` list could promote specific non-suffix services, but
// for v1 the heuristic is sufficient and operator-friendly.
func isScalable(svc string) bool {
	return strings.HasSuffix(svc, "-worker")
}

// countReplicas returns the number of running containers for svc by
// listing IDs from `docker compose ps -q <svc>` and counting non-empty
// lines. Returns (0, nil) when the service has no containers (not an
// error -- legitimate state when scaling from cold).
func (s *server) countReplicas(ctx context.Context, svc string) (int, error) {
	out, err := s.compose(ctx, "ps", "-q", svc)
	if err != nil {
		return 0, fmt.Errorf("ps -q %s: %w (%s)", svc, err, tail(string(out), 10))
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}

func (s *server) handleScale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	svc := r.URL.Query().Get("service")
	repStr := r.URL.Query().Get("replicas")
	if !validService.MatchString(svc) {
		http.Error(w, "invalid service name", http.StatusBadRequest)
		return
	}
	if _, ok := s.allowed[svc]; !ok {
		http.Error(w, "service not in allow-list", http.StatusBadRequest)
		return
	}
	if !isScalable(svc) {
		http.Error(w, "service not scalable (v1: must end in -worker)", http.StatusBadRequest)
		return
	}
	if svc == "deploy-agent" {
		// Belt + braces: the suffix check already rejects it, but make the
		// intent loud.
		http.Error(w, "deploy-agent cannot scale itself", http.StatusBadRequest)
		return
	}
	// Parse replicas as integer manually rather than pulling in strconv-
	// style flexibility (no underscores, no signs, no leading zeros).
	// We want to accept exactly the strings 1..16.
	n := 0
	for _, c := range repStr {
		if c < '0' || c > '9' {
			http.Error(w, "replicas must be an integer 1.."+fmt.Sprintf("%d", maxScaleReplicas), http.StatusBadRequest)
			return
		}
		n = n*10 + int(c-'0')
		if n > maxScaleReplicas {
			break
		}
	}
	if n < 1 || n > maxScaleReplicas {
		http.Error(w, fmt.Sprintf("replicas must be in [1, %d]", maxScaleReplicas), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	before, err := s.countReplicas(ctx, svc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"step": "ps-before", "error": err.Error()})
		return
	}

	log.Printf("scale: service=%s before=%d target=%d", svc, before, n)
	// --no-recreate keeps existing containers running while compose adds
	// only the new ones needed to reach N. Without it, compose would
	// recreate every container of svc (because it computes a "should I
	// recreate?" decision from the service block, not from the running
	// count) -- which would briefly drop ALL workers of this type while
	// the new ones come up. --no-deps prevents postgres/temporal cascades
	// for the same reason as /restart.
	upOut, err := s.compose(ctx, "up", "-d", "--no-deps", "--no-recreate",
		"--scale", fmt.Sprintf("%s=%d", svc, n), svc)
	if err != nil {
		log.Printf("scale: up failed svc=%s target=%d err=%v", svc, n, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"step":   "up",
			"error":  err.Error(),
			"output": tail(string(upOut), 30),
		})
		return
	}
	after, err := s.countReplicas(ctx, svc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"step": "ps-after", "error": err.Error()})
		return
	}
	log.Printf("scale: ok svc=%s before=%d after=%d", svc, before, after)
	writeJSON(w, http.StatusOK, map[string]any{
		"service":   svc,
		"host":      s.manifest.Host,
		"before":    before,
		"after":     after,
		"requested": n,
		"compose":   tail(string(upOut), 30),
	})
}

// handleCatalog returns the embedded worker-types catalog as a sorted
// list of service names. Used by the dashboard to keep its UI-side
// catalog in lockstep with whatever the agent will actually accept.
func (s *server) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"host":    s.manifest.Host,
		"workers": catalogNames(),
	})
}

// handleProvision is the "scale a worker that may not yet have a
// compose service block" variant of /scale. Workflow:
//  1. Validate svc/replicas (same rules as /scale).
//  2. If svc is already in the host's MANIFEST allow-list AND already
//     a compose service in the running stack, behaves identically to
//     /scale -- just bumps replicas.
//  3. Otherwise: svc MUST appear in the embedded worker-types catalog.
//     We merge its compose block into docker-compose.workers.yml
//     (idempotent) and add svc to the in-memory allow-list, then
//     run docker compose pull + up -d --scale.
//
// The override file is the source of truth for "what this host has
// been provisioned with beyond its initial MANIFEST." It survives
// container restarts; on agent boot we re-add any services it lists
// to the allow-list (TODO -- v1 doesn't persist that, but compose
// itself rediscovers the blocks via the -f flag in compose()).
func (s *server) handleProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	svc := r.URL.Query().Get("service")
	repStr := r.URL.Query().Get("replicas")
	if !validService.MatchString(svc) {
		http.Error(w, "invalid service name", http.StatusBadRequest)
		return
	}
	if !isScalable(svc) {
		http.Error(w, "service not scalable (must end in -worker)", http.StatusBadRequest)
		return
	}
	if svc == "deploy-agent" {
		http.Error(w, "deploy-agent cannot scale itself", http.StatusBadRequest)
		return
	}
	// Same digit-only replicas parse as /scale.
	n := 0
	for _, c := range repStr {
		if c < '0' || c > '9' {
			http.Error(w, fmt.Sprintf("replicas must be an integer 1..%d", maxScaleReplicas), http.StatusBadRequest)
			return
		}
		n = n*10 + int(c-'0')
		if n > maxScaleReplicas {
			break
		}
	}
	if n < 1 || n > maxScaleReplicas {
		http.Error(w, fmt.Sprintf("replicas must be in [1, %d]", maxScaleReplicas), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	provisioned := false
	if !catalogHas(svc) {
		// Not in the catalog AND not in MANIFEST = nothing we can do.
		// Catalog is the source of truth for "what can be provisioned
		// at runtime"; MANIFEST workers without catalog entries are
		// the original baked-in services and /scale would handle them.
		if _, ok := s.allowed[svc]; !ok {
			http.Error(w, "service not in worker-types catalog and not in host MANIFEST", http.StatusBadRequest)
			return
		}
	} else {
		// Render into the override file. Idempotent: re-running with
		// an unchanged catalog produces an unchanged file, so compose
		// up --no-recreate is a no-op for the existing container.
		overridePath := filepath.Join(s.composeDir, "docker-compose.workers.yml")
		added, err := overrideUpsert(overridePath, svc)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"step":  "override-upsert",
				"error": err.Error(),
			})
			return
		}
		provisioned = added
		s.allowed[svc] = struct{}{}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	before, err := s.countReplicas(ctx, svc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"step": "ps-before", "error": err.Error()})
		return
	}

	// Pull first so the up call doesn't block on registry latency
	// while holding the mutex (well, it would anyway, but at least
	// the failure mode is clearer when reported separately).
	pullOut, err := s.compose(ctx, "pull", svc)
	if err != nil {
		log.Printf("provision: pull failed svc=%s err=%v", svc, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"step":   "pull",
			"error":  err.Error(),
			"output": tail(string(pullOut), 30),
		})
		return
	}

	log.Printf("provision: service=%s before=%d target=%d provisioned=%v", svc, before, n, provisioned)
	upOut, err := s.compose(ctx, "up", "-d", "--no-deps", "--no-recreate",
		"--scale", fmt.Sprintf("%s=%d", svc, n), svc)
	if err != nil {
		log.Printf("provision: up failed svc=%s target=%d err=%v", svc, n, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"step":   "up",
			"error":  err.Error(),
			"output": tail(string(upOut), 30),
		})
		return
	}
	after, err := s.countReplicas(ctx, svc)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"step": "ps-after", "error": err.Error()})
		return
	}
	log.Printf("provision: ok svc=%s before=%d after=%d provisioned=%v", svc, before, after, provisioned)
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     svc,
		"host":        s.manifest.Host,
		"before":      before,
		"after":       after,
		"requested":   n,
		"provisioned": provisioned,
		"pull":        tail(string(pullOut), 20),
		"compose":     tail(string(upOut), 30),
	})
}

func (s *server) handleRestartAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	tag := r.URL.Query().Get("tag")
	if !validTag.MatchString(tag) {
		http.Error(w, "invalid tag", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tagsPath := filepath.Join(s.composeDir, "image-tags.env")
	results := make([]map[string]any, 0, len(s.manifest.Services))
	for _, svc := range s.manifest.Services {
		if svc == "deploy-agent" {
			results = append(results, map[string]any{"service": svc, "skipped": "self"})
			continue
		}
		if err := upsertTag(tagsPath, svc, tag); err != nil {
			results = append(results, map[string]any{"service": svc, "error": err.Error()})
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		pullOut, err := s.compose(ctx, "pull", svc)
		if err != nil {
			cancel()
			results = append(results, map[string]any{"service": svc, "step": "pull", "error": err.Error(), "output": tail(string(pullOut), 20)})
			continue
		}
		upOut, err := s.compose(ctx, "up", "-d", "--no-deps", "--force-recreate", svc)
		cancel()
		if err != nil {
			results = append(results, map[string]any{"service": svc, "step": "up", "error": err.Error(), "output": tail(string(upOut), 20)})
			continue
		}
		results = append(results, map[string]any{"service": svc, "tag": tag, "ok": true})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// compose shells out to docker compose with the agent's working
// directory + both env files passed via --env-file. The second
// --env-file is required because compose only uses vars from the
// auto-loaded .env (or --env-file) for ${VAR} template substitution
// in image:/volumes:/etc.; service-block env_file: only populates
// container environments. Without this the per-image
// ${ADAMATON_<SVC>_TAG} placeholders would always fall through to
// their :-main defaults regardless of image-tags.env contents.
//
// If a docker-compose.workers.yml override file exists in composeDir
// (written by /provision), include it via a second -f. Compose merges
// the two: services defined in the override are added to the union,
// services in both are merged (override wins). Harmless when empty.
func (s *server) compose(ctx context.Context, args ...string) ([]byte, error) {
	full := []string{"compose", "--env-file", ".env", "--env-file", "image-tags.env"}
	full = append(full, "-f", "docker-compose.yml")
	if _, err := os.Stat(filepath.Join(s.composeDir, "docker-compose.workers.yml")); err == nil {
		full = append(full, "-f", "docker-compose.workers.yml")
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, s.composeBin, full...)
	cmd.Dir = s.composeDir
	return cmd.CombinedOutput()
}

// upsertTag rewrites image-tags.env in place: replaces the line for svc
// or appends a new one. Each tag lives on its own line:
//
//	ADAMATON_<UPPER_SVC>_TAG=<value>
//
// Compose reads this file via env_file so the substitution into each
// service's image: directive picks up the change without restarting
// the whole stack.
func upsertTag(path, svc, tag string) error {
	key := tagEnvKey(svc)
	line := key + "=" + tag

	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := []string{}
	if len(b) > 0 {
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	replaced := false
	for i, l := range lines {
		if strings.HasPrefix(l, key+"=") {
			lines[i] = line
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	out := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(out), 0o644)
}

// tagEnvKey converts a compose service name to its image-tags.env key:
//
//	nano-research-worker -> ADAMATON_NANO_RESEARCH_WORKER_TAG
//	adamaton-worker      -> ADAMATON_WORKER_TAG     (prefix stripped)
//
// The leading "adamaton-" is stripped before uppercasing because every
// key in image-tags.env already starts with "ADAMATON_" — without the
// strip the unified worker service would map to
// ADAMATON_ADAMATON_WORKER_TAG, which doesn't match anything. Every
// legacy service name (skills-worker, dispatch-worker, …) lacks the
// prefix and is unaffected.
func tagEnvKey(svc string) string {
	svc = strings.TrimPrefix(svc, "adamaton-")
	upper := strings.ToUpper(svc)
	upper = strings.ReplaceAll(upper, "-", "_")
	return "ADAMATON_" + upper + "_TAG"
}

// tail returns the last n lines of s (or all of s if shorter).
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

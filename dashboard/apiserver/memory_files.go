// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"gopkg.in/yaml.v3"
)

// MemoryItem is the on-the-wire shape for a filesystem-backed memory
// record. Agents that store memory as plain markdown files (Claude Code,
// Codex, Gemini, OpenCode, the various CLAUDE.md anchors) all flatten
// down to this struct. id is opaque to clients — it's a path safely
// scoped under the agent's root and base64url-encoded so it survives
// being a URL path segment. Body is omitted from list responses; the
// detail endpoint fills it in.
type MemoryItem struct {
	ID           string    `json:"id"`
	Agent        string    `json:"agent"`
	Scope        string    `json:"scope,omitempty"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Type         string    `json:"type,omitempty"`
	Body         string    `json:"body,omitempty"`
	Path         string    `json:"path"`
	LastModified time.Time `json:"last_modified"`
	HasMatter    bool      `json:"has_matter"`
}

// MemorySource lists one queryable bucket of memory. Counts are computed
// at request time (cheap — every agent root has <1k files in practice).
type MemorySource struct {
	Key          string    `json:"key"`
	Label        string    `json:"label"`
	Kind         string    `json:"kind"`
	Count        int       `json:"count"`
	LastModified time.Time `json:"last_modified,omitempty"`
	Path         string    `json:"path,omitempty"`
	Available    bool      `json:"available"`
	Note         string    `json:"note,omitempty"`
}

// memoryAgentRoots resolves the on-disk roots we know about. The home
// directory is computed once at registration time so test overrides can
// flip HOME without restarting the process.
type memoryAgentRoots struct {
	home string
}

func newMemoryAgentRoots() *memoryAgentRoots {
	home, _ := os.UserHomeDir()
	return &memoryAgentRoots{home: home}
}

// claudeProjectsDir is the parent of all per-project Claude Code memory
// dirs. Each child directory is one project slug; each has a MEMORY.md
// index plus per-memory .md files. Empty string when no HOME is set
// (test envs without one).
func (r *memoryAgentRoots) claudeProjectsDir() string {
	if r.home == "" {
		return ""
	}
	return filepath.Join(r.home, ".claude", "projects")
}

func (r *memoryAgentRoots) claudeGlobalMD() string {
	if r.home == "" {
		return ""
	}
	return filepath.Join(r.home, ".claude", "CLAUDE.md")
}

func (r *memoryAgentRoots) codexMemoryDir() string {
	if r.home == "" {
		return ""
	}
	return filepath.Join(r.home, ".codex", "memories")
}

func (r *memoryAgentRoots) geminiMemoryFile() string {
	if r.home == "" {
		return ""
	}
	return filepath.Join(r.home, ".gemini", "GEMINI.md")
}

// opencodeMemoryDir picks the more-specific config root when present.
// OpenCode upstream uses ~/.config/opencode by default; some users still
// have ~/.opencode from older installs. We pick whichever exists; when
// both are present, ~/.config/opencode wins because the upstream config
// schema reference (opencode.json $schema URL) points there.
func (r *memoryAgentRoots) opencodeMemoryDir() string {
	if r.home == "" {
		return ""
	}
	cfg := filepath.Join(r.home, ".config", "opencode", "memories")
	if _, err := os.Stat(cfg); err == nil {
		return cfg
	}
	legacy := filepath.Join(r.home, ".opencode", "memories")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	// Neither exists yet — return the canonical (preferred) path so the
	// UI can show "no memory configured" with the place writes will land.
	return cfg
}

// projectClaudeMDPaths enumerates per-repo CLAUDE.md files. We search a
// handful of well-known parent directories rather than the entire home
// tree — a full walk would be O(disk) and stat tens of thousands of
// node_modules entries.
func (r *memoryAgentRoots) projectClaudeMDPaths() []string {
	out := []string{}
	if r.home == "" {
		return out
	}
	roots := []string{
		filepath.Join("/thearray", "git"),
		filepath.Join(r.home, "code"),
		filepath.Join(r.home, "projects"),
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(root, e.Name(), "CLAUDE.md")
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// memoryAgents enumerates every agent surface the API exposes. Mux's
// path-segment validator below pins {agent} to this set, so a hostile
// caller can't force us to walk an arbitrary directory by varying the
// URL.
var memoryAgents = map[string]struct{}{
	"claude-code":     {},
	"claude-global":   {},
	"claude-projects": {},
	"codex":           {},
	"gemini":          {},
	"opencode":        {},
}

func (s *APIServer) registerMemoryFileEndpoints(api *mux.Router) {
	api.HandleFunc("/memory/sources", s.handleMemorySources).Methods("GET")
	api.HandleFunc("/memory/agents/{agent}/items", s.handleMemoryAgentList).Methods("GET")
	api.HandleFunc("/memory/agents/{agent}/items", s.handleMemoryAgentCreate).Methods("POST")
	api.HandleFunc("/memory/agents/{agent}/items/{id}", s.handleMemoryAgentGet).Methods("GET")
	api.HandleFunc("/memory/agents/{agent}/items/{id}", s.handleMemoryAgentUpdate).Methods("PATCH")
	api.HandleFunc("/memory/agents/{agent}/items/{id}", s.handleMemoryAgentDelete).Methods("DELETE")
}

func (s *APIServer) handleMemorySources(w http.ResponseWriter, r *http.Request) {
	roots := newMemoryAgentRoots()
	out := []MemorySource{}

	// Claude Code per-project memory (the rich source — frontmatter
	// + MEMORY.md index). We aggregate across every project dir.
	if dir := roots.claudeProjectsDir(); dir != "" {
		count, latest := countClaudeProjectMemories(dir)
		out = append(out, MemorySource{
			Key:          "claude-code",
			Label:        "Claude Code (per-project)",
			Kind:         "files",
			Count:        count,
			LastModified: latest,
			Path:         dir,
			Available:    count >= 0,
		})
	}

	// Single-file anchors. We surface these as their own pseudo-sources
	// so the UI's "filter by source" pill still works on something
	// finer-grained than "Claude Code".
	if p := roots.claudeGlobalMD(); p != "" {
		info, err := os.Stat(p)
		src := MemorySource{Key: "claude-global", Label: "Claude Code (global CLAUDE.md)", Kind: "file", Path: p}
		if err == nil {
			src.Count = 1
			src.LastModified = info.ModTime()
			src.Available = true
		}
		out = append(out, src)
	}

	projMDs := roots.projectClaudeMDPaths()
	latest := time.Time{}
	for _, p := range projMDs {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	out = append(out, MemorySource{
		Key:          "claude-projects",
		Label:        "Per-repo CLAUDE.md",
		Kind:         "files",
		Count:        len(projMDs),
		LastModified: latest,
		Available:    true,
	})

	out = append(out, fileSource("codex", "Codex memories", roots.codexMemoryDir()))
	out = append(out, geminiSource(roots.geminiMemoryFile()))
	out = append(out, fileSource("opencode", "OpenCode memories", roots.opencodeMemoryDir()))

	// Postgres-backed sources contribute their own rows. We deliberately
	// do the count here (rather than from memory_db.go) so the response
	// is a single API call from the UI's perspective.
	if s.evoPool != nil {
		insightsCount, insightsLatest := s.countInsights(r)
		out = append(out, MemorySource{
			Key:          "insights",
			Label:        "evo.insights",
			Kind:         "postgres",
			Count:        insightsCount,
			LastModified: insightsLatest,
			Available:    true,
		})
		entCount, entLatest := s.countDeepresearchTable(r, "documents_entities")
		out = append(out, MemorySource{
			Key:          "entities",
			Label:        "deepresearch entities",
			Kind:         "postgres",
			Count:        entCount,
			LastModified: entLatest,
			Available:    entCount >= 0,
		})
		relCount, relLatest := s.countDeepresearchTable(r, "documents_relationships")
		out = append(out, MemorySource{
			Key:          "relationships",
			Label:        "deepresearch relationships",
			Kind:         "postgres",
			Count:        relCount,
			LastModified: relLatest,
			Available:    relCount >= 0,
		})
	}

	writeEvoJSON(w, out)
}

func fileSource(key, label, dir string) MemorySource {
	src := MemorySource{Key: key, Label: label, Kind: "files", Path: dir}
	if dir == "" {
		src.Note = "no memory configured"
		return src
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			src.Note = "no memory configured"
			return src
		}
		src.Note = err.Error()
		return src
	}
	latest := time.Time{}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		count++
		if info, err := e.Info(); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	src.Count = count
	src.LastModified = latest
	src.Available = true
	return src
}

func geminiSource(p string) MemorySource {
	src := MemorySource{Key: "gemini", Label: "Gemini (GEMINI.md)", Kind: "file", Path: p}
	if p == "" {
		src.Note = "no memory configured"
		return src
	}
	info, err := os.Stat(p)
	if err != nil {
		src.Note = "no memory configured"
		return src
	}
	src.Count = 1
	src.LastModified = info.ModTime()
	src.Available = true
	return src
}

// countClaudeProjectMemories walks every project's memory/ dir and
// counts the .md files (excluding MEMORY.md). Returns the freshest
// mtime so the dashboard can render a "last updated 2 days ago" pill.
// Negative count signals an unreachable root rather than zero memories.
func countClaudeProjectMemories(parent string) (int, time.Time) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return -1, time.Time{}
	}
	count := 0
	latest := time.Time{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		memDir := filepath.Join(parent, e.Name(), "memory")
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(strings.ToLower(f.Name()), ".md") {
				continue
			}
			if strings.EqualFold(f.Name(), "MEMORY.md") {
				continue
			}
			count++
			if info, err := f.Info(); err == nil && info.ModTime().After(latest) {
				latest = info.ModTime()
			}
		}
	}
	return count, latest
}

func (s *APIServer) handleMemoryAgentList(w http.ResponseWriter, r *http.Request) {
	agent := mux.Vars(r)["agent"]
	if _, ok := memoryAgents[agent]; !ok {
		writeEvoErr(w, http.StatusBadRequest, "unknown agent")
		return
	}
	items, err := s.listAgentMemory(agent)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeEvoJSON(w, items)
}

func (s *APIServer) handleMemoryAgentGet(w http.ResponseWriter, r *http.Request) {
	agent := mux.Vars(r)["agent"]
	id := mux.Vars(r)["id"]
	if _, ok := memoryAgents[agent]; !ok {
		writeEvoErr(w, http.StatusBadRequest, "unknown agent")
		return
	}
	item, err := s.readAgentMemory(agent, id)
	if err != nil {
		if errors.Is(err, errMemoryNotFound) {
			writeEvoErr(w, http.StatusNotFound, "memory not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeEvoJSON(w, item)
}

// memoryWriteInput is the union of POST and PATCH bodies. Fields a
// caller doesn't set are left untouched on PATCH. POST validates that
// the required fields (name + body) are present.
type memoryWriteInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Scope       string `json:"scope"`
	Body        string `json:"body"`
}

func (s *APIServer) handleMemoryAgentCreate(w http.ResponseWriter, r *http.Request) {
	agent := mux.Vars(r)["agent"]
	if _, ok := memoryAgents[agent]; !ok {
		writeEvoErr(w, http.StatusBadRequest, "unknown agent")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in memoryWriteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeEvoErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if strings.TrimSpace(in.Body) == "" {
		writeEvoErr(w, http.StatusBadRequest, "body is required")
		return
	}
	item, err := s.createAgentMemory(agent, &in)
	if err != nil {
		if errors.Is(err, errMemoryReadOnly) {
			writeEvoErr(w, http.StatusMethodNotAllowed, err.Error())
			return
		}
		if errors.Is(err, errMemoryExists) {
			writeEvoErr(w, http.StatusConflict, "memory with that name exists")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeEvoJSON(w, item)
}

func (s *APIServer) handleMemoryAgentUpdate(w http.ResponseWriter, r *http.Request) {
	agent := mux.Vars(r)["agent"]
	id := mux.Vars(r)["id"]
	if _, ok := memoryAgents[agent]; !ok {
		writeEvoErr(w, http.StatusBadRequest, "unknown agent")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in memoryWriteInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	item, err := s.updateAgentMemory(agent, id, &in)
	if err != nil {
		if errors.Is(err, errMemoryNotFound) {
			writeEvoErr(w, http.StatusNotFound, "memory not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeEvoJSON(w, item)
}

func (s *APIServer) handleMemoryAgentDelete(w http.ResponseWriter, r *http.Request) {
	agent := mux.Vars(r)["agent"]
	id := mux.Vars(r)["id"]
	if _, ok := memoryAgents[agent]; !ok {
		writeEvoErr(w, http.StatusBadRequest, "unknown agent")
		return
	}
	if err := s.deleteAgentMemory(agent, id); err != nil {
		if errors.Is(err, errMemoryNotFound) {
			writeEvoErr(w, http.StatusNotFound, "memory not found")
			return
		}
		if errors.Is(err, errMemoryReadOnly) {
			writeEvoErr(w, http.StatusMethodNotAllowed, err.Error())
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Per-agent implementations ----

var (
	errMemoryNotFound = errors.New("memory not found")
	errMemoryReadOnly = errors.New("this memory source is single-file; create/delete are not supported")
	errMemoryExists   = errors.New("memory already exists")
)

func (s *APIServer) listAgentMemory(agent string) ([]MemoryItem, error) {
	roots := newMemoryAgentRoots()
	switch agent {
	case "claude-code":
		return listClaudeProjects(roots.claudeProjectsDir())
	case "claude-global":
		return listSingleFile(agent, "global", roots.claudeGlobalMD(), false)
	case "claude-projects":
		out := []MemoryItem{}
		for _, p := range roots.projectClaudeMDPaths() {
			items, err := listSingleFile(agent, filepath.Base(filepath.Dir(p)), p, false)
			if err != nil {
				return nil, err
			}
			out = append(out, items...)
		}
		return out, nil
	case "codex":
		return listDirMarkdown(agent, "", roots.codexMemoryDir())
	case "gemini":
		return listSingleFile(agent, "", roots.geminiMemoryFile(), false)
	case "opencode":
		return listDirMarkdown(agent, "", roots.opencodeMemoryDir())
	}
	return nil, fmt.Errorf("unsupported agent: %s", agent)
}

func (s *APIServer) readAgentMemory(agent, id string) (*MemoryItem, error) {
	roots := newMemoryAgentRoots()
	root, scope, err := resolveAgentRoot(agent, id, roots)
	if err != nil {
		return nil, err
	}
	target, err := safeJoin(root, id)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errMemoryNotFound
		}
		return nil, err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	meta, body := parseFrontmatter(raw)
	item := &MemoryItem{
		ID:           encodeMemoryID(id),
		Agent:        agent,
		Scope:        scope,
		Path:         target,
		LastModified: info.ModTime(),
		Body:         body,
		Name:         memoryNameFromPath(target, meta),
		Description:  meta.Description,
		Type:         meta.Type,
		HasMatter:    meta.present,
	}
	return item, nil
}

func (s *APIServer) createAgentMemory(agent string, in *memoryWriteInput) (*MemoryItem, error) {
	roots := newMemoryAgentRoots()
	switch agent {
	case "claude-global", "gemini":
		return nil, errMemoryReadOnly
	case "claude-projects":
		return nil, errMemoryReadOnly
	}

	var (
		dir         string
		filename    string
		writeMatter bool
	)
	slug := slugify(in.Name)
	if slug == "" {
		return nil, errors.New("name must yield a non-empty slug")
	}
	switch agent {
	case "claude-code":
		// Scope = project slug (the basename under .claude/projects). When
		// absent, default to the current project so a casual "new memory"
		// click lands somewhere sane.
		scope := strings.TrimSpace(in.Scope)
		if scope == "" {
			scope = filepath.Base(strings.TrimPrefix(currentProjectSlug(), "-"))
			if scope == "" || scope == "." {
				return nil, errors.New("scope (project slug) is required when no current project is detectable")
			}
		}
		dir = filepath.Join(roots.claudeProjectsDir(), scope, "memory")
		filename = slug + ".md"
		writeMatter = true
	case "codex":
		dir = roots.codexMemoryDir()
		filename = slug + ".md"
	case "opencode":
		dir = roots.opencodeMemoryDir()
		filename = slug + ".md"
	default:
		return nil, fmt.Errorf("unsupported agent: %s", agent)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	target := filepath.Join(dir, filename)
	if _, err := os.Stat(target); err == nil {
		return nil, errMemoryExists
	}

	content := in.Body
	if writeMatter {
		matter := frontmatter{
			Name:        slug,
			Description: in.Description,
			Type:        in.Type,
			present:     true,
		}
		content = renderFrontmatter(matter) + "\n" + ensureTrailingNewline(in.Body)
	} else {
		content = ensureTrailingNewline(in.Body)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	if agent == "claude-code" {
		// Best-effort: a failed index update is logged but doesn't roll
		// back the write, because the body file is still independently
		// useful and the index can be regenerated. The user can see the
		// missing pointer in the UI on next list refresh.
		_ = updateMemoryIndex(filepath.Dir(target), filename, in.Description, true)
	}

	// Re-read so the response reflects the same parser path the client
	// would hit on a subsequent GET. The path-relative ID is computed
	// from the agent's root.
	relID := strings.TrimPrefix(target, agentRoot(agent, roots)+string(os.PathSeparator))
	return s.readAgentMemory(agent, relID)
}

func (s *APIServer) updateAgentMemory(agent, id string, in *memoryWriteInput) (*MemoryItem, error) {
	roots := newMemoryAgentRoots()
	_, _, err := resolveAgentRoot(agent, id, roots)
	if err != nil {
		return nil, err
	}
	target, err := safeJoin(agentRoot(agent, roots), id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errMemoryNotFound
		}
		return nil, err
	}
	meta, body := parseFrontmatter(raw)
	// PATCH semantics: only overwrite fields the caller actually sent.
	// Empty strings are treated as "no change" rather than "delete" —
	// otherwise a UI that omits e.g. type on a Claude Code memory would
	// silently strip it. Callers who want to clear a field should send
	// a single space.
	if in.Body != "" {
		body = in.Body
	}
	if in.Description != "" {
		meta.Description = in.Description
	}
	if in.Type != "" {
		meta.Type = in.Type
	}
	if in.Name != "" {
		meta.Name = slugify(in.Name)
	}

	var content string
	if meta.present {
		content = renderFrontmatter(meta) + "\n" + ensureTrailingNewline(body)
	} else {
		content = ensureTrailingNewline(body)
	}
	// atomic-ish replace: write to <name>.tmp then rename. Without this,
	// a crash mid-write leaves a half-empty memory file.
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("rename: %w", err)
	}

	if agent == "claude-code" && meta.Description != "" {
		_ = updateMemoryIndex(filepath.Dir(target), filepath.Base(target), meta.Description, true)
	}
	return s.readAgentMemory(agent, id)
}

func (s *APIServer) deleteAgentMemory(agent, id string) error {
	roots := newMemoryAgentRoots()
	switch agent {
	case "claude-global", "gemini", "claude-projects":
		return errMemoryReadOnly
	}
	_, _, err := resolveAgentRoot(agent, id, roots)
	if err != nil {
		return err
	}
	target, err := safeJoin(agentRoot(agent, roots), id)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errMemoryNotFound
		}
		return err
	}
	if info.IsDir() {
		return errors.New("refusing to delete a directory")
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if agent == "claude-code" {
		_ = updateMemoryIndex(filepath.Dir(target), filepath.Base(target), "", false)
	}
	return nil
}

// ---- helpers ----

// resolveAgentRoot validates that {agent} + decoded id stays under the
// configured root for that agent. Returns the root and a scope label
// (the project slug for claude-code; empty for everything else).
func resolveAgentRoot(agent, id string, roots *memoryAgentRoots) (string, string, error) {
	root := agentRoot(agent, roots)
	if root == "" {
		return "", "", errors.New("memory source not configured for agent")
	}
	if agent == "claude-code" && id != "" {
		// id is "<project-slug>/memory/<file>.md" — surface the slug.
		parts := strings.SplitN(id, string(os.PathSeparator), 2)
		if len(parts) > 0 {
			return root, parts[0], nil
		}
	}
	return root, "", nil
}

func agentRoot(agent string, roots *memoryAgentRoots) string {
	switch agent {
	case "claude-code":
		return roots.claudeProjectsDir()
	case "claude-global":
		return filepath.Dir(roots.claudeGlobalMD())
	case "claude-projects":
		// File rooted at /; we use "/" as the synthetic root and rely on
		// safeJoin to reject anything not under one of the project dirs
		// we already discovered.
		return "/"
	case "codex":
		return roots.codexMemoryDir()
	case "gemini":
		return filepath.Dir(roots.geminiMemoryFile())
	case "opencode":
		return roots.opencodeMemoryDir()
	}
	return ""
}

// safeJoin enforces the path-traversal boundary. id is decoded from the
// URL path segment; the result is required to land strictly under root
// after symlink resolution. Bare absolute paths are rejected, as are
// any "../" segments — both common path-traversal payloads.
func safeJoin(root, id string) (string, error) {
	dec, err := decodeMemoryID(id)
	if err != nil {
		return "", err
	}
	if dec == "" {
		return "", errors.New("empty id")
	}
	if filepath.IsAbs(dec) {
		// Single special case: /thearray/git/... CLAUDE.md absolute
		// targets for the claude-projects agent. We allow them only when
		// the resolved file is under one of the well-known parent dirs
		// we walked at list time.
		rootInfo, _ := os.Stat(root)
		if rootInfo != nil && rootInfo.IsDir() && root == "/" {
			clean := filepath.Clean(dec)
			if !strings.HasSuffix(clean, "CLAUDE.md") {
				return "", errors.New("absolute id outside of claude-projects scope")
			}
			return clean, nil
		}
		return "", errors.New("absolute id not allowed")
	}
	clean := filepath.Clean(filepath.Join(root, dec))
	rootClean := filepath.Clean(root)
	// First gate: the lexically-cleaned path must not climb above root.
	// This rejects "../" payloads before we ever touch the filesystem.
	if !pathWithin(clean, rootClean) {
		return "", errors.New("path escapes root")
	}
	// Second gate: resolve symlinks. The old implementation only
	// os.Readlink'd the FINAL component and then merely checked
	// resolved != root — so (a) an intermediate directory symlink that
	// pointed outside root was never resolved, and (b) a final-component
	// symlink that pointed anywhere outside root (but wasn't literally
	// root) slipped through. We now resolve the longest existing prefix
	// of `clean` with filepath.EvalSymlinks (which follows EVERY symlink
	// in the chain, intermediate dirs included) and require the resolved
	// parent to remain within the resolved root.
	resolvedRoot, err := filepath.EvalSymlinks(rootClean)
	if err != nil {
		// root itself doesn't exist / isn't traversable — fall back to the
		// lexical rootClean so containment is still anchored somewhere.
		resolvedRoot = rootClean
	}
	resolvedParent, err := evalSymlinksLongestPrefix(filepath.Dir(clean))
	if err != nil {
		return "", errors.New("resolve path: " + err.Error())
	}
	if !pathWithin(resolvedParent, resolvedRoot) {
		return "", errors.New("symlink escapes root")
	}
	// If the final component itself is a symlink, resolve its target and
	// re-check — a leaf symlink pointing outside root is rejected even when
	// its parent is clean. We use os.Readlink (not EvalSymlinks) so a
	// symlink whose TARGET doesn't exist yet is still validated: the old
	// bug was that a leaf pointing outside root passed as long as it wasn't
	// literally root, and EvalSymlinks would skip a dangling link entirely.
	if target, err := os.Readlink(clean); err == nil {
		var resolvedLeaf string
		if filepath.IsAbs(target) {
			resolvedLeaf = filepath.Clean(target)
		} else {
			resolvedLeaf = filepath.Clean(filepath.Join(resolvedParent, target))
		}
		// Fully resolve any further symlink chain on the existing prefix of
		// the target so a leaf -> symlink -> outside chain is also caught.
		if deep, derr := evalSymlinksLongestPrefix(resolvedLeaf); derr == nil {
			resolvedLeaf = deep
		}
		if !pathWithin(resolvedLeaf, resolvedRoot) {
			return "", errors.New("symlink escapes root")
		}
	}
	return clean, nil
}

// pathWithin reports whether p is rootClean itself or a descendant of it,
// using a separator-anchored prefix check so "/root-evil" is NOT treated
// as being within "/root".
func pathWithin(p, rootClean string) bool {
	if p == rootClean {
		return true
	}
	return strings.HasPrefix(p, rootClean+string(os.PathSeparator))
}

// evalSymlinksLongestPrefix resolves symlinks on the longest existing
// ancestor of p. filepath.EvalSymlinks fails if p doesn't exist (a new
// memory file being created lives under an existing dir but isn't itself
// present yet), so we walk up to the first existing ancestor, resolve
// THAT, then re-append the non-existent tail lexically. The tail can't
// contain new symlinks (it doesn't exist on disk), so this is sound.
func evalSymlinksLongestPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	var tail []string
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if len(tail) == 0 {
				return resolved, nil
			}
			parts := append([]string{resolved}, tail...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding an existing
			// ancestor — return the lexical clean as a best effort.
			return p, nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func listClaudeProjects(parent string) ([]MemoryItem, error) {
	out := []MemoryItem{}
	if parent == "" {
		return out, nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		memDir := filepath.Join(parent, e.Name(), "memory")
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(strings.ToLower(f.Name()), ".md") {
				continue
			}
			if strings.EqualFold(f.Name(), "MEMORY.md") {
				continue
			}
			rel := filepath.Join(e.Name(), "memory", f.Name())
			full := filepath.Join(parent, rel)
			info, err := f.Info()
			if err != nil {
				continue
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			meta, _ := parseFrontmatter(raw)
			out = append(out, MemoryItem{
				ID:           encodeMemoryID(rel),
				Agent:        "claude-code",
				Scope:        e.Name(),
				Name:         memoryNameFromPath(full, meta),
				Description:  meta.Description,
				Type:         meta.Type,
				Path:         full,
				LastModified: info.ModTime(),
				HasMatter:    meta.present,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	return out, nil
}

func listDirMarkdown(agent, scope, dir string) ([]MemoryItem, error) {
	out := []MemoryItem{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		meta, _ := parseFrontmatter(raw)
		out = append(out, MemoryItem{
			ID:           encodeMemoryID(e.Name()),
			Agent:        agent,
			Scope:        scope,
			Name:         memoryNameFromPath(full, meta),
			Description:  meta.Description,
			Type:         meta.Type,
			Path:         full,
			LastModified: info.ModTime(),
			HasMatter:    meta.present,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	return out, nil
}

// listSingleFile wraps a one-file source as a single MemoryItem so the
// API surface is uniform with the dir-backed agents. The id is the
// absolute path encoded; we never list the same file twice.
func listSingleFile(agent, scope, path string, _ bool) ([]MemoryItem, error) {
	if path == "" {
		return []MemoryItem{}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []MemoryItem{}, nil
		}
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta, _ := parseFrontmatter(raw)
	return []MemoryItem{
		{
			ID:           encodeMemoryID(path),
			Agent:        agent,
			Scope:        scope,
			Name:         memoryNameFromPath(path, meta),
			Description:  meta.Description,
			Path:         path,
			LastModified: info.ModTime(),
			HasMatter:    meta.present,
		},
	}, nil
}

// frontmatter is the parsed YAML head of a Claude Code memory file.
// We only care about a handful of fields — anything else is preserved
// as-is in the raw map so a round-trip edit doesn't drop unknown keys.
type frontmatter struct {
	Name        string                 `yaml:"name,omitempty"`
	Description string                 `yaml:"description,omitempty"`
	Type        string                 `yaml:"type,omitempty"`
	Metadata    map[string]interface{} `yaml:"metadata,omitempty"`
	Extra       map[string]interface{} `yaml:",inline,omitempty"`
	present     bool
}

var matterRE = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?`)

// parseFrontmatter splits a markdown file into its YAML matter and
// body. When no `---` block is present, the returned frontmatter is
// zero-valued with present=false so writers know to omit the head on
// round-trip. We also flatten metadata.type up onto Type so the wire
// shape doesn't need to know about the nested key.
func parseFrontmatter(raw []byte) (frontmatter, string) {
	m := matterRE.FindSubmatchIndex(raw)
	if m == nil {
		return frontmatter{}, string(raw)
	}
	yamlBody := raw[m[2]:m[3]]
	body := string(raw[m[1]:])
	var fm frontmatter
	if err := yaml.Unmarshal(yamlBody, &fm); err != nil {
		// Malformed YAML — surface the raw body so the user can fix
		// the head in place rather than silently losing the content.
		return frontmatter{}, string(raw)
	}
	fm.present = true
	if fm.Type == "" && fm.Metadata != nil {
		if t, ok := fm.Metadata["type"].(string); ok {
			fm.Type = t
		}
	}
	return fm, body
}

// renderFrontmatter serialises a frontmatter back to a YAML head. We
// re-fold Type into metadata.type for the Claude Code shape so the
// agent's own readers still see what they expect.
func renderFrontmatter(fm frontmatter) string {
	if fm.Metadata == nil {
		fm.Metadata = map[string]interface{}{}
	}
	if _, ok := fm.Metadata["node_type"]; !ok {
		fm.Metadata["node_type"] = "memory"
	}
	if fm.Type != "" {
		fm.Metadata["type"] = fm.Type
	}
	out := map[string]interface{}{}
	if fm.Name != "" {
		out["name"] = fm.Name
	}
	if fm.Description != "" {
		out["description"] = fm.Description
	}
	out["metadata"] = fm.Metadata
	for k, v := range fm.Extra {
		if _, taken := out[k]; !taken {
			out[k] = v
		}
	}
	b, err := yaml.Marshal(out)
	if err != nil {
		// Marshalling well-formed Go data through yaml.v3 only fails
		// for cycles, which we don't construct. Defensive empty head.
		return "---\n---\n"
	}
	return "---\n" + string(b) + "---\n"
}

func memoryNameFromPath(path string, fm frontmatter) string {
	if fm.Name != "" {
		return fm.Name
	}
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".md")
	return base
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// slugify converts "Codex Sticky Notes!" -> "codex-sticky-notes". Used
// for filename derivation so a user's free-text "name" lands on disk
// as a predictable, traversal-safe path component.
func slugify(in string) string {
	lower := strings.ToLower(strings.TrimSpace(in))
	slug := slugRE.ReplaceAllString(lower, "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 80 {
		slug = slug[:80]
	}
	return slug
}

// updateMemoryIndex maintains the MEMORY.md pointer file Claude Code
// uses to anchor cross-memory links. add=true upserts a "* [slug](slug.md) — description"
// row; add=false removes any row pointing at the file. Best-effort: a
// missing MEMORY.md is created with a leading "# Memory index ..." line.
func updateMemoryIndex(dir, filename, description string, add bool) error {
	indexPath := filepath.Join(dir, "MEMORY.md")
	current, err := os.ReadFile(indexPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lines := strings.Split(string(current), "\n")
	out := make([]string, 0, len(lines)+1)
	found := false
	target := "(" + filename + ")"
	for _, ln := range lines {
		if strings.Contains(ln, target) {
			found = true
			if add {
				// Replace with the freshly-edited description so list
				// entries stay in sync with the body's frontmatter.
				slug := strings.TrimSuffix(filename, ".md")
				out = append(out, indexLine(slug, filename, description))
			}
			continue
		}
		out = append(out, ln)
	}
	if add && !found {
		slug := strings.TrimSuffix(filename, ".md")
		out = append(out, indexLine(slug, filename, description))
	}
	body := strings.Join(out, "\n")
	if !strings.HasPrefix(body, "#") {
		body = "# Memory index for " + filepath.Dir(dir) + "\n\n" + body
	}
	return os.WriteFile(indexPath, []byte(ensureTrailingNewline(body)), 0o644)
}

func indexLine(slug, filename, description string) string {
	if description != "" {
		return fmt.Sprintf("- [%s](%s) — %s", slug, filename, description)
	}
	return fmt.Sprintf("- [%s](%s)", slug, filename)
}

// encodeMemoryID / decodeMemoryID round-trip a relative path through a
// URL-safe representation. Forward slashes survive, so the id reads as
// a path in the URL bar; everything else gets percent-escaped. We use
// our own helper rather than encodeURIComponent so mux's route variable
// regex (default: [^/]+) sees a single segment.
func encodeMemoryID(rel string) string {
	// We swap "/" for ".../" so mux's default segment regex still treats
	// the whole id as one variable. Decoding reverses the swap.
	return strings.ReplaceAll(rel, "/", "~~")
}

func decodeMemoryID(id string) (string, error) {
	if strings.Contains(id, "..") {
		return "", errors.New("traversal in id")
	}
	return strings.ReplaceAll(id, "~~", "/"), nil
}

// currentProjectSlug returns the slug for the current working dir in
// the same shape Claude Code uses: leading dash, slashes replaced by
// dashes. Best-effort — empty when CWD is unknowable.
func currentProjectSlug() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return "-" + strings.ReplaceAll(strings.TrimPrefix(wd, "/"), "/", "-")
}

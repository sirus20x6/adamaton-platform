// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

// HTTP handlers for POST /api/v1/skills/import and POST
// /api/v1/skills/{id}/check-source. Pulled out of skills_endpoints.go
// to keep that file focused on CRUD.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	skillsworkflows "github.com/sirus20x6/adamomaton-knowledge/skills/workflows"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

type importRequest struct {
	Kind      string   `json:"kind"`      // "claude-files" | "local-file" | "local-dir" | "github-file" | "github-repo"
	Path      string   `json:"path"`      // local-file / local-dir / claude-files override
	URL       string   `json:"url"`       // github-file
	RepoURL   string   `json:"repo_url"`  // github-repo
	Subpath   string   `json:"subpath"`   // github-repo (defaults to "skills")
	Ref       string   `json:"ref"`       // github-repo (defaults to HEAD)
	Community string   `json:"community"` // applied when the parsed skill has no community
	Tags      []string `json:"tags"`      // appended to every imported skill
}

type importItemResult struct {
	Path      string   `json:"path,omitempty"`        // upstream path/url, for traceability
	Name      string   `json:"name,omitempty"`
	SkillID   string   `json:"skill_id,omitempty"`
	Status    string   `json:"status"`                // created | updated | unchanged | error
	Error     string   `json:"error,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

type importResponse struct {
	Results []importItemResult        `json:"results"`
	Summary map[string]int            `json:"summary"`
}

// importStartResponse is the 202 body returned when an import is
// scheduled as a Temporal workflow. The frontend polls status_url
// (a /skills/imports/{workflow_id}/progress route) every ~2s.
type importStartResponse struct {
	WorkflowID string `json:"workflow_id"`
	StatusURL  string `json:"status_url"`
}

func (s *APIServer) importSkillsHandler(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req importRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	// GitHub paths are long-running enough to risk Caddy / browser
	// timeouts on big repos; hand them to the importer workflow and
	// return immediately with a workflow_id the UI can poll. Local
	// paths stay synchronous: they finish in <1s for ~100 files and
	// would otherwise require filesystem access on the worker, which
	// is the wrong layering (the worker runs on the Pi; the path is
	// workstation-relative).
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "github-file" || kind == "github-repo" {
		s.startGithubImportWorkflow(w, r, req, kind)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	items, origin, err := s.gatherImportItems(ctx, req)
	// A non-empty items slice means the walker (or single-file fetch)
	// produced at least some parsed results. Apply those even if a
	// later step (e.g. GitHub rate-limit) bailed — partial credit is
	// useful, and re-running picks up the rest via sha-based dedup.
	if err != nil && len(items) == 0 {
		writeEvoErr(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := importResponse{
		Results: make([]importItemResult, 0, len(items)),
		Summary: map[string]int{},
	}
	if err != nil {
		resp.Summary["walker_error"] = 1
		resp.Results = append(resp.Results, importItemResult{
			Status: "error",
			Error:  "walker stopped early: " + err.Error(),
		})
	}
	for _, item := range items {
		row := importItemResult{Path: item.Path, Name: item.Skill.Name}
		if item.Err != nil {
			row.Status = "error"
			row.Error = item.Err.Error()
			resp.Results = append(resp.Results, row)
			resp.Summary["error"]++
			continue
		}
		sk := item.Skill
		sk.Origin = origin
		// Apply caller-provided defaults / appends.
		if sk.Community == "" {
			sk.Community = req.Community
		}
		sk.Tags = mergeTags(sk.Tags, req.Tags)

		applied, status, err := s.applyImportedSkill(ctx, sk)
		if err != nil {
			row.Status = "error"
			row.Error = err.Error()
			resp.Results = append(resp.Results, row)
			resp.Summary["error"]++
			continue
		}
		row.Status = status
		row.SkillID = applied.ID
		row.Tags = applied.Tags
		row.Name = applied.Name
		resp.Results = append(resp.Results, row)
		resp.Summary[status]++

		// Mirror the freshly-applied skill into R2R for retrieval.
		// On "unchanged" we skip — R2R already has the doc.
		if status == "created" || status == "updated" {
			s.recordR2RDocID(ctx, applied.ID)
			s.syncSkillToR2R(applied)
		}
	}
	writeEvoJSON(w, resp)
}

// startGithubImportWorkflow schedules an ImportSkillsFromGithubWorkflow
// and returns 202 with the workflow_id + a status_url the UI polls. The
// workflow itself owns the GitHub walker, dedup, and R2R mirror — the
// dashboard only validates the input shape and starts the run.
func (s *APIServer) startGithubImportWorkflow(w http.ResponseWriter, r *http.Request, req importRequest, kind string) {
	if s.temporalClient == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "temporal client not configured")
		return
	}

	// Per-kind required-field validation up front so the workflow
	// doesn't burn a retry budget on a 400-class input mistake.
	url := req.URL
	if kind == "github-repo" {
		// The legacy synchronous path read req.RepoURL; the workflow's
		// input only has a URL field, so we collapse the two.
		url = req.RepoURL
	}
	if strings.TrimSpace(url) == "" {
		writeEvoErr(w, http.StatusBadRequest, kind+" import: url is required")
		return
	}

	workflowID := "skill-import-" + uuid.New().String()
	in := skillsworkflows.ImportSkillsFromGithubInput{
		Kind:      kind,
		URL:       url,
		Subpath:   req.Subpath,
		Community: req.Community,
		Tags:      req.Tags,
	}
	opts := client.StartWorkflowOptions{
		ID:                    workflowID,
		TaskQueue:             skillsworkflows.TaskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}

	// Decouple from r.Context(): the workflow is durable and a client
	// hangup must not cancel the scheduling RPC. 5s is plenty for the
	// frontend gRPC roundtrip; if Temporal is wedged longer than that
	// we'd rather 5xx than pin the inflight slot.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.temporalClient.ExecuteWorkflow(ctx, opts, skillsworkflows.WorkflowImportSkillsFromGH, in); err != nil {
		s.logger.WithError(err).WithField("workflow_id", workflowID).
			Error("failed to start skill import workflow")
		writeEvoErr(w, http.StatusInternalServerError, "failed to start import workflow: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(importStartResponse{
		WorkflowID: workflowID,
		StatusURL:  "/api/v1/skills/imports/" + workflowID + "/progress",
	})
}

// gatherImportItems dispatches on req.Kind and returns the raw parsed
// items. Each item carries its own Err so a single bad file doesn't
// fail the whole batch.
func (s *APIServer) gatherImportItems(ctx context.Context, req importRequest) ([]localImportResult, string, error) {
	switch strings.ToLower(strings.TrimSpace(req.Kind)) {
	case "claude-files":
		path := req.Path
		if path == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, "", fmt.Errorf("resolve home dir: %w", err)
			}
			path = filepath.Join(home, ".claude", "skills")
		}
		items, err := importLocalDir(path, "claude-files")
		return items, "claude-files", err
	case "local-dir":
		if req.Path == "" {
			return nil, "", errors.New("local-dir import: path is required")
		}
		items, err := importLocalDir(req.Path, "manual")
		return items, "manual", err
	case "local-file":
		if req.Path == "" {
			return nil, "", errors.New("local-file import: path is required")
		}
		sk, err := importLocalFile(req.Path, "manual")
		return []localImportResult{{Skill: sk, Err: err, Path: req.Path}}, "manual", nil
	case "github-file":
		if req.URL == "" {
			return nil, "", errors.New("github-file import: url is required")
		}
		sk, err := importGithubRawURL(ctx, req.URL, "github")
		return []localImportResult{{Skill: sk, Err: err, Path: req.URL}}, "github", nil
	case "github-repo":
		if req.RepoURL == "" {
			return nil, "", errors.New("github-repo import: repo_url is required")
		}
		items, err := importGithubRepo(ctx, req.RepoURL, req.Subpath, req.Ref, "github")
		return items, "github", err
	default:
		return nil, "", fmt.Errorf("unknown import kind %q (expected one of: claude-files, local-dir, local-file, github-file, github-repo)", req.Kind)
	}
}

// applyImportedSkill is the dedup-aware writer:
//  1. If a row with the same source_url exists AND its source_sha
//     matches, return ``unchanged`` without touching it.
//  2. If source_url matches but sha differs, UPDATE → ``updated``.
//  3. Else INSERT. On UNIQUE(source_sha) collision the same body
//     content is already indexed under a different url → ``unchanged``.
//  4. On UNIQUE(name) collision return an error (caller can rename
//     the upstream file or remove the existing skill).
func (s *APIServer) applyImportedSkill(ctx context.Context, sk ParsedSkill) (Skill, string, error) {
	tags := sk.Tags
	if tags == nil {
		tags = []string{}
	}
	dependsOn := []string{}

	// Find by source_url first — that's the most reliable "same skill"
	// signal. If multiple rows share a url (shouldn't happen, but
	// defensively) we pick the most recently updated.
	var existing Skill
	var exists bool
	if sk.SourceURL != "" {
		row := s.evoPool.QueryRow(ctx, `
			SELECT id, name, description, body, when_to_use, example, community,
			       tags, depends_on, origin, source_url, source_sha, source_checked_at,
			       r2r_document_id, r2r_corpus_id, created_at, updated_at
			FROM evo.skills
			WHERE source_url = $1
			ORDER BY updated_at DESC
			LIMIT 1
		`, sk.SourceURL)
		sc, scanErr := scanSkill(row)
		if scanErr == nil {
			existing = sc
			exists = true
		} else if !errors.Is(scanErr, pgx.ErrNoRows) {
			return Skill{}, "", fmt.Errorf("lookup by url: %w", scanErr)
		}
	}

	if exists {
		if existing.SourceSHA != nil && *existing.SourceSHA == sk.SourceSHA {
			return existing, "unchanged", nil
		}
		// Update in place. Keep the existing name to avoid renaming
		// rows behind the user's back; everything else takes the new
		// upstream value.
		nowChecked := time.Now()
		row := s.evoPool.QueryRow(ctx, `
			UPDATE evo.skills SET
			  description = $1, body = $2, when_to_use = $3, example = $4,
			  community = COALESCE(NULLIF($5, ''), community),
			  tags = $6,
			  source_sha = $7,
			  source_checked_at = $8,
			  updated_at = NOW()
			WHERE id = $9
			RETURNING id, name, description, body, when_to_use, example, community,
			          tags, depends_on, origin, source_url, source_sha, source_checked_at,
			          r2r_document_id, r2r_corpus_id, created_at, updated_at
		`, sk.Description, sk.Body, nullable(sk.WhenToUse), nullable(sk.Example),
			sk.Community, tags, sk.SourceSHA, nowChecked, existing.ID)
		updated, err := scanSkill(row)
		if err != nil {
			return Skill{}, "", fmt.Errorf("update: %w", err)
		}
		return updated, "updated", nil
	}

	// Fresh INSERT path.
	nowChecked := time.Now()
	row := s.evoPool.QueryRow(ctx, `
		INSERT INTO evo.skills
		  (name, description, body, when_to_use, example, community,
		   tags, depends_on, origin, source_url, source_sha, source_checked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id, name, description, body, when_to_use, example, community,
		          tags, depends_on, origin, source_url, source_sha, source_checked_at,
		          r2r_document_id, r2r_corpus_id, created_at, updated_at
	`, sk.Name, sk.Description, sk.Body, nullable(sk.WhenToUse), nullable(sk.Example),
		nullable(sk.Community), tags, dependsOn, sk.Origin,
		nullable(sk.SourceURL), sk.SourceSHA, nowChecked)
	created, err := scanSkill(row)
	if err == nil {
		return created, "created", nil
	}
	if !isUniqueViolation(err) {
		return Skill{}, "", fmt.Errorf("insert: %w", err)
	}
	// Unique-violation: either name or source_sha collided. Disambiguate
	// by re-querying.
	conflict := s.evoPool.QueryRow(ctx, `
		SELECT id, name, description, body, when_to_use, example, community,
		       tags, depends_on, origin, source_url, source_sha, source_checked_at,
		       r2r_document_id, r2r_corpus_id, created_at, updated_at
		FROM evo.skills
		WHERE source_sha = $1 OR name = $2
		ORDER BY (source_sha = $1) DESC, updated_at DESC
		LIMIT 1
	`, sk.SourceSHA, sk.Name)
	conflictSkill, cErr := scanSkill(conflict)
	if cErr != nil {
		return Skill{}, "", fmt.Errorf("insert collision but could not identify cause: %w (original %v)", cErr, err)
	}
	if conflictSkill.SourceSHA != nil && *conflictSkill.SourceSHA == sk.SourceSHA {
		// Same content already indexed under a different name/url.
		return conflictSkill, "unchanged", nil
	}
	// Name collision with different content.
	return Skill{}, "", fmt.Errorf("a different skill already exists with name %q (id %s)", conflictSkill.Name, conflictSkill.ID)
}

// nullable returns a *string so the SQL driver inserts NULL when the
// string is empty (rather than the empty string).
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mergeTags(existing, extra []string) []string {
	if len(extra) == 0 {
		return existing
	}
	seen := make(map[string]bool, len(existing)+len(extra))
	out := make([]string, 0, len(existing)+len(extra))
	for _, t := range existing {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range extra {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ---------------- check-source ----------------

type checkSourceResponse struct {
	Changed  bool   `json:"changed"`
	NewSHA   string `json:"new_sha,omitempty"`
	OldSHA   string `json:"old_sha,omitempty"`
	NewBody  string `json:"new_body,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (s *APIServer) checkSkillSource(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	id := mux.Vars(r)["id"]
	if id == "" {
		writeEvoErr(w, http.StatusBadRequest, "id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	row := s.evoPool.QueryRow(ctx, `
		SELECT id, name, description, body, when_to_use, example, community,
		       tags, depends_on, origin, source_url, source_sha, source_checked_at,
		       r2r_document_id, r2r_corpus_id, created_at, updated_at
		FROM evo.skills WHERE id = $1
	`, id)
	sk, err := scanSkill(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeEvoErr(w, http.StatusNotFound, "skill not found")
			return
		}
		writeEvoErr(w, http.StatusInternalServerError, "lookup: "+err.Error())
		return
	}
	if sk.SourceURL == nil || *sk.SourceURL == "" {
		writeEvoErr(w, http.StatusBadRequest, "skill has no source_url; nothing to check")
		return
	}

	parsed, err := refetchSource(ctx, *sk.SourceURL, sk.Origin, sk.Name)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, "refetch failed: "+err.Error())
		return
	}

	// Stamp checked_at regardless of outcome — confirms we looked.
	if _, uerr := s.evoPool.Exec(ctx,
		`UPDATE evo.skills SET source_checked_at = NOW() WHERE id = $1`, id); uerr != nil {
		s.logger.WithError(uerr).Warn("check-source: failed to update source_checked_at")
	}

	resp := checkSourceResponse{
		NewSHA: parsed.SourceSHA,
	}
	if sk.SourceSHA != nil {
		resp.OldSHA = *sk.SourceSHA
	}
	if resp.OldSHA == resp.NewSHA {
		resp.Changed = false
		resp.Reason = "source sha matches existing skill"
	} else {
		resp.Changed = true
		resp.NewBody = parsed.Body
		resp.Reason = "source content differs from stored sha"
	}
	writeEvoJSON(w, resp)
}

// refetchSource fetches a source_url and returns the parsed result.
// Handles file:// + http(s):// (including github blob URLs via the
// importer's URL normaliser).
func refetchSource(ctx context.Context, source, origin, defaultName string) (ParsedSkill, error) {
	switch {
	case strings.HasPrefix(source, "file://"):
		path := strings.TrimPrefix(source, "file://")
		return importLocalFile(path, origin)
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		return importGithubRawURL(ctx, source, origin)
	case strings.HasPrefix(source, "delegator-task:"):
		return ParsedSkill{}, errors.New("delegator-mined skills don't have a re-fetchable source")
	default:
		return ParsedSkill{}, fmt.Errorf("unsupported source_url scheme: %s", source)
	}
}
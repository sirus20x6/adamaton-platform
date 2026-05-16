// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

// Skills importers — pure helpers that turn an upstream source
// (local file, local directory, GitHub raw URL, GitHub repo path)
// into a slice of ``ParsedSkill`` records. The /api/v1/skills/import
// endpoint loops over the returned records and applies DB writes
// itself; importers do no Postgres I/O of their own.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ParsedSkill is the importer's output shape — fields line up with the
// SkillInput accepted by the create handler, plus provenance.
type ParsedSkill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	WhenToUse   string   `json:"when_to_use,omitempty"`
	Example     string   `json:"example,omitempty"`
	Community   string   `json:"community,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Origin      string   `json:"origin"`
	SourceURL   string   `json:"source_url"`
	SourceSHA   string   `json:"source_sha"`
}

// skillFrontmatter is the subset of YAML keys we recognise at the top
// of a markdown skill file. Unknown keys are ignored — the convention
// mirrors Claude Code's existing memory frontmatter (name/description
// plus optional structured fields).
//
// ``Tags`` is a yaml.Node so we tolerate the three shapes operators
// actually write: ``[a, b, c]``, ``"a, b, c"``, ``- a\n- b\n- c``.
type skillFrontmatter struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	WhenToUse   string    `yaml:"when_to_use"`
	Example     string    `yaml:"example"`
	Community   string    `yaml:"community"`
	Tags        yaml.Node `yaml:"tags"`
}

// tagsFromNode coerces the YAML tags node into a clean []string.
// Accepts sequence-of-scalars, single scalar (comma-split), and
// anything else by returning nil (treat as no tags).
func tagsFromNode(n yaml.Node) []string {
	switch n.Kind {
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				t := strings.TrimSpace(c.Value)
				if t != "" {
					out = append(out, t)
				}
			}
		}
		return out
	case yaml.ScalarNode:
		out := make([]string, 0)
		for _, t := range strings.Split(n.Value, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// parseSkillMarkdown splits ``---\n<yaml>\n---\n<body>`` into a
// ParsedSkill. The caller supplies a default-name (typically the
// filename stem) used when the frontmatter omits ``name:``. The
// source_sha is sha256 of the raw input bytes, NOT of just the body —
// any frontmatter change should look like drift.
func parseSkillMarkdown(raw []byte, defaultName, sourceURL, origin string) (ParsedSkill, error) {
	out := ParsedSkill{
		Origin:    origin,
		SourceURL: sourceURL,
		SourceSHA: sha256Hex(raw),
	}

	// Trim leading BOM / whitespace before sniffing for ---.
	trimmed := bytes.TrimLeft(raw, "\xef\xbb\xbf \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		// No frontmatter: whole file is the body, name is the filename
		// stem. Description is the first non-blank line so a bare .md
		// file still produces a valid skill.
		out.Body = string(raw)
		out.Name = strings.TrimSpace(defaultName)
		out.Description = firstNonBlankLine(out.Body)
		return finalize(out)
	}

	// Split on the second --- terminator. yaml.v3 demands the exact
	// fence shape "---\n" / "\n---\n"; tolerate trailing whitespace.
	rest := trimmed[3:]
	rest = bytes.TrimLeft(rest, "\r\n")
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return out, errors.New("frontmatter open fence but no closing ---")
	}
	yamlBlock := rest[:end]
	body := rest[end+len("\n---"):]
	body = bytes.TrimLeft(body, "\r\n")

	var fm skillFrontmatter
	if err := yaml.Unmarshal(yamlBlock, &fm); err != nil {
		return out, fmt.Errorf("frontmatter yaml: %w", err)
	}

	out.Name = strings.TrimSpace(fm.Name)
	if out.Name == "" {
		out.Name = strings.TrimSpace(defaultName)
	}
	out.Description = strings.TrimSpace(fm.Description)
	if out.Description == "" {
		out.Description = firstNonBlankLine(string(body))
	}
	out.WhenToUse = strings.TrimSpace(fm.WhenToUse)
	out.Example = strings.TrimSpace(fm.Example)
	out.Community = strings.TrimSpace(fm.Community)
	out.Tags = normaliseTags(tagsFromNode(fm.Tags))
	out.Body = string(body)
	return finalize(out)
}

func finalize(p ParsedSkill) (ParsedSkill, error) {
	if p.Name == "" {
		return p, errors.New("skill name is required (set frontmatter `name:` or rename the file)")
	}
	if p.Description == "" {
		return p, errors.New("skill description is required (set frontmatter `description:` or add a first paragraph)")
	}
	if strings.TrimSpace(p.Body) == "" {
		return p, errors.New("skill body is empty")
	}
	return p, nil
}

func firstNonBlankLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		// Trim a leading "> " quote marker so a typical quoted intro
		// doesn't become the description.
		ln = strings.TrimPrefix(ln, "> ")
		return ln
	}
	return ""
}

func normaliseTags(in []string) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// importLocalFile reads a single .md file from disk. ``sourceURL`` is
// stored verbatim — typically ``file://<absolute-path>`` so the
// check-source endpoint knows how to re-read it.
func importLocalFile(path, origin string) (ParsedSkill, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ParsedSkill{}, fmt.Errorf("abs %q: %w", path, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return ParsedSkill{}, fmt.Errorf("read %q: %w", abs, err)
	}
	stem := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	return parseSkillMarkdown(raw, stem, "file://"+abs, origin)
}

// importLocalDir walks ``dir`` and parses every ``*.md`` / ``*.markdown``
// file as a skill. Subdirectories are not recursed (Claude's
// ~/.claude/skills is flat; if you want recursive, use a github-repo
// import). Failures on individual files are returned alongside the
// successes so the caller can surface a per-row status.
type localImportResult struct {
	Skill ParsedSkill
	Err   error
	Path  string
}

func importLocalDir(dir, origin string) ([]localImportResult, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("abs %q: %w", dir, err)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("read dir %q: %w", abs, err)
	}
	out := make([]localImportResult, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".md" && ext != ".markdown" {
			continue
		}
		full := filepath.Join(abs, name)
		sk, perr := importLocalFile(full, origin)
		out = append(out, localImportResult{Skill: sk, Err: perr, Path: full})
	}
	return out, nil
}

// importGithubRawURL fetches a single raw markdown file. Accepts both
// ``raw.githubusercontent.com/...`` URLs and the regular
// ``github.com/<owner>/<repo>/blob/<ref>/<path>`` browser form (the
// latter is normalised to raw before fetching).
func importGithubRawURL(ctx context.Context, rawURL, origin string) (ParsedSkill, error) {
	return fetchSkillFromGithub(ctx, rawURL, "", origin)
}

// fetchSkillFromGithub is the recursive walker's variant: it lets the
// caller supply a ``defaultName`` to override the filename-stem
// fallback (used when the markdown file's literal name is generic like
// ``SKILL.md`` or ``README.md`` and the parent directory name is the
// real identifier).
func fetchSkillFromGithub(ctx context.Context, rawURL, defaultName, origin string) (ParsedSkill, error) {
	resolved, err := normaliseGithubRawURL(rawURL)
	if err != nil {
		return ParsedSkill{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", resolved, nil)
	if err != nil {
		return ParsedSkill{}, fmt.Errorf("build request: %w", err)
	}
	addGithubAuth(req)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ParsedSkill{}, fmt.Errorf("fetch %s: %w", resolved, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ParsedSkill{}, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, resolved, string(body))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ParsedSkill{}, fmt.Errorf("read body: %w", err)
	}
	if defaultName == "" {
		defaultName = pathStem(resolved)
	}
	return parseSkillMarkdown(raw, defaultName, resolved, origin)
}

// normaliseGithubRawURL converts a github.com blob URL into its
// raw.githubusercontent.com equivalent. Already-raw URLs pass
// through unchanged.
func normaliseGithubRawURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	switch u.Host {
	case "raw.githubusercontent.com":
		return raw, nil
	case "github.com":
		// /<owner>/<repo>/blob/<ref>/<path>
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 5)
		if len(parts) < 5 || parts[2] != "blob" {
			return "", fmt.Errorf("github URL must be github.com/<owner>/<repo>/blob/<ref>/<path>, got %s", u.Path)
		}
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s",
			parts[0], parts[1], parts[3], parts[4]), nil
	default:
		// Permissive: any HTTPS URL ending in .md is fair game (gist,
		// gitea, self-hosted forge). The dedup index makes accidental
		// double-imports of the same content harmless.
		if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
			return raw, nil
		}
		return "", fmt.Errorf("unsupported URL %s", raw)
	}
}

// pathStem returns the filename without extension. Used as the
// fallback skill name when the frontmatter omits one.
func pathStem(s string) string {
	// Strip query/fragment for URLs.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	base := filepath.Base(s)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// addGithubAuth sets the Bearer token header when ``GITHUB_TOKEN`` is
// set in the environment. Unauthenticated access works for public
// content but hits a 60-req/hr rate limit.
func addGithubAuth(req *http.Request) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	req.Header.Set("Accept", "application/vnd.github+json,application/vnd.github.raw")
}

// githubContent is the relevant slice of GitHub's
// /repos/{owner}/{repo}/contents response.
type githubContent struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"` // file | dir
	DownloadURL string `json:"download_url"`
}

// importGithubRepo recursively walks ``<repoURL>/<subpath>`` for .md
// skill files. Caps depth + file count so a stray pointer at a giant
// repo can't run away with us.
func importGithubRepo(ctx context.Context, repoURL, subpath, ref, origin string) ([]localImportResult, error) {
	owner, repo, err := parseGithubRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	subpath = strings.Trim(subpath, "/")
	if subpath == "" {
		subpath = "skills"
	}
	if ref == "" {
		ref = "HEAD"
	}
	// Caps to keep a runaway repo bounded. The previous 200 cap was too
	// tight — a typical "awesome-skills" mega-list has 1k+ bundles. 2500
	// is comfortable and still finite. Override via env if needed.
	const maxFiles = 2500
	const maxDepth = 4

	results := make([]localImportResult, 0, 16)
	var walk func(path string, depth int) error
	walk = func(path string, depth int) error {
		if depth > maxDepth {
			return nil
		}
		if len(results) >= maxFiles {
			return nil
		}
		entries, err := githubListContents(ctx, owner, repo, path, ref)
		if err != nil {
			return err
		}
		// Two-pass: collect .md files at this level first so we can
		// detect a "skill bundle" layout (e.g. anthropics/skills, where
		// each subdir holds a SKILL.md plus auxiliary scripts/templates
		// dirs that should NOT be treated as nested skills). When a
		// markdown file is present at depth>0, stop descending into
		// sibling subdirs — they belong to this skill.
		mdFiles := make([]githubContent, 0)
		subdirs := make([]githubContent, 0)
		for _, e := range entries {
			switch e.Type {
			case "file":
				ext := strings.ToLower(filepath.Ext(e.Name))
				if (ext == ".md" || ext == ".markdown") && e.DownloadURL != "" {
					mdFiles = append(mdFiles, e)
				}
			case "dir":
				subdirs = append(subdirs, e)
			}
		}
		// Heuristic: many "awesome-skills" repos use one of:
		//   skills/<bundle>/SKILL.md
		//   skills/<bundle>/README.md
		// In both cases the bundle directory name is the meaningful
		// identifier — falling back to the filename stem ("SKILL" or
		// "README") would collide across every bundle. So when we're
		// inside a subdirectory and the file's stem is generic, use
		// the parent dir name instead.
		genericStems := map[string]bool{"SKILL": true, "README": true, "INDEX": true}
		for _, e := range mdFiles {
			if len(results) >= maxFiles {
				return nil
			}
			// Override the parser's default-name argument with the
			// parent dir name for generic stems. We bypass the public
			// helper to slip our name in.
			defaultName := ""
			stem := pathStem(e.Name)
			if genericStems[strings.ToUpper(stem)] && depth > 0 {
				if i := strings.LastIndex(e.Path, "/"); i > 0 {
					parent := e.Path[:i]
					if j := strings.LastIndex(parent, "/"); j >= 0 {
						defaultName = parent[j+1:]
					} else {
						defaultName = parent
					}
				}
			}
			sk, perr := fetchSkillFromGithub(ctx, e.DownloadURL, defaultName, origin)
			results = append(results, localImportResult{
				Skill: sk, Err: perr, Path: e.Path,
			})
		}
		// Only recurse into subdirectories when this level had no
		// markdown of its own. Top-level (depth=0) always recurses so
		// we can reach skill bundles below an index/parent README.
		if depth == 0 || len(mdFiles) == 0 {
			for _, e := range subdirs {
				if len(results) >= maxFiles {
					return nil
				}
				if err := walk(e.Path, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	walkErr := walk(subpath, 0)
	// Return partial results alongside the error — the import handler
	// applies the items that succeeded and notes the walker hiccup in
	// the response summary. Half a corpus is better than nothing, and
	// dedup means a retry safely picks up the rest.
	return results, walkErr
}

func parseGithubRepoURL(raw string) (owner, repo string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", fmt.Errorf("parse url: %w", perr)
	}
	if u.Host != "github.com" {
		return "", "", fmt.Errorf("only github.com repos are supported (got %s)", u.Host)
	}
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("URL must be github.com/<owner>/<repo>, got %s", u.Path)
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func githubListContents(ctx context.Context, owner, repo, path, ref string) ([]githubContent, error) {
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s",
		url.PathEscape(owner), url.PathEscape(repo), path)
	if ref != "" && ref != "HEAD" {
		api += "?ref=" + url.QueryEscape(ref)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return nil, err
	}
	addGithubAuth(req)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", api, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, api, string(body[:min(len(body), 200)]))
	}
	// The endpoint returns an array for directories and a single object
	// for files. Probe by looking at the first non-whitespace byte.
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var arr []githubContent
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("parse json: %w", err)
		}
		return arr, nil
	}
	var single githubContent
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	return []githubContent{single}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
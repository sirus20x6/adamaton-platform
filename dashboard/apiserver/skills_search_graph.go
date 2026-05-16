// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

// /api/v1/skills/search and /api/v1/skills/graph — Phase 4 endpoints
// that the delegator (Phase 5) and the UI's Graph tab consume.
//
//   /skills/search   POST  proxies to R2R /v3/retrieval/search with a
//                          collection filter scoped to the Skills corpus.
//   /skills/graph    GET   returns nodes + edges suitable for a force-
//                          directed view. Nodes come from evo.skills;
//                          edges come from (a) evo.skills.depends_on
//                          (always available) and (b) R2R's extracted
//                          graph_relationships if the Skills corpus has
//                          graph extraction enabled (best-effort).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// skillsSearchRequest is the thin shape the dashboard accepts. Internal
// callers (the delegator integration coming in Phase 5) pass the same
// body. We translate it into R2R's SearchSettings shape server-side so
// the rest of the system doesn't need to know R2R's vocabulary.
type skillsSearchRequest struct {
	Query   string  `json:"query"`
	Limit   int     `json:"limit,omitempty"`
	CorpusID string `json:"corpus_id,omitempty"` // optional override
}

// skillsSearchHit is the dashboard-facing search hit. Each chunk's
// skill_id metadata is hoisted to the top level so the UI can jump
// straight to the canonical row.
type skillsSearchHit struct {
	SkillID    string                 `json:"skill_id,omitempty"`
	SkillName  string                 `json:"skill_name,omitempty"`
	ChunkKind  string                 `json:"chunk_kind,omitempty"`
	Score      float64                `json:"score"`
	Text       string                 `json:"text"`
	Community  string                 `json:"community,omitempty"`
	Tags       []string               `json:"tags,omitempty"`
	DocumentID string                 `json:"document_id,omitempty"`
	Metadata   map[string]interface{} `json:"metadata,omitempty"`
}

type skillsSearchResponse struct {
	Hits  []skillsSearchHit `json:"hits"`
	Error string            `json:"error,omitempty"`
}

func (s *APIServer) searchSkills(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	var req skillsSearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeEvoErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Query == "" {
		writeEvoErr(w, http.StatusBadRequest, "query is required")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}
	corpus := req.CorpusID
	if corpus == "" {
		corpus = skillsR2RCorpusID()
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	hits, err := s.proxySkillsSearch(ctx, req.Query, req.Limit, corpus)
	if err != nil {
		writeEvoErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeEvoJSON(w, skillsSearchResponse{Hits: hits})
}

// proxySkillsSearch builds an R2R retrieval payload, posts it, and
// converts the result into the dashboard's compact ``skillsSearchHit``
// shape. Exposed separately so Phase 5's in-process delegator
// integration can call it without going through HTTP.
func (s *APIServer) proxySkillsSearch(ctx context.Context, query string, limit int, corpus string) ([]skillsSearchHit, error) {
	base := s.deepResearchURL()
	if base == "" {
		return nil, errors.New("deepresearch URL not configured")
	}

	// R2R SearchSettings: filter to the Skills collection if known,
	// limit results, prefer the meta chunks (lower order_in_doc) by
	// asking for a slightly larger pool so the UI can de-dupe by
	// skill_id without losing diverse hits.
	settings := map[string]interface{}{
		"limit": limit * 3,
		"include_metadatas": true,
	}
	if corpus != "" {
		settings["filters"] = map[string]interface{}{
			"collection_ids": map[string]interface{}{"$overlap": []string{corpus}},
		}
	}
	payload := map[string]interface{}{
		"query":           query,
		"search_mode":     "custom",
		"search_settings": settings,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", base+"/v3/retrieval/search", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.deepResearchHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return parseR2RSearchHits(body, limit)
}

// parseR2RSearchHits handles R2R's response shape, which wraps the
// chunks under ``results.chunk_search_results``. We dedupe by skill_id
// so a "meta + 3 body" overlap doesn't crowd the top results.
func parseR2RSearchHits(body []byte, limit int) ([]skillsSearchHit, error) {
	var raw struct {
		Results struct {
			ChunkResults []struct {
				ID         string                 `json:"id"`
				DocumentID string                 `json:"document_id"`
				Text       string                 `json:"text"`
				Score      float64                `json:"score"`
				Metadata   map[string]interface{} `json:"metadata"`
			} `json:"chunk_search_results"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse upstream: %w (body: %s)", err, truncate(string(body), 200))
	}
	seen := map[string]bool{}
	out := make([]skillsSearchHit, 0, limit)
	for _, c := range raw.Results.ChunkResults {
		md := c.Metadata
		skID := stringField(md, "skill_id")
		if skID == "" {
			// Fall back to the R2R document_id — for our Skill docs the
			// two are identical, but defensively guard against drift.
			skID = c.DocumentID
		}
		if skID != "" && seen[skID] {
			continue
		}
		hit := skillsSearchHit{
			SkillID:    skID,
			SkillName:  stringField(md, "skill_name"),
			ChunkKind:  stringField(md, "chunk_kind"),
			DocumentID: c.DocumentID,
			Text:       c.Text,
			Score:      c.Score,
			Community:  stringField(md, "community"),
			Metadata:   md,
		}
		if tags, ok := md["tags"].([]interface{}); ok {
			hit.Tags = make([]string, 0, len(tags))
			for _, t := range tags {
				if s, ok := t.(string); ok {
					hit.Tags = append(hit.Tags, s)
				}
			}
		}
		out = append(out, hit)
		if skID != "" {
			seen[skID] = true
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func stringField(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ----------------------- graph endpoint -----------------------

type graphNode struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Community string   `json:"community,omitempty"`
	Origin    string   `json:"origin,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

type graphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Kind   string `json:"kind"` // "depends_on" | "r2r"
	Label  string `json:"label,omitempty"`
	Weight float64 `json:"weight,omitempty"`
}

type graphResponse struct {
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
	// R2RAvailable indicates whether the upstream graph extraction
	// contributed edges. The UI uses it to surface "graph extraction
	// pending" rather than implying the relationships truly don't exist.
	R2RAvailable bool   `json:"r2r_available"`
	R2RNote      string `json:"r2r_note,omitempty"`
}

func (s *APIServer) skillsGraph(w http.ResponseWriter, r *http.Request) {
	if s.evoPool == nil {
		writeEvoErr(w, http.StatusServiceUnavailable, "evo pool not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Nodes + intrinsic edges come from evo.skills — always available.
	rows, err := s.evoPool.Query(ctx, `
		SELECT id::text, name, community, origin, tags, depends_on
		FROM evo.skills
		ORDER BY community NULLS LAST, name
	`)
	if err != nil {
		writeEvoErr(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := graphResponse{Nodes: []graphNode{}, Edges: []graphEdge{}}
	nodeIDs := map[string]bool{}
	for rows.Next() {
		var (
			id, name        string
			community, origin *string
			tags            []string
			depends         []string
		)
		if err := rows.Scan(&id, &name, &community, &origin, &tags, &depends); err != nil {
			writeEvoErr(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		n := graphNode{ID: id, Name: name, Tags: tags}
		if community != nil {
			n.Community = *community
		}
		if origin != nil {
			n.Origin = *origin
		}
		out.Nodes = append(out.Nodes, n)
		nodeIDs[id] = true
		for _, dep := range depends {
			out.Edges = append(out.Edges, graphEdge{
				Source: id, Target: dep, Kind: "depends_on",
			})
		}
	}

	// R2R-extracted relationships are layered on top when the corpus
	// is configured. Failures are non-fatal — the dashboard still
	// renders the intrinsic graph.
	corpus := r.URL.Query().Get("corpus_id")
	if corpus == "" {
		corpus = skillsR2RCorpusID()
	}
	if corpus != "" {
		edges, note, ok := s.fetchR2RRelationships(ctx, corpus, nodeIDs)
		out.Edges = append(out.Edges, edges...)
		out.R2RAvailable = ok
		out.R2RNote = note
	} else {
		out.R2RNote = "SKILLS_R2R_CORPUS_ID not configured; only intrinsic depends_on edges shown"
	}

	writeEvoJSON(w, out)
}

// fetchR2RRelationships hits ``/v3/graphs/{corpus}/relationships`` and
// keeps only the edges whose subject + object map onto existing
// skill nodes. Returns ``ok=false`` on any upstream failure so the UI
// surfaces a status pill rather than a stale-looking empty graph.
func (s *APIServer) fetchR2RRelationships(ctx context.Context, corpus string, nodeIDs map[string]bool) ([]graphEdge, string, bool) {
	base := s.deepResearchURL()
	if base == "" {
		return nil, "deepresearch URL not configured", false
	}
	url := fmt.Sprintf("%s/v3/graphs/%s/relationships?limit=1000",
		base, corpus)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, "build request: " + err.Error(), false
	}
	resp, err := s.deepResearchHTTPClient().Do(req)
	if err != nil {
		return nil, "upstream call: " + err.Error(), false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode >= 400 {
		return nil, "upstream HTTP " + strconv.Itoa(resp.StatusCode), false
	}

	var raw struct {
		Results []struct {
			ID          string  `json:"id"`
			Subject     string  `json:"subject"`
			SubjectID   string  `json:"subject_id"`
			Predicate   string  `json:"predicate"`
			Object      string  `json:"object"`
			ObjectID    string  `json:"object_id"`
			Weight      float64 `json:"weight"`
			ChunkIDs    []string `json:"chunk_ids"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		// R2R may also nest under .results.relationships — try once.
		var fallback struct {
			Results struct {
				Relationships []json.RawMessage `json:"relationships"`
			} `json:"results"`
		}
		if err2 := json.Unmarshal(body, &fallback); err2 == nil && len(fallback.Results.Relationships) > 0 {
			// Best-effort: hand back what we got without re-parsing each item.
			return nil, "upstream relationships present but in unexpected shape", true
		}
		return nil, "parse: " + err.Error(), false
	}

	out := make([]graphEdge, 0)
	for _, e := range raw.Results {
		// We only display edges that connect two skills we know about.
		// R2R may extract entities that aren't skills (Library, Tool, etc.) —
		// those land in the graph_entities table but aren't node IDs in
		// our view.
		if e.SubjectID == "" || e.ObjectID == "" {
			continue
		}
		if !nodeIDs[e.SubjectID] || !nodeIDs[e.ObjectID] {
			continue
		}
		out = append(out, graphEdge{
			Source: e.SubjectID, Target: e.ObjectID,
			Kind: "r2r", Label: e.Predicate, Weight: e.Weight,
		})
	}
	return out, fmt.Sprintf("loaded %d r2r relationships", len(out)), true
}
// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"strings"
	"testing"
)

func TestParseR2RSearchHitsDedupesBySkillID(t *testing.T) {
	body := []byte(`{
	  "results": {
	    "chunk_search_results": [
	      {"id":"c1","document_id":"d1","text":"meta chunk text","score":0.91,
	       "metadata":{"skill_id":"d1","skill_name":"extract-method",
	                   "chunk_kind":"skill_meta","community":"code-refactoring",
	                   "tags":["refactor","any-language"]}},
	      {"id":"c2","document_id":"d1","text":"body chunk text","score":0.85,
	       "metadata":{"skill_id":"d1","skill_name":"extract-method",
	                   "chunk_kind":"skill_body"}},
	      {"id":"c3","document_id":"d2","text":"rename meta","score":0.82,
	       "metadata":{"skill_id":"d2","skill_name":"rename-variable",
	                   "chunk_kind":"skill_meta","tags":["refactor"]}}
	    ]
	  }
	}`)
	hits, err := parseR2RSearchHits(body, 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits (dedup by skill_id), got %d", len(hits))
	}
	if hits[0].SkillID != "d1" || hits[0].SkillName != "extract-method" {
		t.Errorf("first hit wrong: %+v", hits[0])
	}
	if hits[0].ChunkKind != "skill_meta" {
		t.Errorf("expected first hit to be the meta chunk (higher score), got %q", hits[0].ChunkKind)
	}
	if len(hits[0].Tags) != 2 || hits[0].Tags[0] != "refactor" {
		t.Errorf("tags not preserved: %v", hits[0].Tags)
	}
	if hits[1].SkillID != "d2" || hits[1].SkillName != "rename-variable" {
		t.Errorf("second hit wrong: %+v", hits[1])
	}
}

func TestParseR2RSearchHitsHandlesEmpty(t *testing.T) {
	body := []byte(`{"results":{"chunk_search_results":[]}}`)
	hits, err := parseR2RSearchHits(body, 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected 0 hits, got %d", len(hits))
	}
}

func TestParseR2RSearchHitsRejectsBadJSON(t *testing.T) {
	_, err := parseR2RSearchHits([]byte("not json"), 10)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse upstream") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestParseR2RSearchHitsHonoursLimit(t *testing.T) {
	chunks := strings.Join([]string{
		`{"id":"c1","document_id":"d1","text":"a","score":0.9,"metadata":{"skill_id":"d1"}}`,
		`{"id":"c2","document_id":"d2","text":"b","score":0.8,"metadata":{"skill_id":"d2"}}`,
		`{"id":"c3","document_id":"d3","text":"c","score":0.7,"metadata":{"skill_id":"d3"}}`,
		`{"id":"c4","document_id":"d4","text":"d","score":0.6,"metadata":{"skill_id":"d4"}}`,
	}, ",")
	body := []byte(`{"results":{"chunk_search_results":[` + chunks + `]}}`)
	hits, err := parseR2RSearchHits(body, 2)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("limit not applied: got %d hits", len(hits))
	}
}
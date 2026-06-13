// Cache hit/refresh tests for the /system/status skills aggregate. The
// four-subquery COUNT is now served through a short-TTL in-process cache;
// these tests pin the miss→hit→refresh behaviour. DB-backed (skips when
// the evo DB is unavailable).
package apiserver

import (
	"context"
	"testing"
	"time"
)

func TestCachedSkillsCounts_HitAndRefresh(t *testing.T) {
	s := newDBTestServer(t) // skips if DB unavailable
	skillsCountsCacheTestReset()
	t.Cleanup(skillsCountsCacheTestReset)

	ctx := context.Background()

	// First call: cold cache -> miss (cached == false), value fetched.
	v1, cached1, err := s.cachedSkillsCounts(ctx)
	if err != nil {
		t.Fatalf("first cachedSkillsCounts: %v", err)
	}
	if cached1 {
		t.Error("first call on a cold cache should report cached=false (a miss)")
	}

	// Second call within TTL: hit (cached == true), identical value.
	v2, cached2, err := s.cachedSkillsCounts(ctx)
	if err != nil {
		t.Fatalf("second cachedSkillsCounts: %v", err)
	}
	if !cached2 {
		t.Error("second call within TTL should report cached=true (a hit)")
	}
	if v1 != v2 {
		t.Errorf("cached value changed within TTL: %+v vs %+v", v1, v2)
	}

	// Force expiry by back-dating the fetch time, then confirm a refresh.
	skillsCountsCache.mu.Lock()
	skillsCountsCache.fetchedAt = time.Now().Add(-2 * skillsCountsTTL)
	skillsCountsCache.mu.Unlock()

	_, cached3, err := s.cachedSkillsCounts(ctx)
	if err != nil {
		t.Fatalf("post-expiry cachedSkillsCounts: %v", err)
	}
	if cached3 {
		t.Error("call after TTL expiry should refresh (cached=false)")
	}
}

func TestCheckSkills_StatsShapePreserved(t *testing.T) {
	s := newDBTestServer(t)
	skillsCountsCacheTestReset()
	t.Cleanup(skillsCountsCacheTestReset)

	st := s.checkSkills(context.Background())
	if st.Name != "skills" {
		t.Errorf("subsystem name = %q, want skills", st.Name)
	}
	if st.Status != "ok" {
		t.Skipf("skills subsystem not ok (DB may lack evo.skills): %s", st.Detail)
	}
	// The original four keys must still be present (response shape parity).
	for _, k := range []string{"skills", "communities", "usages", "tasks_today"} {
		if _, ok := st.Stats[k]; !ok {
			t.Errorf("stats missing key %q: %+v", k, st.Stats)
		}
	}
	// The additive observability key reflects the cache state.
	if _, ok := st.Stats["cached"]; !ok {
		t.Error("stats should include the additive 'cached' key")
	}
}

package apiserver

// Kanban stale-claim sweeper — platform copy with observability.
//
// The delegator-mcp process runs the original sweeper (delegator/
// kanban_sweeper.go) but emits no metrics, so a broken sweeper is
// indistinguishable from a healthy idle one. This apiserver-side sweeper runs
// the same idempotent reclaim UPDATE (safe to run alongside the delegator's —
// whichever fires first wins the rows) and additionally:
//
//   - counts runs / errors / reclaimed cards (atomics),
//   - stamps the last run + last error time,
//   - logs every pass structurally (Debug when idle, Info on reclaim,
//     Warn on failure),
//   - surfaces everything at GET /api/v1/kanban/sweeper/status so the
//     dashboard (and `bin/adam fleet verify`) can alert on "no recent run".
//
// Tuning: EVO_KANBAN_CLAIM_TTL_MINUTES (default 30), EVO_KANBAN_SWEEP_
// INTERVAL_MINUTES (default 5), EVO_KANBAN_SWEEPER_DISABLED=1 to opt out.

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// kanbanSweeperStats aggregates sweeper observability. All fields atomic —
// written by the sweep goroutine, read by the status handler.
type kanbanSweeperStats struct {
	enabled        atomic.Bool
	runs           atomic.Int64
	errors         atomic.Int64
	cardsReclaimed atomic.Int64
	lastRunUnix    atomic.Int64
	lastErrorUnix  atomic.Int64
	lastReclaimed  atomic.Int64
	ttlSeconds     atomic.Int64
	intervalSecs   atomic.Int64
}

// reclaimStaleKanbanClaimsSQL mirrors the delegator sweeper's UPDATE: flip
// every stale 'claimed' card back to 'unclaimed', clearing claim metadata.
const reclaimStaleKanbanClaimsSQL = `
UPDATE evo.kanban_cards
   SET claim_status = 'unclaimed',
       claimed_by   = NULL,
       claim_token  = NULL,
       claimed_at   = NULL,
       updated_at   = NOW()
 WHERE claim_status = 'claimed'
   AND claimed_at IS NOT NULL
   AND claimed_at < NOW() - ($1 * INTERVAL '1 second')`

func envMinutes(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return time.Duration(n) * time.Minute
}

// StartKanbanSweeper launches the observable stale-claim sweep loop. No-op
// when the pool is nil or EVO_KANBAN_SWEEPER_DISABLED is set. Called from
// NewAPIServer with a background context (process lifetime).
func (s *APIServer) StartKanbanSweeper(ctx context.Context) {
	if s.evoPool == nil {
		s.logger.Debug("kanban sweeper: no evo pool; disabled")
		return
	}
	if os.Getenv("EVO_KANBAN_SWEEPER_DISABLED") != "" {
		s.logger.Info("kanban sweeper: disabled via EVO_KANBAN_SWEEPER_DISABLED")
		return
	}
	ttl := envMinutes("EVO_KANBAN_CLAIM_TTL_MINUTES", 30*time.Minute)
	interval := envMinutes("EVO_KANBAN_SWEEP_INTERVAL_MINUTES", 5*time.Minute)

	s.kanbanSweep.enabled.Store(true)
	s.kanbanSweep.ttlSeconds.Store(int64(ttl.Seconds()))
	s.kanbanSweep.intervalSecs.Store(int64(interval.Seconds()))
	s.logger.WithField("ttl", ttl.String()).WithField("interval", interval.String()).
		Info("kanban sweeper: started (apiserver)")

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Prompt first pass so a freshly-booted apiserver reclaims promptly.
		s.sweepKanbanOnce(ctx, ttl)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepKanbanOnce(ctx, ttl)
			}
		}
	}()
}

// sweepKanbanOnce runs one reclaim pass, updating counters + logs. Returns
// the number of cards reclaimed (for tests).
func (s *APIServer) sweepKanbanOnce(ctx context.Context, ttl time.Duration) int64 {
	sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	now := time.Now()
	s.kanbanSweep.runs.Add(1)
	s.kanbanSweep.lastRunUnix.Store(now.Unix())

	tag, err := s.evoPool.Exec(sweepCtx, reclaimStaleKanbanClaimsSQL, int64(ttl.Seconds()))
	if err != nil {
		s.kanbanSweep.errors.Add(1)
		s.kanbanSweep.lastErrorUnix.Store(now.Unix())
		s.logger.WithError(err).Warn("kanban sweeper: reclaim query failed")
		return 0
	}
	n := tag.RowsAffected()
	s.kanbanSweep.lastReclaimed.Store(n)
	if n > 0 {
		s.kanbanSweep.cardsReclaimed.Add(n)
		s.logger.WithField("cards_reclaimed", n).WithField("ttl", ttl.String()).
			Info("kanban sweeper: reclaimed stale card claims")
	} else {
		s.logger.WithField("ttl", ttl.String()).Debug("kanban sweeper: pass complete, nothing stale")
	}
	return n
}

// kanbanSweeperStatus serves GET /api/v1/kanban/sweeper/status.
func (s *APIServer) kanbanSweeperStatus(w http.ResponseWriter, r *http.Request) {
	st := &s.kanbanSweep
	unixOrNil := func(v int64) any {
		if v == 0 {
			return nil
		}
		return time.Unix(v, 0).UTC().Format(time.RFC3339)
	}
	writeEvoJSON(w, map[string]any{
		"enabled":             st.enabled.Load(),
		"runs":                st.runs.Load(),
		"errors":              st.errors.Load(),
		"cards_reclaimed":     st.cardsReclaimed.Load(),
		"last_run_at":         unixOrNil(st.lastRunUnix.Load()),
		"last_error_at":       unixOrNil(st.lastErrorUnix.Load()),
		"last_run_reclaimed":  st.lastReclaimed.Load(),
		"claim_ttl_seconds":   st.ttlSeconds.Load(),
		"sweep_interval_secs": st.intervalSecs.Load(),
	})
}

package main

// Per-caller rate limiting for the deploy-agent's mutating endpoints
// (/restart, /restart-all, /scale, /provision). These serialize on the
// compose mutex, so a token-holding client that floods them starves
// legitimate deploys. The throttle sits BEFORE any validation or mutex
// acquisition: over-limit requests cost one map lookup and a 429.
//
// Tunables:
//
//	DEPLOY_AGENT_MUTATE_RPM   sustained mutating requests per minute per
//	                          caller (default 30)
//	DEPLOY_AGENT_MUTATE_BURST bucket size (default 5)

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type rateLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	rate      float64 // tokens per second
	burst     float64
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = 0.5
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    ratePerSec,
		burst:   burst,
		now:     time.Now,
	}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastSweep) > 10*time.Minute {
		idle := time.Duration(l.burst/l.rate)*time.Second + 10*time.Minute
		for k, b := range l.buckets {
			if now.Sub(b.last) > idle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// newMutateLimiter builds the limiter from env tunables.
func newMutateLimiter() *rateLimiter {
	rpm := envFloatOr("DEPLOY_AGENT_MUTATE_RPM", 30)
	burst := envFloatOr("DEPLOY_AGENT_MUTATE_BURST", 5)
	return newRateLimiter(rpm/60.0, burst)
}

func envFloatOr(key string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

// limiterKey identifies the caller: first X-Forwarded-For hop (Caddy fronts
// the agent) or the socket peer.
func limiterKey(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// rateLimited wraps a mutating handler with the per-caller throttle.
func (s *server) rateLimited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.mutateLimiter.allow(limiterKey(r)) {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "rate limit exceeded; slow down", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}

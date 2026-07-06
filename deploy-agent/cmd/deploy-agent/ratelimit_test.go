package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterBurstThen429(t *testing.T) {
	l := newRateLimiter(0.001, 2) // effectively no refill within the test
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }

	if !l.allow("caller") || !l.allow("caller") {
		t.Fatal("burst should admit the first two requests")
	}
	if l.allow("caller") {
		t.Fatal("third request should be denied")
	}
	if !l.allow("other") {
		t.Fatal("independent caller should have its own bucket")
	}
}

func TestRateLimiterRefill(t *testing.T) {
	l := newRateLimiter(1, 1)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }

	if !l.allow("c") {
		t.Fatal("first request should pass")
	}
	if l.allow("c") {
		t.Fatal("second immediate request should be denied")
	}
	now = now.Add(1100 * time.Millisecond)
	if !l.allow("c") {
		t.Fatal("request after refill window should pass")
	}
}

// TestRateLimitedEndpoint429 exercises the wrapped handler path: over-limit
// requests get 429 BEFORE the handler (and thus before validation, the
// compose mutex, or any docker interaction).
func TestRateLimitedEndpoint429(t *testing.T) {
	s := &server{mutateLimiter: newRateLimiter(0.001, 1)}
	calls := 0
	h := s.rateLimited(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/restart?service=x&tag=y", nil)
	req.RemoteAddr = "10.0.0.5:1111"

	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 should carry Retry-After")
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times, want 1", calls)
	}
}

func TestLimiterKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/scale", nil)
	r.RemoteAddr = "192.168.0.7:4242"
	if got := limiterKey(r); got != "192.168.0.7" {
		t.Fatalf("limiterKey = %q, want socket host", got)
	}
	r.Header.Set("X-Forwarded-For", "10.9.8.7, 172.16.0.1")
	if got := limiterKey(r); got != "10.9.8.7" {
		t.Fatalf("limiterKey = %q, want first XFF hop", got)
	}
}

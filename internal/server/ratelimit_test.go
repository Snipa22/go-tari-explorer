package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestLimiter builds an ipRateLimiter with a small, fast, deterministic budget for
// tests: 1 token/sec sustained, burst of 2 - small enough that a handful of requests
// in a tight loop reliably crosses the limit without a slow test.
func newTestLimiter() *ipRateLimiter {
	return &ipRateLimiter{
		rps:     1,
		burst:   2,
		buckets: make(map[string]*rateLimitEntry),
	}
}

func testHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}

// TestRateLimitMiddleware_WithinBurst_Succeeds proves requests within the configured
// burst succeed (200, not 429).
func TestRateLimitMiddleware_WithinBurst_Succeeds(t *testing.T) {
	l := newTestLimiter()
	ts := httptest.NewServer(l.rateLimitMiddleware(testHandler()))
	defer ts.Close()

	for i := 0; i < 2; i++ { // burst == 2
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i, resp.StatusCode, http.StatusOK)
		}
	}
}

// TestRateLimitMiddleware_ExceedingLimit_Returns429NotError proves a request beyond
// the burst/rate gets a real 429, not a 500 or any other error status.
func TestRateLimitMiddleware_ExceedingLimit_Returns429NotError(t *testing.T) {
	l := newTestLimiter()
	ts := httptest.NewServer(l.rateLimitMiddleware(testHandler()))
	defer ts.Close()

	// Exhaust the burst.
	for i := 0; i < 2; i++ {
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatalf("burst request %d: %v", i, err)
		}
		resp.Body.Close()
	}

	// The next request from the same client should be rejected with 429.
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("over-limit request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-limit request: status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || ct[:10] != "text/plain" {
		t.Errorf("over-limit response Content-Type = %q, want text/plain prefix", ct)
	}
}

// TestRateLimitMiddleware_RefillsAfterWindow proves a client that has been
// rate-limited succeeds again once its token bucket refills.
func TestRateLimitMiddleware_RefillsAfterWindow(t *testing.T) {
	l := newTestLimiter()
	ts := httptest.NewServer(l.rateLimitMiddleware(testHandler()))
	defer ts.Close()

	for i := 0; i < 2; i++ {
		resp, err := http.Get(ts.URL)
		if err != nil {
			t.Fatalf("burst request %d: %v", i, err)
		}
		resp.Body.Close()
	}
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("over-limit request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 before refill, got %d", resp.StatusCode)
	}

	// rps == 1 token/sec: wait long enough for at least one token to refill.
	time.Sleep(1200 * time.Millisecond)

	resp, err = http.Get(ts.URL)
	if err != nil {
		t.Fatalf("post-refill request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-refill request: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestRateLimitMiddleware_IndependentPerIP proves one IP's limit does not affect a
// different IP: a single global limiter would fail this (one abusive IP could starve
// everyone else), which is exactly the shape this middleware must avoid.
func TestRateLimitMiddleware_IndependentPerIP(t *testing.T) {
	l := newTestLimiter()

	rec := httptest.NewRecorder()
	handler := l.rateLimitMiddleware(testHandler())

	newReqFromIP := func(ip string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = ip + ":12345"
		return req
	}

	// Exhaust IP A's burst.
	for i := 0; i < 2; i++ {
		rec = httptest.NewRecorder()
		handler(rec, newReqFromIP("10.0.0.1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("IP A burst request %d: status = %d, want 200", i, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	handler(rec, newReqFromIP("10.0.0.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("IP A over-limit request: status = %d, want 429", rec.Code)
	}

	// IP B, never seen before, must still succeed - its budget is independent of A's.
	rec = httptest.NewRecorder()
	handler(rec, newReqFromIP("10.0.0.2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("IP B request: status = %d, want 200 (must not be affected by IP A's limit)", rec.Code)
	}
}

func TestClientIP_SplitsHostFromPort(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	if got := clientIP(req); got != "203.0.113.7" {
		t.Errorf("clientIP = %q, want %q", got, "203.0.113.7")
	}
}

func TestClientIP_FallsBackToRawOnParseFailure(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "not-a-host-port"
	if got := clientIP(req); got != "not-a-host-port" {
		t.Errorf("clientIP = %q, want raw fallback %q", got, "not-a-host-port")
	}
}

func TestResolveClientIP_UsesXForwardedForSingleIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:54321" // proxy's own address - must not win.
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := resolveClientIP(req); got != "203.0.113.7" {
		t.Errorf("resolveClientIP = %q, want %q", got, "203.0.113.7")
	}
}

func TestResolveClientIP_UsesLeftmostOfXForwardedForChain(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:54321"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.2, 10.0.0.3")
	if got := resolveClientIP(req); got != "203.0.113.7" {
		t.Errorf("resolveClientIP = %q, want leftmost entry %q", got, "203.0.113.7")
	}
}

func TestResolveClientIP_FallsBackToXRealIPWhenXFFAbsent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:54321"
	req.Header.Set("X-Real-IP", "203.0.113.9")
	if got := resolveClientIP(req); got != "203.0.113.9" {
		t.Errorf("resolveClientIP = %q, want %q", got, "203.0.113.9")
	}
}

func TestResolveClientIP_FallsBackToRemoteAddrWhenNoHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:54321"
	if got := resolveClientIP(req); got != "203.0.113.7" {
		t.Errorf("resolveClientIP = %q, want %q (regression: today's r.RemoteAddr-based behavior)", got, "203.0.113.7")
	}
}

// TestRateLimitMiddleware_BehindReverseProxy_IndependentPerXFF proves the actual
// production bug fix: two requests sharing the SAME r.RemoteAddr (as they would behind
// Caddy, where RemoteAddr is always the proxy's own address) but with DIFFERENT
// X-Forwarded-For values are rate-limited independently, keyed on the real client IP
// rather than the proxy's address. Before this fix, both would collapse into one
// bucket and one client's traffic could exhaust the other's budget.
func TestRateLimitMiddleware_BehindReverseProxy_IndependentPerXFF(t *testing.T) {
	l := newTestLimiter()
	handler := l.rateLimitMiddleware(testHandler())

	newProxiedReq := func(xff string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:54321" // same proxy address for every request.
		req.Header.Set("X-Forwarded-For", xff)
		return req
	}

	// Exhaust client A's burst.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler(rec, newProxiedReq("203.0.113.1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("client A burst request %d: status = %d, want 200", i, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	handler(rec, newProxiedReq("203.0.113.1"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client A over-limit request: status = %d, want 429", rec.Code)
	}

	// Client B shares RemoteAddr with A (same proxy) but has a different
	// X-Forwarded-For - its budget must be independent of A's.
	rec = httptest.NewRecorder()
	handler(rec, newProxiedReq("203.0.113.2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("client B request: status = %d, want 200 (must not be affected by client A's limit despite sharing RemoteAddr)", rec.Code)
	}
}

func TestIPRateLimiter_EvictIdle(t *testing.T) {
	l := newTestLimiter()
	l.allow("1.2.3.4")
	if len(l.buckets) != 1 {
		t.Fatalf("expected 1 bucket after allow, got %d", len(l.buckets))
	}

	// Not idle yet - eviction at "now" (same instant) shouldn't drop it.
	l.evictIdle(time.Now())
	if len(l.buckets) != 1 {
		t.Fatalf("expected bucket to survive a non-idle sweep, got %d buckets", len(l.buckets))
	}

	// Simulate the bucket having gone idle well past the eviction threshold.
	l.evictIdle(time.Now().Add(rateLimitIdleEvict + time.Minute))
	if len(l.buckets) != 0 {
		t.Fatalf("expected idle bucket to be evicted, got %d buckets", len(l.buckets))
	}
}

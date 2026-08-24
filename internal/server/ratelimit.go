// IP-based rate limiting for the two routes that trigger a live GRPC call out to the
// operator's own configured Tari base node per request: /search and /tx-state (see
// internal/txsearch and internal/nodeclient - grep confirms these are the only two
// routes in internal/server that reach either package). Every other route
// (blocks-list, block detail, pool-stats, all /analysis/* pages and their .png
// companions) is a Postgres-only read and doesn't have this exposure, so this
// middleware is deliberately wired into just these two routes in Handler(), not the
// whole mux.
//
// Approach: golang.org/x/time/rate's token-bucket Limiter, one per client IP, kept in
// an in-memory map guarded by a mutex. golang.org/x/time/rate is the standard, tiny,
// well-maintained token-bucket implementation for exactly this shape of problem - no
// need to hand-roll one. A single global limiter was deliberately rejected: that would
// let one abusive IP's traffic exhaust the shared budget and rate-limit every other
// visitor too, which defeats the point on a public site.
package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Defaults for the per-IP token bucket applied to /search and /tx-state: 1 request/sec
// sustained, with a burst of 5 (so a user can legitimately fire off a handful of quick
// lookups - e.g. retrying a search after fixing a typo - without immediately hitting
// the limit) before being throttled back down to the sustained rate. These two routes
// are the only ones that reach out to the operator's own base node per request, so the
// limit is sized around "a real human occasionally searching", not bulk/API traffic.
const (
	defaultRateLimitPerSecond = 1
	defaultRateLimitBurst     = 5
)

// rateLimitIdleEvict is how long an IP's bucket may go unused before it's evicted from
// the in-memory map on the next sweep - bounds this middleware's memory growth on a
// public-facing site that will see many distinct, mostly one-off, source IPs (rather
// than growing the map forever, one entry per IP ever seen).
const rateLimitIdleEvict = 10 * time.Minute

// rateLimitSweepInterval is how often the background eviction sweep runs.
const rateLimitSweepInterval = 5 * time.Minute

// ipRateLimiter is a per-IP token-bucket rate limiter with bounded memory growth via
// periodic eviction of idle entries. Safe for concurrent use.
type ipRateLimiter struct {
	rps   rate.Limit
	burst int

	mu      sync.Mutex
	buckets map[string]*rateLimitEntry
}

// rateLimitEntry pairs a rate.Limiter with the last time it was touched, so the
// eviction sweep can tell an idle IP (safe to drop) from an active one.
type rateLimitEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newIPRateLimiter constructs an ipRateLimiter and starts its background eviction
// sweep goroutine. The sweep goroutine runs for the lifetime of the process (matching
// this server's other long-lived background work, e.g. its HTTP listener itself) -
// there's no explicit shutdown hook because the whole process exits together.
func newIPRateLimiter(rps rate.Limit, burst int) *ipRateLimiter {
	l := &ipRateLimiter{
		rps:     rps,
		burst:   burst,
		buckets: make(map[string]*rateLimitEntry),
	}
	go l.sweepLoop()
	return l
}

func (l *ipRateLimiter) sweepLoop() {
	ticker := time.NewTicker(rateLimitSweepInterval)
	defer ticker.Stop()
	for range ticker.C {
		l.evictIdle(time.Now())
	}
}

// evictIdle drops any bucket not touched within rateLimitIdleEvict of now - exported
// as its own method (rather than inlined into sweepLoop) so tests can drive eviction
// deterministically without a real ticker.
func (l *ipRateLimiter) evictIdle(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, entry := range l.buckets {
		if now.Sub(entry.lastSeen) > rateLimitIdleEvict {
			delete(l.buckets, ip)
		}
	}
}

// allow reports whether a request from ip is within its rate limit, creating a fresh
// full bucket for any IP not seen before (or evicted since).
func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	entry, ok := l.buckets[ip]
	if !ok {
		entry = &rateLimitEntry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[ip] = entry
	}
	entry.lastSeen = time.Now()
	limiter := entry.limiter
	l.mu.Unlock()

	return limiter.Allow()
}

// clientIP extracts the host portion of r.RemoteAddr (net/http always sets this to
// "ip:port"), falling back to the raw RemoteAddr string if it isn't in that shape
// (defensive only - net/http's server guarantees the "ip:port" shape in practice) so a
// parse hiccup degrades to "one bucket per weird value" rather than panicking.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// resolveClientIP determines the real client IP to key the rate limiter on, for use
// behind a reverse proxy (this server is deployed behind Caddy - see the package doc
// comment). r.RemoteAddr alone is useless in that topology: it's always Caddy's own
// address, so every request would land in the same bucket and per-IP rate limiting
// would be silently defeated for the entire site.
//
// TRUST BOUNDARY: this function trusts the X-Forwarded-For and X-Real-IP headers
// unconditionally, without validating that the immediate peer is a known/allowlisted
// proxy. That is safe *only* because this server is never directly reachable from the
// public internet - it only ever receives connections from the trusted Caddy reverse
// proxy in front of it, which sets these headers itself. If that topology ever changes
// (e.g. this server becomes directly internet-reachable, or sits behind additional
// untrusted hops), these headers become attacker-controllable and must be validated
// against a trusted-proxy allowlist (e.g. only trust them when r.RemoteAddr is a known
// proxy IP) rather than trusted unconditionally as done here. That allowlist logic is
// intentionally not implemented yet - do not assume it exists.
//
// Precedence:
//  1. X-Forwarded-For: may be a comma-separated chain ("client, proxy1, proxy2") when
//     multiple proxies are involved - the leftmost entry is the original client, so
//     that's the one extracted (after trimming surrounding whitespace).
//  2. X-Real-IP, if X-Forwarded-For is absent or empty.
//  3. clientIP(r)'s r.RemoteAddr-based extraction, as the final fallback.
func resolveClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first := xff
		if i := strings.IndexByte(xff, ','); i != -1 {
			first = xff[:i]
		}
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}

	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}

	return clientIP(r)
}

// rateLimitMiddleware wraps next with per-IP rate limiting, responding 429 with a
// short plain-text body (never a 500) once an IP exceeds its token bucket.
func (l *ipRateLimiter) rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(resolveClientIP(r)) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limit exceeded - please slow down and try again shortly\n"))
			return
		}
		next(w, r)
	}
}

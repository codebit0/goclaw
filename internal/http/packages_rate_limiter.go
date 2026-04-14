package http

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// perKeyRateLimiter is a minimal per-key token-bucket limiter used to cap
// external GitHub API usage initiated through /v1/packages/github-releases.
// Key is userID (header X-GoClaw-User-Id) or RemoteAddr when anonymous.
type perKeyRateLimiter struct {
	limiters sync.Map // key → *perKeyEntry
	rps      rate.Limit
	burst    int
	cleanup  sync.Once
}

type perKeyEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newPerKeyRateLimiter: rpm is requests per minute, burst is max burst size.
// rpm <= 0 disables (always allows).
func newPerKeyRateLimiter(rpm, burst int) *perKeyRateLimiter {
	if burst <= 0 {
		burst = 5
	}
	r := rate.Limit(0)
	if rpm > 0 {
		r = rate.Limit(float64(rpm) / 60.0)
	}
	return &perKeyRateLimiter{rps: r, burst: burst}
}

// Allow reports whether the request is within budget.
func (rl *perKeyRateLimiter) Allow(key string) bool {
	if rl.rps == 0 {
		return true // disabled
	}
	rl.cleanup.Do(func() { go rl.cleanupLoop() })
	v, _ := rl.limiters.LoadOrStore(key, &perKeyEntry{
		limiter:  rate.NewLimiter(rl.rps, rl.burst),
		lastSeen: time.Now(),
	})
	entry := v.(*perKeyEntry)
	if !entry.limiter.Allow() {
		return false
	}
	entry.lastSeen = time.Now()
	return true
}

func (rl *perKeyRateLimiter) cleanupLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-10 * time.Minute)
		rl.limiters.Range(func(k, v any) bool {
			if v.(*perKeyEntry).lastSeen.Before(cutoff) {
				rl.limiters.Delete(k)
			}
			return true
		})
	}
}

// githubReleasesLimiter caps calls to the picker endpoint to protect the
// shared upstream GitHub API quota. 30 req/min/user with burst 10 leaves
// plenty of headroom for UX while preventing quota exhaustion.
var githubReleasesLimiter = newPerKeyRateLimiter(30, 10)

// rateLimitKeyFromRequest returns the user ID if present, else the remote IP.
func rateLimitKeyFromRequest(r *http.Request) string {
	if uid := extractUserID(r); uid != "" {
		return "uid:" + uid
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

// enforceGitHubReleasesLimit returns true if the request is allowed; false (after
// writing 429) if throttled.
func enforceGitHubReleasesLimit(w http.ResponseWriter, r *http.Request) bool {
	key := rateLimitKeyFromRequest(r)
	if githubReleasesLimiter.Allow(key) {
		return true
	}
	slog.Warn("security.rate_limited", "endpoint", "/v1/packages/github-releases", "key", key)
	w.Header().Set("Retry-After", "60")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{
		"error": "rate limit exceeded; try again in 60 seconds",
	})
	return false
}

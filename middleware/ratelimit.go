package middleware

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dvislobokov/veyro"
)

// RateLimitOptions configures RateLimit.
type RateLimitOptions struct {
	// Rate is the sustained number of requests per second per key.
	Rate float64
	// Burst is the bucket size: how many requests may arrive at once.
	Burst int
	// Key groups requests; default is the client IP (see RealIP).
	Key func(*http.Request) string
	// Skip exempts requests (health checks, trusted callers).
	Skip func(*http.Request) bool
}

// RateLimit applies a token bucket per key, in memory. Requests over the
// limit get a 429 problem with Retry-After. For multi-instance deployments
// put the limit in the proxy or a shared store instead.
func RateLimit(o RateLimitOptions) veyro.Middleware {
	if o.Rate <= 0 {
		panic("middleware: RateLimit needs a positive Rate")
	}
	if o.Burst <= 0 {
		o.Burst = int(o.Rate)
		if o.Burst < 1 {
			o.Burst = 1
		}
	}
	if o.Key == nil {
		o.Key = ClientIP
	}
	l := &limiter{rate: o.Rate, burst: float64(o.Burst), buckets: map[string]*bucket{}}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o.Skip != nil && o.Skip(r) {
				next.ServeHTTP(w, r)
				return
			}
			if wait := l.take(o.Key(r), time.Now()); wait > 0 {
				secs := int(wait.Seconds() + 0.999)
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				writeProblem(w, r, veyro.NewProblem(http.StatusTooManyRequests,
					"Rate limit exceeded; retry in "+strconv.Itoa(secs)+"s."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the client IP from RemoteAddr (as rewritten by RealIP).
func ClientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu          sync.Mutex
	rate, burst float64
	buckets     map[string]*bucket
	lastPrune   time.Time
}

// take consumes a token for key and returns 0, or the time until one is
// available.
func (l *limiter) take(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastPrune) > time.Minute {
		l.prune(now)
	}
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// prune drops buckets that have refilled completely; they are
// indistinguishable from new ones.
func (l *limiter) prune(now time.Time) {
	l.lastPrune = now
	for k, b := range l.buckets {
		if now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

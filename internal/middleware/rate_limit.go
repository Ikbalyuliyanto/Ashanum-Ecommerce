package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimit allows at most limit requests per client IP in each fixed
// window, answering onLimit (with Retry-After) beyond that.
//
// State lives in process memory: fine for the single-binary deployment.
// Expired entries are swept lazily at most once per window, so there is no
// background goroutine and memory is bounded by the IPs seen in one window.
func RateLimit(limit int, window time.Duration, onLimit http.Handler) func(http.Handler) http.Handler {
	l := &fixedWindow{limit: limit, window: window, hits: make(map[string]*windowCount)}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retry := l.allow(ClientIP(r), time.Now()); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				onLimit.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type windowCount struct {
	n     int
	reset time.Time
}

type fixedWindow struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	hits      map[string]*windowCount
	lastSweep time.Time
}

func (l *fixedWindow) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) > l.window {
		for k, c := range l.hits {
			if !now.Before(c.reset) {
				delete(l.hits, k)
			}
		}
		l.lastSweep = now
	}

	c := l.hits[key]
	if c == nil || !now.Before(c.reset) {
		l.hits[key] = &windowCount{n: 1, reset: now.Add(l.window)}
		return true, 0
	}
	if c.n >= l.limit {
		return false, c.reset.Sub(now)
	}
	c.n++
	return true, 0
}

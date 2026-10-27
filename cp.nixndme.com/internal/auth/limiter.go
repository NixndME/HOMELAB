package auth

import (
	"sync"
	"time"
)

// Lockout counts failures per key (client IP) and locks a key out after too many.
// State is in memory: a restart clears it, which is acceptable for a single-user app.
type Lockout struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	lockFor  time.Duration
	failures map[string][]time.Time
	locked   map[string]time.Time
	now      func() time.Time
}

// NewLockout locks a key for lockFor after max failures within window.
func NewLockout(max int, window, lockFor time.Duration) *Lockout {
	return &Lockout{max: max, window: window, lockFor: lockFor,
		failures: map[string][]time.Time{}, locked: map[string]time.Time{}, now: time.Now}
}

// SetClock overrides the time source (tests only).
func (l *Lockout) SetClock(now func() time.Time) { l.now = now }

// Locked reports whether key is locked and for how much longer.
func (l *Lockout) Locked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	until, ok := l.locked[key]
	if !ok {
		return false, 0
	}
	left := until.Sub(l.now())
	if left <= 0 {
		delete(l.locked, key)
		return false, 0
	}
	return true, left
}

// Fail records a failure and reports whether key is now locked.
func (l *Lockout) Fail(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var recent []time.Time
	for _, t := range l.failures[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	if len(recent) >= l.max {
		l.locked[key] = now.Add(l.lockFor)
		delete(l.failures, key)
		return true
	}
	l.failures[key] = recent
	l.prune(now)
	return false
}

// Succeed clears a key's failure history.
func (l *Lockout) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// prune drops stale entries so the maps cannot grow without bound. Caller holds mu.
func (l *Lockout) prune(now time.Time) {
	if len(l.failures) < 10_000 {
		return
	}
	for k, ts := range l.failures {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= l.window {
			delete(l.failures, k)
		}
	}
}

// RateLimiter is a per-key token bucket for public endpoints.
type RateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows burst requests at once and perMinute sustained per key.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{rate: float64(perMinute) / 60, burst: float64(burst),
		buckets: map[string]*bucket{}, now: time.Now}
}

// Allow takes one token for key.
func (r *RateLimiter) Allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.buckets[key]
	if !ok {
		if len(r.buckets) >= 50_000 {
			r.buckets = map[string]*bucket{} // crude bound; a flood just resets the table
		}
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

package auth

import (
	"sync"
	"time"
)

const (
	limiterFreeFailures = 3
	limiterMaxShift     = 9 // 1s<<9 is past the cap, and a capped shift never overflows
	limiterMaxBlock     = 5 * time.Minute
	limiterPruneAfter   = time.Hour
	limiterPruneAbove   = 1024
)

// Limiter blocks a login key after repeated failures, with exponential
// backoff. It is in memory on purpose: a restart forgets it.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	now     func() time.Time
}

type limiterEntry struct {
	failures     int
	blockedUntil time.Time
	lastActivity time.Time
}

// NewLimiter returns a Limiter that reads time from now.
func NewLimiter(now func() time.Time) *Limiter {
	return &Limiter{entries: map[string]*limiterEntry{}, now: now}
}

// Allow reports whether an attempt for key may proceed, or how long until one may.
func (l *Limiter) Allow(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok {
		return 0, true
	}

	now := l.now()
	if now.Before(e.blockedUntil) {
		return e.blockedUntil.Sub(now), false
	}

	return 0, true
}

// Failure records a failed attempt. From the third failure on, the key is
// blocked for 2^(failures-3) seconds, up to five minutes.
func (l *Limiter) Failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.prune(now)

	e, ok := l.entries[key]
	if !ok {
		e = &limiterEntry{}
		l.entries[key] = e
	}

	e.failures++
	e.lastActivity = now

	if e.failures >= limiterFreeFailures {
		shift := min(e.failures-limiterFreeFailures, limiterMaxShift)
		e.blockedUntil = now.Add(min(time.Second<<shift, limiterMaxBlock))
	}
}

// Reset forgets key after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.entries, key)
}

// prune drops idle entries once the map is large, which bounds memory under
// many failed logins with varied names. The caller holds l.mu.
func (l *Limiter) prune(now time.Time) {
	if len(l.entries) <= limiterPruneAbove {
		return
	}

	for k, e := range l.entries {
		if now.Sub(e.lastActivity) > limiterPruneAfter && now.After(e.blockedUntil) {
			delete(l.entries, k)
		}
	}
}

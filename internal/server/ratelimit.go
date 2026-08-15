package server

import (
	"sync"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

// limiter is a token-bucket rate limiter keyed by an arbitrary string (an API
// token name for uploads, a client IP for failed authentication).
//
// It is deliberately in-memory and per-process: GoDrop is a single-instance
// service, and a shared limiter would mean introducing Redis, which the whole
// design exists to avoid.
type limiter struct {
	rate config.Rate
	now  func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate *config.Rate, now func() time.Time) *limiter {
	if rate == nil {
		return nil
	}
	return &limiter{
		rate:    *rate,
		now:     now,
		buckets: make(map[string]*bucket),
		swept:   now(),
	}
}

// allow consumes one unit for key and reports whether it was permitted. When it
// is not, the second return value says how long to wait before retrying.
func (l *limiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()
	refillPerSecond := float64(l.rate.N) / l.rate.Period.Seconds()
	capacity := float64(l.rate.N)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(capacity, b.tokens+elapsed*refillPerSecond)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / refillPerSecond * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// sweepLocked drops buckets that have fully refilled, so a stream of unique
// keys (spoofed IPs, for instance) cannot grow the map without bound.
func (l *limiter) sweepLocked(now time.Time) {
	if now.Sub(l.swept) < l.rate.Period {
		return
	}
	l.swept = now
	for key, b := range l.buckets {
		if now.Sub(b.last) > l.rate.Period {
			delete(l.buckets, key)
		}
	}
}

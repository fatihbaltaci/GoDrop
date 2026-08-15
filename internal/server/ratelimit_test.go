package server

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fatihbaltaci/GoDrop/internal/config"
)

var errFake = errors.New("something went wrong")

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

func TestNilLimiterAllowsEverything(t *testing.T) {
	t.Parallel()
	var l *limiter
	for range 100 {
		if ok, _ := l.allow("key"); !ok {
			t.Fatal("a disabled limiter must allow every request")
		}
	}
	if newLimiter(nil, time.Now) != nil {
		t.Error("newLimiter(nil) should return a disabled limiter")
	}
}

func TestLimiterConsumesAndRefills(t *testing.T) {
	t.Parallel()
	now := time.Now()
	clock := func() time.Time { return now }
	l := newLimiter(&config.Rate{N: 2, Period: time.Minute}, clock)

	for i := range 2 {
		if ok, _ := l.allow("token"); !ok {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	ok, wait := l.allow("token")
	if ok {
		t.Fatal("the third request should be rejected")
	}
	if wait < time.Second {
		t.Errorf("wait = %v, want at least a second so Retry-After is meaningful", wait)
	}

	// Half the window returns one slot at the configured rate.
	now = now.Add(30 * time.Second)
	if ok, _ := l.allow("token"); !ok {
		t.Error("a slot should have refilled after half the period")
	}
}

func TestLimiterIsPerKey(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newLimiter(&config.Rate{N: 1, Period: time.Minute}, func() time.Time { return now })

	if ok, _ := l.allow("first"); !ok {
		t.Fatal("first key should be allowed")
	}
	if ok, _ := l.allow("second"); !ok {
		t.Fatal("a different key has its own allowance")
	}
	if ok, _ := l.allow("first"); ok {
		t.Fatal("the first key is now exhausted")
	}
}

func TestLimiterDropsIdleBuckets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newLimiter(&config.Rate{N: 1, Period: time.Second}, func() time.Time { return now })

	// A flood of one-off keys, the shape of spoofed source addresses.
	for i := range 100 {
		l.allow(string(rune('a' + i%26)))
		now = now.Add(10 * time.Millisecond)
	}
	before := len(l.buckets)
	now = now.Add(time.Hour)
	l.allow("trigger-sweep")
	if len(l.buckets) >= before {
		t.Errorf("buckets = %d, want the idle ones swept (was %d)", len(l.buckets), before)
	}
}

func TestLimiterNeverExceedsCapacity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	l := newLimiter(&config.Rate{N: 3, Period: time.Minute}, func() time.Time { return now })

	if ok, _ := l.allow("k"); !ok {
		t.Fatal("first request should be allowed")
	}
	// A long idle period must not accumulate more than the burst size.
	now = now.Add(24 * time.Hour)
	allowed := 0
	for range 10 {
		if ok, _ := l.allow("k"); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("allowed %d after a long idle period, want the burst size of 3", allowed)
	}
}

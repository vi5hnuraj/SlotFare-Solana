package proxy

import (
	"sync"
	"time"
)

// Rate limiting is one fixed minute window per bucket, counted from the
// first request that opens the window. Buckets are keyed by the key hash
// (never plaintext) for the per-key layer and by the registry model id for
// the per-model upstream cap. A rejected request does not open a window.
//
// Buckets whose window has closed are dead weight - the map only ever grew
// - so they are swept at most once a minute while requests keep arriving.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]bucket
	limits  map[string]int
	now     func() time.Time // injectable clock; tests advance it without sleeping
	sweepAt time.Time        // when the expired-bucket sweep next runs
}

type bucket struct {
	opened time.Time
	count  int
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{buckets: map[string]bucket{}, limits: map[string]int{}, now: now}
}

// setLimit registers one layer with its per-minute ceiling. A limit of zero
// disables the layer entirely (the defensive off switch).
func (rl *rateLimiter) setLimit(name string, limit int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.limits[name] = limit
}

// allow reports whether the bucket named key may serve another request in
// the current window and counts it when it may. It reports how long the
// caller should wait when the window is full.
func (rl *rateLimiter) allow(layer, key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	limit := rl.limits[layer]
	if limit <= 0 {
		return true, 0
	}
	now := rl.now()
	// Sweep at most once a minute: every bucket whose window closed is
	// dropped, so a key that stopped being used stops costing memory
	// instead of staying resident for the life of the process.
	if rl.sweepAt.IsZero() || !now.Before(rl.sweepAt) {
		for k, b := range rl.buckets {
			if !now.Before(b.opened.Add(time.Minute)) {
				delete(rl.buckets, k)
			}
		}
		rl.sweepAt = now.Add(time.Minute)
	}
	b, seen := rl.buckets[layer+"/"+key]
	if !seen || !now.Before(b.opened.Add(time.Minute)) {
		// The window rolled over (or the bucket is new): open a fresh one.
		rl.buckets[layer+"/"+key] = bucket{opened: now, count: 0}
	}
	b = rl.buckets[layer+"/"+key]
	if b.count >= limit {
		wait := time.Until(b.opened.Add(time.Minute))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	b.count++
	rl.buckets[layer+"/"+key] = b
	return true, 0
}

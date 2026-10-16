package notifier

import (
	"sync"
	"time"
)

// Deduper holds a TTL-bounded deduplication window for fired alerts.
//
// Per design.md §5 ("通知去抖"), a (task_id, rule) pair fires at most once
// every DedupTTL. ShouldFire returns true the first time it is called for a
// given key, then false until TTL elapses.
type Deduper struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]time.Time
}

// NewDeduper constructs a deduper with the given TTL. TTL must be > 0.
func NewDeduper(ttl time.Duration) *Deduper {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Deduper{ttl: ttl, m: make(map[string]time.Time)}
}

// ShouldFire returns true the first time it's called for `key` within the
// TTL window; false thereafter until the window expires.
//
// Passing `now` explicitly keeps the deduper testable without a clock real
// dependency.
func (d *Deduper) ShouldFire(key string, now time.Time) bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	last, ok := d.m[key]
	if ok && now.Sub(last) < d.ttl {
		return false
	}
	d.m[key] = now
	return true
}

// Reset clears the dedup window.
func (d *Deduper) Reset() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m = make(map[string]time.Time)
}

// Size returns the number of keys currently being tracked.
func (d *Deduper) Size() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.m)
}

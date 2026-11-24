package mcpserver

import (
	"sync"
	"time"
)

const (
	maxFailures  = 10
	failWindow   = time.Minute
	maxAddresses = 10_000
)

type window struct {
	start time.Time
	n     int
}

// limiter counts authentication failures per source address in fixed windows of a minute.
// It tracks at most capacity addresses. It is safe for concurrent use.
type limiter struct {
	now      func() time.Time
	capacity int

	mu sync.Mutex
	m  map[string]window
}

func newLimiter(now func() time.Time, capacity int) *limiter {
	return &limiter{now: now, capacity: capacity, m: map[string]window{}}
}

func (l *limiter) live(w window, now time.Time) bool { return now.Sub(w.start) < failWindow }

// fail records a failure of addr and says, in the same locked step, how many failures its
// window holds, whether addr is now over the limit and for how long it stays so.
func (l *limiter) fail(addr string) (n int, over bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.m[addr]
	switch {
	case ok && l.live(w, now):
		w.n++
	case !ok && len(l.m) >= l.capacity:
		l.makeRoom(now)
		fallthrough
	default:
		w = window{start: now, n: 1}
	}
	l.m[addr] = w
	if w.n <= maxFailures {
		return w.n, false, 0
	}
	return w.n, true, w.start.Add(failWindow).Sub(now)
}

// makeRoom drops the expired entries, or the oldest one when none has expired.
func (l *limiter) makeRoom(now time.Time) {
	var oldest string
	for a, w := range l.m {
		if !l.live(w, now) {
			delete(l.m, a)
		} else if oldest == "" || w.start.Before(l.m[oldest].start) {
			oldest = a
		}
	}
	if len(l.m) >= l.capacity {
		delete(l.m, oldest)
	}
}

// failures is for tests: the count of addr's current window.
func (l *limiter) failures(addr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w, ok := l.m[addr]; ok && l.live(w, l.now()) {
		return w.n
	}
	return 0
}

func (l *limiter) size() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.m) }

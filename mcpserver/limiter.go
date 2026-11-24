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

// blocked says whether addr has used up its failures, and for how long it stays blocked.
func (l *limiter) blocked(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.m[addr]
	if !ok || !l.live(w, now) || w.n < maxFailures {
		return false, 0
	}
	return true, w.start.Add(failWindow).Sub(now)
}

func (l *limiter) fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if w, ok := l.m[addr]; ok && l.live(w, now) {
		w.n++
		l.m[addr] = w
		return
	}
	if _, ok := l.m[addr]; !ok && len(l.m) >= l.capacity {
		l.makeRoom(now)
	}
	l.m[addr] = window{start: now, n: 1}
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

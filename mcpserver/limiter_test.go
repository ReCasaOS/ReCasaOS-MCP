package mcpserver

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func TestLimiterBlocksAfterTenFailuresForAMinute(t *testing.T) {
	c := newClock()
	l := newLimiter(c.now, 100)
	for i := 0; i < 10; i++ {
		if blocked, _ := l.blocked("a"); blocked {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail("a")
	}
	blocked, wait := l.blocked("a")
	if !blocked || wait <= 0 || wait > time.Minute {
		t.Fatalf("blocked %v wait %v", blocked, wait)
	}
	if blocked, _ := l.blocked("b"); blocked {
		t.Fatal("another address is blocked too")
	}
	c.add(time.Minute)
	if blocked, _ := l.blocked("a"); blocked {
		t.Fatal("still blocked once the window passed")
	}
}

func TestLimiterIsBounded(t *testing.T) {
	c := newClock()
	l := newLimiter(c.now, 3)
	for i := 0; i < 10; i++ {
		l.fail("old")
	}
	c.add(time.Second)
	l.fail("x")
	c.add(time.Second)
	l.fail("y")
	c.add(time.Second)
	l.fail("z") // full: the oldest, "old", goes
	if n := l.size(); n != 3 {
		t.Fatalf("%d addresses tracked, want 3", n)
	}
	if blocked, _ := l.blocked("old"); blocked {
		t.Fatal("the oldest entry was not evicted")
	}

	// expired entries go first, before any live one
	c.add(time.Minute - 1500*time.Millisecond) // x is now expired, y and z are not
	l.fail("w")
	if l.failures("x") != 0 || l.failures("y") != 1 || l.failures("z") != 1 || l.failures("w") != 1 {
		t.Fatalf("x %d y %d z %d w %d", l.failures("x"), l.failures("y"), l.failures("z"), l.failures("w"))
	}

	big := newLimiter(c.now, maxAddresses)
	for i := 0; i < maxAddresses+500; i++ {
		big.fail(strconv.Itoa(i))
	}
	if n := big.size(); n != maxAddresses {
		t.Fatalf("%d addresses tracked, want %d", n, maxAddresses)
	}
}

// Run with -race where cgo is available.
func TestLimiterIsSafeForConcurrentUse(t *testing.T) {
	l := newLimiter(time.Now, 50)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				a := strconv.Itoa((g*200 + i) % 70)
				l.fail(a)
				l.blocked(a)
			}
		}(g)
	}
	wg.Wait()
	if n := l.size(); n > 50 {
		t.Fatalf("%d addresses tracked, want at most 50", n)
	}
}

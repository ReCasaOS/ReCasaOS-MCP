package mcpserver

import (
	"strconv"
	"sync"
	"sync/atomic"
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

func TestLimiterIsOverAfterTenFailuresForAMinute(t *testing.T) {
	c := newClock()
	l := newLimiter(c.now, 100)
	for i := 0; i < 10; i++ {
		if over, _ := l.fail("a"); over {
			t.Fatalf("over at failure %d", i+1)
		}
	}
	over, wait := l.fail("a")
	if !over || wait <= 0 || wait > time.Minute {
		t.Fatalf("over %v wait %v", over, wait)
	}
	if over, _ := l.fail("b"); over {
		t.Fatal("another address is over too")
	}
	c.add(time.Minute)
	if over, _ := l.fail("a"); over || l.failures("a") != 1 {
		t.Fatal("still over once the window passed")
	}
}

// Run with -race where cgo is available.
func TestABurstOfBadTokensIsCountedOnce(t *testing.T) {
	l := newLimiter(newClock().now, 100)
	const n = 50
	var under, over atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if o, _ := l.fail("a"); o {
				over.Add(1)
			} else {
				under.Add(1)
			}
		}()
	}
	wg.Wait()
	if under.Load() != 10 || over.Load() != n-10 {
		t.Fatalf("%d under, %d over", under.Load(), over.Load())
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
	if l.failures("old") != 0 {
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
			}
		}(g)
	}
	wg.Wait()
	if n := l.size(); n > 50 {
		t.Fatalf("%d addresses tracked, want at most 50", n)
	}
}

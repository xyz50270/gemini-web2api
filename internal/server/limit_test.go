package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterSerialisesHolders(t *testing.T) {
	l := NewLimiter(1)

	var inFlight int32
	var maxSeen int32
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background()); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer l.Release()

			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxSeen)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxSeen, prev, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}()
	}

	wg.Wait()

	if maxSeen != 1 {
		t.Errorf("max concurrent holders = %d, want 1", maxSeen)
	}
}

func TestLimiterAllowsConfiguredConcurrency(t *testing.T) {
	l := NewLimiter(3)

	var inFlight int32
	var maxSeen int32
	var wg sync.WaitGroup

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Acquire(context.Background()); err != nil {
				return
			}
			defer l.Release()

			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxSeen)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxSeen, prev, cur) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}()
	}
	wg.Wait()

	if maxSeen > 3 {
		t.Errorf("max concurrent holders = %d, want at most 3", maxSeen)
	}
	if maxSeen < 2 {
		t.Errorf("max concurrent holders = %d, expected the limit to allow more than 1", maxSeen)
	}
}

func TestLimiterWaiterRespectsCancel(t *testing.T) {
	l := NewLimiter(1)

	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := l.Acquire(ctx)
	if err == nil {
		l.Release()
		t.Fatal("expected a context error while the only slot was taken")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waiter took %v, expected it to give up on ctx cancellation", elapsed)
	}

	l.Release()
	if _, waiting := l.Stats(); waiting != 0 {
		t.Errorf("waiting = %d after a cancelled waiter, want 0", waiting)
	}
}

func TestLimiterZeroValueFallsBackToOne(t *testing.T) {
	l := NewLimiter(0)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); err == nil {
		t.Error("a zero-size limiter should still serialise to 1")
	}
	l.Release()
}

func TestLimiterStats(t *testing.T) {
	l := NewLimiter(2)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	inflight, waiting := l.Stats()
	if inflight != 1 || waiting != 0 {
		t.Errorf("Stats = (%d,%d), want (1,0)", inflight, waiting)
	}
	l.Release()
	inflight, _ = l.Stats()
	if inflight != 0 {
		t.Errorf("inflight = %d after Release, want 0", inflight)
	}
}

func TestLimiterReleaseWithoutAcquireDoesNotPanic(t *testing.T) {
	l := NewLimiter(1)
	l.Release() // must be a no-op, not a panic
	if _, waiting := l.Stats(); waiting != 0 {
		t.Errorf("waiting = %d, want 0", waiting)
	}
}

package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterSerialisesHolders(t *testing.T) {
	l := NewLimiter(1, 0)

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
	l := NewLimiter(3, 0)

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
	l := NewLimiter(1, 0)

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
	l := NewLimiter(0, 0)
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
	l := NewLimiter(2, 0)
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
	l := NewLimiter(1, 0)
	l.Release() // must be a no-op, not a panic
	if _, waiting := l.Stats(); waiting != 0 {
		t.Errorf("waiting = %d, want 0", waiting)
	}
}

func TestLimiterQueueTimeout(t *testing.T) {
	// One slot, one second budget: the second acquire must give up rather than
	// waiting for the wedged holder.
	l := NewLimiter(1, 1)

	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()

	start := time.Now()
	err := l.Acquire(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("err = %v, want ErrQueueTimeout", err)
	}
	if elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("waited %v, want about 1s", elapsed)
	}
	if _, waiting := l.Stats(); waiting != 0 {
		t.Errorf("waiting = %d after timeout, want 0", waiting)
	}
}

func TestLimiterQueueTimeoutZeroWaitsForever(t *testing.T) {
	// queueTimeoutSec <= 0 disables the deadline; the acquire must still respect
	// its own context.
	l := NewLimiter(1, 0)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer l.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	err := l.Acquire(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrQueueTimeout) {
		t.Error("with the queue timeout disabled the error must come from ctx")
	}
}

func TestLimiterSlotFreedBeforeTimeoutSucceeds(t *testing.T) {
	l := NewLimiter(1, 5)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- l.Acquire(context.Background())
	}()

	time.Sleep(50 * time.Millisecond)
	l.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter should have acquired after Release, got %v", err)
		}
		l.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not acquire after Release")
	}
}

func TestWritePoolErrorQueueTimeout(t *testing.T) {
	rec := httptest.NewRecorder()
	ok := writePoolError(rec, ErrQueueTimeout, 240*time.Second)
	if !ok {
		t.Fatal("writePoolError should handle ErrQueueTimeout")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "240" {
		t.Errorf("Retry-After = %q, want 240", got)
	}
	if !strings.Contains(rec.Body.String(), "queue_timeout") {
		t.Errorf("body = %s, want code queue_timeout", rec.Body.String())
	}
}

func TestWritePoolErrorIgnoresOtherErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	if writePoolError(rec, errors.New("boom"), time.Second) {
		t.Error("writePoolError must not handle unrelated errors")
	}
}

func TestWritePoolErrorAllExhausted(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &ErrAllExhausted{Accounts: []ExhaustedAccount{{Index: 0, Label: "a"}}}
	if !writePoolError(rec, err, 0) {
		t.Fatal("writePoolError should handle ErrAllExhausted")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "all_accounts_exhausted") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

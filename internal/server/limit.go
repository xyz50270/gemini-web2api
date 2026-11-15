package server

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQueueTimeout is returned when a request waited for a slot longer than the
// configured budget. With one slot, a single wedged upstream call would
// otherwise park every later request until the client gives up.
var ErrQueueTimeout = errors.New("timed out waiting for an upstream slot")

// Limiter serialises upstream generation.
//
// Google's anti-abuse heuristics expect a web client to run one generation at a
// time per account; firing several concurrent StreamGenerate calls is a strong
// bot signal. A buffered channel of size n is the whole mechanism: the nth
// caller blocks until a slot frees up.
//
// The zero value is not usable; use NewLimiter.
type Limiter struct {
	sem chan struct{}

	// queueTimeout bounds the wait for a slot. Zero means wait forever.
	queueTimeout time.Duration

	mu       sync.Mutex
	waiting  int
	inflight int
}

// NewLimiter returns a limiter allowing at most n concurrent holders. n < 1 is
// treated as 1 so a misconfiguration cannot disable the guard entirely.
// queueTimeoutSec <= 0 disables the queue timeout.
func NewLimiter(n, queueTimeoutSec int) *Limiter {
	if n < 1 {
		n = 1
	}
	var timeout time.Duration
	if queueTimeoutSec > 0 {
		timeout = time.Duration(queueTimeoutSec) * time.Second
	}
	return &Limiter{sem: make(chan struct{}, n), queueTimeout: timeout}
}

// Acquire blocks until a slot is free, ctx is done, or the queue timeout fires.
// Returning a non-nil error means the caller must NOT call Release.
func (l *Limiter) Acquire(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// Fast path: an uncontended acquire should not touch the counters.
	select {
	case l.sem <- struct{}{}:
		l.mu.Lock()
		l.inflight++
		l.mu.Unlock()
		return nil
	default:
	}

	l.mu.Lock()
	l.waiting++
	l.mu.Unlock()

	// A wedged upstream call must not park every later request indefinitely, so
	// the wait gets its own deadline in addition to the caller's context.
	var timeoutCh <-chan time.Time
	if l.queueTimeout > 0 {
		timer := time.NewTimer(l.queueTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case l.sem <- struct{}{}:
		l.mu.Lock()
		l.waiting--
		l.inflight++
		l.mu.Unlock()
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		l.waiting--
		l.mu.Unlock()
		return ctx.Err()
	case <-timeoutCh:
		l.mu.Lock()
		l.waiting--
		l.mu.Unlock()
		return ErrQueueTimeout
	}
}

// Release frees a slot previously taken by Acquire.
func (l *Limiter) Release() {
	select {
	case <-l.sem:
		l.mu.Lock()
		if l.inflight > 0 {
			l.inflight--
		}
		l.mu.Unlock()
	default:
		// Releasing without holding a slot is a programming error; ignoring it
		// keeps a stray defer from turning into a panic under load.
	}
}

// Stats reports the current concurrency for logging and the status endpoint.
func (l *Limiter) Stats() (inflight, waiting int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight, l.waiting
}

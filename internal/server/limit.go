package server

import (
	"context"
	"sync"
)

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

	mu       sync.Mutex
	waiting  int
	inflight int
}

// NewLimiter returns a limiter allowing at most n concurrent holders. n < 1 is
// treated as 1 so a misconfiguration cannot disable the guard entirely.
func NewLimiter(n int) *Limiter {
	if n < 1 {
		n = 1
	}
	return &Limiter{sem: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free or ctx is done. Returning a non-nil error
// means the caller must NOT call Release.
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

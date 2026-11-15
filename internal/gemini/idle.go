package gemini

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleGuard cancels a request when no bytes arrive for a while.
//
// A total request deadline is the wrong tool for streamed generation: a model
// that keeps producing tokens for several minutes is healthy, yet a fixed
// http.Client.Timeout would slice it off mid-stream. What actually signals a
// dead upstream is silence, so the guard watches the gap between reads instead
// of the total elapsed time.
type idleGuard struct {
	d      time.Duration
	cancel func()

	mu    sync.Mutex
	timer *time.Timer
	fired bool
	done  bool
}

// newIdleGuard starts a timer that fires cancel after d of inactivity.
// d <= 0 disables the guard.
func newIdleGuard(d time.Duration, cancel func()) *idleGuard {
	g := &idleGuard{d: d, cancel: cancel}
	if d > 0 {
		g.timer = time.AfterFunc(d, g.onFire)
	}
	return g
}

func (g *idleGuard) onFire() {
	g.mu.Lock()
	fired := !g.done
	g.fired = fired
	g.mu.Unlock()
	if fired && g.cancel != nil {
		g.cancel()
	}
}

// touch resets the deadline. Called on every successful read.
func (g *idleGuard) touch() {
	if g.d == 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil && !g.done {
		g.timer.Reset(g.d)
	}
}

// stop ends the guard; safe to call more than once.
func (g *idleGuard) stop() {
	g.mu.Lock()
	g.done = true
	t := g.timer
	g.mu.Unlock()
	if t != nil {
		t.Stop()
	}
}

// exceeded reports whether the guard fired, i.e. the cancellation came from
// inactivity rather than from the caller or a real transport failure.
func (g *idleGuard) exceeded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

// guardedReader pokes the guard after every read that made progress.
type guardedReader struct {
	r io.Reader
	g *idleGuard
}

func (gr *guardedReader) Read(p []byte) (int, error) {
	n, err := gr.r.Read(p)
	if n > 0 {
		gr.g.touch()
	}
	return n, err
}

// attemptContext returns a context carrying the response-header deadline.
//
// The deadline only covers "time until upstream starts answering" — once the
// response headers arrive we cancel the deadline but keep the cancel func, so
// the idle guard can take over for the body. This is what separates "upstream is
// dead" from "upstream is still generating".
func (c *Client) attemptContext() (context.Context, context.CancelFunc) {
	secs := c.Cfg.RequestTimeoutSec
	if secs <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
}

// idleTimeout is how long the response body may stay silent before the attempt
// is aborted.
func (c *Client) idleTimeout() time.Duration {
	secs := c.Cfg.RequestTimeoutSec
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// classifyTransportError turns a transport failure into an UpstreamError,
// distinguishing "upstream went silent / never answered" from a generic
// transport problem so callers can log something actionable.
func (c *Client) classifyTransportError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		return &UpstreamError{
			Kind: "timeout",
			Msg:  "upstream did not respond in time",
		}
	}
	return &UpstreamError{Kind: "transport", Msg: err.Error()}
}

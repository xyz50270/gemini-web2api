package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowReader yields chunks with a configurable gap between them.
type slowReader struct {
	chunks []string
	gap    time.Duration
	i      int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.gap)
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

func TestIdleGuardFiresOnSilence(t *testing.T) {
	var cancelled atomic.Bool
	g := newIdleGuard(80*time.Millisecond, func() { cancelled.Store(true) })
	defer g.stop()

	time.Sleep(200 * time.Millisecond)
	if !cancelled.Load() {
		t.Fatal("guard should have fired after silence")
	}
	if !g.exceeded() {
		t.Error("exceeded() should report true")
	}
}

func TestIdleGuardTouchKeepsItAlive(t *testing.T) {
	var cancelled atomic.Bool
	g := newIdleGuard(120*time.Millisecond, func() { cancelled.Store(true) })
	defer g.stop()

	// Steady progress well past the idle budget: the guard must never fire.
	for i := 0; i < 8; i++ {
		time.Sleep(40 * time.Millisecond)
		g.touch()
	}
	if cancelled.Load() {
		t.Fatal("an actively reading stream must not be cancelled")
	}
	if g.exceeded() {
		t.Error("exceeded() should be false while data keeps flowing")
	}
}

// The whole point of this change: a generation that keeps producing tokens for
// longer than the timeout budget must still complete.
func TestGuardedReaderAllowsLongStream(t *testing.T) {
	src := &slowReader{chunks: []string{"a", "b", "c", "d", "e", "f"}, gap: 40 * time.Millisecond}
	var cancelled atomic.Bool
	g := newIdleGuard(100*time.Millisecond, func() { cancelled.Store(true) })
	defer g.stop()

	// Total elapsed (~240ms) far exceeds the 100ms idle budget, but no single
	// gap does.
	out, err := io.ReadAll(&guardedReader{r: src, g: g})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != "abcdef" {
		t.Errorf("out = %q", out)
	}
	if cancelled.Load() {
		t.Fatal("a steadily producing stream must not be cancelled")
	}
}

func TestGuardedReaderStopsOnStall(t *testing.T) {
	src := &slowReader{chunks: []string{"a", "b"}, gap: 200 * time.Millisecond}
	var cancelled atomic.Bool
	g := newIdleGuard(60*time.Millisecond, func() { cancelled.Store(true) })
	defer g.stop()

	buf := make([]byte, 8)
	// First read sleeps 200ms then returns data; the guard fires during that gap
	// and cancels, which a real transport would turn into a read error.
	_, _ = src.Read(buf)
	time.Sleep(120 * time.Millisecond)
	if !cancelled.Load() {
		t.Fatal("guard should fire when the gap exceeds the idle budget")
	}
	if !g.exceeded() {
		t.Error("exceeded() should be true after a stall")
	}
}

func TestIdleGuardDisabled(t *testing.T) {
	g := newIdleGuard(0, func() { t.Error("disabled guard must never cancel") })
	defer g.stop()
	time.Sleep(50 * time.Millisecond)
	g.touch()
}

func TestIdleGuardStopPreventsFire(t *testing.T) {
	var cancelled atomic.Bool
	g := newIdleGuard(60*time.Millisecond, func() { cancelled.Store(true) })
	g.stop()
	time.Sleep(150 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("stopped guard must not fire")
	}
}

func TestClassifyTransportErrorTimeout(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond)

	err := c.classifyTransportError(ctx, errors.New("context deadline exceeded"))
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("err type = %T", err)
	}
	if ue.Kind != "timeout" {
		t.Errorf("Kind = %q, want timeout", ue.Kind)
	}
	if !IsTimeoutError(err) {
		t.Error("IsTimeoutError should recognise it")
	}
}

func TestClassifyTransportErrorGeneric(t *testing.T) {
	c := &Client{}
	err := c.classifyTransportError(context.Background(), errors.New("connection reset"))
	ue, ok := err.(*UpstreamError)
	if !ok {
		t.Fatalf("err type = %T", err)
	}
	if ue.Kind != "transport" {
		t.Errorf("Kind = %q, want transport", ue.Kind)
	}
	if IsTimeoutError(err) {
		t.Error("a generic transport fault is not a timeout")
	}
}

func TestIsTimeoutErrorRejectsOther(t *testing.T) {
	if IsTimeoutError(nil) {
		t.Error("nil is not a timeout")
	}
	if IsTimeoutError(errors.New("x")) {
		t.Error("plain error is not a timeout")
	}
	if IsTimeoutError(&UpstreamError{Kind: "bard", Msg: "x"}) {
		t.Error("bard error is not a timeout")
	}
}

func TestStreamParserEmittedTracking(t *testing.T) {
	p := NewStreamParser()
	if p.Emitted() {
		t.Error("fresh parser should not report output")
	}

	line := metaFrameForIdle(t, "hello")
	if _, err := p.Feed(line); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if !p.Emitted() {
		t.Fatal("parser should record that text was delivered")
	}
	if p.EmittedLen() != len("hello") {
		t.Errorf("EmittedLen = %d, want %d", p.EmittedLen(), len("hello"))
	}
}

func metaFrameForIdle(t *testing.T, text string) string {
	t.Helper()

	part := []any{"rc_1", []string{text}, nil, nil, nil, nil, nil, nil, []int{1}, "en"}
	inner := make([]any, 26)
	inner[1] = []any{"c_1", "r_1"}
	inner[4] = []any{part}
	// Padding keeps the encoded line above ExtractTextsFromLine's 200-byte gate.
	for i := 5; i < 26; i++ {
		if inner[i] == nil {
			inner[i] = strings.Repeat("p", 12)
		}
	}

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	line, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	if err != nil {
		t.Fatalf("marshal line: %v", err)
	}
	if len(line) < 200 {
		t.Fatalf("fixture too short (%d bytes), ExtractTextsFromLine would ignore it", len(line))
	}
	return string(line) + "\n"
}

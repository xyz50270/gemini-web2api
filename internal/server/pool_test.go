package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/conv"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// stubRequester answers every request with a canned body/status so pool logic
// can be exercised without touching the network.
type stubRequester struct {
	status int
	body   string
	err    error
	calls  int
}

func (s *stubRequester) Do(req *http.Request) (*http.Response, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func poolConfig(threshold float64, users ...string) config.Config {
	cfg := config.Default()
	cfg.LogRequests = false
	cfg.QuotaThreshold = threshold
	cfg.AutoXSRF = false
	cfg.Accounts = make([]config.AccountConfig, 0, len(users))
	for _, u := range users {
		cfg.Accounts = append(cfg.Accounts, config.AccountConfig{
			AuthUser: u,
			Label:    "account-" + u,
		})
	}
	return cfg
}

func newTestPool(t *testing.T, cfg config.Config, req gemini.Requester) *Pool {
	t.Helper()
	p := NewPool(cfg, req, nil)
	t.Cleanup(p.Close)
	return p
}

func setQuota(t *testing.T, a *Account, fiveHour, weekly float64, reset time.Time) {
	t.Helper()
	q := &gemini.Quota{Tier: "TIER1", FetchedAt: time.Now()}
	if fiveHour >= 0 {
		q.FiveHour = &gemini.QuotaPool{PoolType: gemini.PoolFiveHourly, PercentageUsed: fiveHour}
	}
	if weekly >= 0 {
		w := &gemini.QuotaPool{PoolType: gemini.PoolWeekly, PercentageUsed: weekly}
		if !reset.IsZero() {
			w.WindowRefreshesAt = &reset
		}
		q.Weekly = w
	}
	if !reset.IsZero() && q.FiveHour != nil {
		q.FiveHour.WindowRefreshesAt = &reset
	}
	a.setQuota(q, nil)
}

func TestResolvedAccountsLegacyFallback(t *testing.T) {
	cfg := config.Default()
	cfg.AuthUser = "3"
	cfg.CookieFile = "/tmp/ck.json"

	got := cfg.ResolvedAccounts()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].AuthUser != "3" {
		t.Errorf("AuthUser = %q, want 3", got[0].AuthUser)
	}
	if got[0].CookieFile != "/tmp/ck.json" {
		t.Errorf("CookieFile = %q, want inherited value", got[0].CookieFile)
	}
}

func TestResolvedAccountsFiltersDisabled(t *testing.T) {
	disabled := false
	cfg := config.Default()
	cfg.Accounts = []config.AccountConfig{
		{AuthUser: "0"},
		{AuthUser: "1", Enabled: &disabled},
		{AuthUser: "2"},
	}
	got := cfg.ResolvedAccounts()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].AuthUser != "0" || got[1].AuthUser != "2" {
		t.Errorf("unexpected accounts: %+v", got)
	}
}

func TestResolvedAccountsInheritsCookieFile(t *testing.T) {
	cfg := config.Default()
	cfg.CookieFile = "/shared/cookie.json"
	cfg.XSRFToken = "base-token"
	cfg.Accounts = []config.AccountConfig{
		{AuthUser: "0"},
		{AuthUser: "1", XSRFToken: "own-token", CookieFile: "/own/cookie.json"},
	}
	got := cfg.ResolvedAccounts()
	if got[0].CookieFile != "/shared/cookie.json" {
		t.Errorf("account 0 CookieFile = %q", got[0].CookieFile)
	}
	if got[0].XSRFToken != "base-token" {
		t.Errorf("account 0 XSRFToken = %q", got[0].XSRFToken)
	}
	if got[1].CookieFile != "/own/cookie.json" {
		t.Errorf("account 1 CookieFile = %q, explicit value should win", got[1].CookieFile)
	}
	if got[1].XSRFToken != "own-token" {
		t.Errorf("account 1 XSRFToken = %q", got[1].XSRFToken)
	}
}

func TestPoolStickyPrimary(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})

	// Nothing known about usage: the first account always wins.
	for i := 0; i < 5; i++ {
		got, err := p.Pick(nil)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if got.AuthUser != "0" {
			t.Fatalf("Pick #%d = u/%s, want u/0 (sticky primary)", i, got.AuthUser)
		}
	}
}

func TestPoolSwitchesWhenPrimaryOverThreshold(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})
	reset := time.Now().Add(3 * time.Hour)

	setQuota(t, p.accounts[0], 0.99, 0.10, reset)
	setQuota(t, p.accounts[1], 0.02, 0.01, reset)

	got, err := p.Pick(nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.AuthUser != "1" {
		t.Errorf("Pick = u/%s, want u/1 after primary exceeded threshold", got.AuthUser)
	}
}

func TestPoolReturnsToPrimaryAfterWindowRefresh(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})

	// Primary is over threshold but its window already refreshed, so it is
	// usable again.
	reset := time.Now().Add(-time.Minute)
	setQuota(t, p.accounts[0], 0.99, 0.10, reset)
	setQuota(t, p.accounts[1], 0.02, 0.01, time.Now().Add(time.Hour))

	got, err := p.Pick(nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.AuthUser != "0" {
		t.Errorf("Pick = u/%s, want u/0 once the window has refreshed", got.AuthUser)
	}
}

func TestPoolAllExhausted(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})
	reset := time.Now().Add(2 * time.Hour)
	setQuota(t, p.accounts[0], 0.97, 0.5, reset)
	setQuota(t, p.accounts[1], 0.96, 0.5, reset)

	_, err := p.Pick(nil)
	if err == nil {
		t.Fatal("expected ErrAllExhausted")
	}
	var exhausted *ErrAllExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("err type = %T, want *ErrAllExhausted", err)
	}
	if len(exhausted.Accounts) != 2 {
		t.Fatalf("accounts in error = %d, want 2", len(exhausted.Accounts))
	}
	for _, a := range exhausted.Accounts {
		if a.Available {
			t.Errorf("account %s reported available", a.Label)
		}
		if a.ResetAt == nil {
			t.Errorf("account %s missing reset_at", a.Label)
		}
	}
}

func TestPoolUnknownQuotaIsAvailable(t *testing.T) {
	// A failed quota probe must not take the pool offline.
	p := newTestPool(t, poolConfig(0.95, "0"), &stubRequester{})
	p.accounts[0].setQuota(nil, errors.New("probe failed"))

	got, err := p.Pick(nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got == nil {
		t.Fatal("Pick returned nil account")
	}
}

func TestPoolCooldownBlocksAccount(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})
	setQuota(t, p.accounts[0], 0.0, 0.0, time.Time{})

	// Simulate a real upstream 429 on the primary.
	p.MarkExhausted(p.accounts[0], &gemini.UpstreamError{Status: 429, Kind: "http"})

	got, err := p.Pick(nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.AuthUser != "1" {
		t.Errorf("Pick = u/%s, want u/1 while primary is cooling down", got.AuthUser)
	}
}

func TestQuotaRefreshClearsCooldown(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})
	setQuota(t, p.accounts[0], 0.0, 0.0, time.Time{})
	p.MarkExhausted(p.accounts[0], &gemini.UpstreamError{Status: 429, Kind: "http"})

	if p.accounts[0].Available(0.95, time.Now()) {
		t.Fatal("account should be unavailable during cooldown")
	}

	// Fresh usage data means the cooldown is stale.
	setQuota(t, p.accounts[0], 0.05, 0.01, time.Time{})
	p.accounts[0].clearExhausted()

	if !p.accounts[0].Available(0.95, time.Now()) {
		t.Error("account should be available again after quota refresh")
	}
}

func TestPickSkipsTriedAccounts(t *testing.T) {
	p := newTestPool(t, poolConfig(0.95, "0", "1"), &stubRequester{})

	got, err := p.Pick(map[int]bool{0: true})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.AuthUser != "1" {
		t.Errorf("Pick = u/%s, want u/1 when u/0 is skipped", got.AuthUser)
	}

	if _, err := p.Pick(map[int]bool{0: true, 1: true}); err == nil {
		t.Error("expected error when every account is skipped")
	}
}

func TestGenerateFailsOverOnQuotaError(t *testing.T) {
	req := &stubRequester{status: http.StatusTooManyRequests, body: "quota"}
	p := newTestPool(t, poolConfig(0.95, "0", "1"), req)

	_, _, err := p.Generate(context.Background(), "hi", gemini.ModelOptions{}, nil, nil)
	if err == nil {
		t.Fatal("expected error when both accounts are rate limited")
	}

	// Both accounts should have been marked exhausted.
	for _, a := range p.accounts {
		if a.Available(0.95, time.Now()) {
			t.Errorf("account %s still available after a 429", a.Label)
		}
	}
}

func TestGenerateExhaustedWithoutUpstreamCall(t *testing.T) {
	req := &stubRequester{}
	p := newTestPool(t, poolConfig(0.95, "0", "1"), req)
	reset := time.Now().Add(time.Hour)
	setQuota(t, p.accounts[0], 0.99, 0.99, reset)
	setQuota(t, p.accounts[1], 0.98, 0.98, reset)

	_, _, err := p.Generate(context.Background(), "hi", gemini.ModelOptions{}, nil, nil)
	if !IsAllExhausted(err) {
		t.Fatalf("err = %v, want ErrAllExhausted", err)
	}
	if req.calls != 0 {
		t.Errorf("upstream calls = %d, want 0 (must short-circuit when exhausted)", req.calls)
	}
}

// streamingRequester returns a stream body that carries text, so a real delta is
// emitted before the connection ends.
type streamingRequester struct {
	body  string
	calls int
}

func (s *streamingRequester) Do(req *http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// wrbLine builds one batchexecute stream frame carrying the given text.
// The line must exceed 200 bytes and follow the [["wrb.fr",null,"<inner>"]] shape
// that ExtractTextsFromLine expects, so it is assembled with json.Marshal rather
// than string concatenation.
func wrbLine(t *testing.T, text string) string {
	t.Helper()

	// inner[4] holds the parts; each part is ["rc_id", ["text"], ...].
	part := []any{"rc_1", []string{text}, nil, nil, nil, nil, nil, nil, []int{1}, "en"}
	inner := []any{strings.Repeat("x", 220), nil, nil, nil, []any{part}}

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("marshal inner: %v", err)
	}
	line := []any{[]any{"wrb.fr", nil, string(innerJSON)}}
	lineJSON, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("marshal line: %v", err)
	}

	out := string(lineJSON) + "\n"
	if len(out) < 200 {
		t.Fatalf("fixture line too short (%d bytes), ExtractTextsFromLine would ignore it", len(out))
	}
	return out
}

func TestGenerateStreamEmitsDeltas(t *testing.T) {
	req := &streamingRequester{body: wrbLine(t, "hello")}
	p := newTestPool(t, poolConfig(0.95, "0"), req)
	setQuota(t, p.accounts[0], 0.0, 0.0, time.Time{})

	var got []string
	_, err := p.GenerateStream(context.Background(), "hi", gemini.ModelOptions{}, nil, nil, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("deltas = %q, want [hello]", got)
	}
}

// failThenEmitRequester fails the first account with a quota error and lets the
// second succeed is not possible through a single requester, so the split is
// modelled by switching the requester between calls.
type switchRequester struct {
	responses []*http.Response
	index     int
}

func (s *switchRequester) Do(req *http.Request) (*http.Response, error) {
	if s.index >= len(s.responses) {
		return nil, errors.New("no more responses")
	}
	resp := s.responses[s.index]
	s.index++
	return resp, nil
}

func TestGenerateStreamFailsOverBeforeFirstDelta(t *testing.T) {
	quotaResp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       io.NopCloser(strings.NewReader("quota")),
		Header:     make(http.Header),
	}
	okResp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(wrbLine(t, "switched"))),
		Header:     make(http.Header),
	}
	req := &switchRequester{responses: []*http.Response{quotaResp, okResp}}
	p := newTestPool(t, poolConfig(0.95, "0", "1"), req)
	setQuota(t, p.accounts[0], 0.0, 0.0, time.Time{})
	setQuota(t, p.accounts[1], 0.0, 0.0, time.Time{})

	var got []string
	_, err := p.GenerateStream(context.Background(), "hi", gemini.ModelOptions{}, nil, nil, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream should fail over to account 1, got: %v", err)
	}
	if len(got) != 1 || got[0] != "switched" {
		t.Errorf("deltas = %q, want [switched]", got)
	}
	if p.accounts[0].Available(0.95, time.Now()) {
		t.Error("account 0 should be cooling down after the 429")
	}
}

func TestIsQuotaErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", &gemini.UpstreamError{Status: 429, Kind: "http"}, true},
		{"403", &gemini.UpstreamError{Status: 403, Kind: "http"}, true},
		{"400", &gemini.UpstreamError{Status: 400, Kind: "http"}, false},
		{"500", &gemini.UpstreamError{Status: 500, Kind: "http"}, false},
		{"transport", &gemini.UpstreamError{Kind: "transport"}, false},
		{"bard plain", &gemini.UpstreamError{Kind: "bard", Msg: "parse"}, false},
		{"bard info", &gemini.UpstreamError{Kind: "bard", Msg: "BardErrorInfo [1024]"}, true},
		{"plain error", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gemini.IsQuotaError(tt.err); got != tt.want {
				t.Errorf("IsQuotaError = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsCredentialErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"400", &gemini.UpstreamError{Status: 400, Kind: "http"}, true},
		{"401", &gemini.UpstreamError{Status: 401, Kind: "http"}, true},
		{"403", &gemini.UpstreamError{Status: 403, Kind: "http"}, true},
		{"429", &gemini.UpstreamError{Status: 429, Kind: "http"}, false},
		{"500", &gemini.UpstreamError{Status: 500, Kind: "http"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gemini.IsCredentialError(tt.err); got != tt.want {
				t.Errorf("IsCredentialError = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSnapshotRendering(t *testing.T) {
	p := newTestPool(t, poolConfig(0.9, "0", "1"), &stubRequester{})
	reset := time.Now().Add(90 * time.Minute)
	setQuota(t, p.accounts[0], 0.93, 0.4, reset)

	snap := p.accounts[0].Snapshot(0.9, time.Now())
	if snap.Available {
		t.Error("account over 90% threshold should be unavailable")
	}
	if snap.Tier != "TIER1" {
		t.Errorf("Tier = %q", snap.Tier)
	}
	if snap.ResetAt == nil {
		t.Error("ResetAt should be set")
	}
	if snap.FiveHour == nil || snap.FiveHour.PercentageUsed != 0.93 {
		t.Errorf("FiveHour = %+v", snap.FiveHour)
	}

	snap1 := p.accounts[1].Snapshot(0.9, time.Now())
	if !snap1.Available {
		t.Error("account with no quota data should be available")
	}
}

// A conversation handle is scoped to the cookie that created it, so it must not
// be replayed after the pool moves to another account.
func TestConvAccountBinding(t *testing.T) {
	cfg := poolConfig(0.95, "0", "1")
	cfg.LogRequests = false
	app := New(cfg, "test")

	store, err := conv.Open("")
	if err != nil {
		t.Fatal(err)
	}
	app.Pool.convs = store

	hist := []conv.Msg{{Role: "user", Content: "hi"}}
	key := conv.HistoryKey("gemini-4-hydrogen", hist)
	store.Put(&conv.Record{
		Key:      key,
		Model:    conv.NormalizeModel("gemini-4-hydrogen"),
		Account:  "0",
		Metadata: []any{"c_1", "r_1"},
	})

	if _, _, hit := app.resolveConv("gemini-4-hydrogen", hist, "0"); !hit {
		t.Error("the creating account must reuse its own handle")
	}
	if _, _, hit := app.resolveConv("gemini-4-hydrogen", hist, "1"); hit {
		t.Error("another account must not replay a foreign conversation id")
	}
}

// The resolver callback is what the pool calls per candidate account; it must
// return the full prompt on a miss and the short prompt on a hit.
func TestConvResolverPromptSelection(t *testing.T) {
	cfg := poolConfig(0.95, "0")
	cfg.LogRequests = false
	app := New(cfg, "test")

	store, _ := conv.Open("")
	app.Pool.convs = store

	msgs := []models.OpenAIMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	history := chatMessagesToConvMsgs(msgs)
	prefix := history[:len(history)-1]

	full := "FULL-HISTORY-PROMPT"
	last := lastMessageText(msgs)
	if last != "second" {
		t.Fatalf("lastMessageText = %q", last)
	}

	var convKey string
	var convHit bool
	resolve := func(account string) ([]any, string, string) {
		meta, key, hit := app.resolveConv("gemini-4-hydrogen", prefix, account)
		if !hit {
			return nil, full, key
		}
		convKey, convHit = key, true
		return meta, last, key
	}

	// Miss: full prompt, no metadata.
	meta, prompt, _ := resolve("0")
	if meta != nil {
		t.Errorf("miss should not return metadata, got %v", meta)
	}
	if prompt != full {
		t.Errorf("miss prompt = %q, want the full history", prompt)
	}
	if convHit {
		t.Error("convHit should be false on a miss")
	}

	// Now cache a handle and expect the short prompt.
	key := conv.HistoryKey("gemini-4-hydrogen", prefix)
	store.Put(&conv.Record{Key: key, Model: conv.NormalizeModel("gemini-4-hydrogen"),
		Account: "0", Metadata: []any{"c_1", "r_1"}})

	meta, prompt, keyOut := resolve("0")
	if len(meta) != 2 {
		t.Errorf("hit should return metadata, got %v", meta)
	}
	if prompt != last {
		t.Errorf("hit prompt = %q, want only the new message", prompt)
	}
	if !convHit || convKey == "" || keyOut == "" {
		t.Error("hit should be recorded")
	}
}

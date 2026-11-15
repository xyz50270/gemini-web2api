package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/conv"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/multimodal"
)

// exhaustedCooldown is how long a real upstream 429/403 keeps an account out of
// rotation when the usage API has no fresher information to go on.
const exhaustedCooldown = 10 * time.Minute

// Account is one Google login the pool can serve requests from.
type Account struct {
	Index    int
	Label    string
	AuthUser string

	Client *gemini.Client
	Tokens *multimodal.TokenCache

	mu        sync.RWMutex
	xsrfToken string
	// xsrfFailedAt records the last failed page-token fetch. A 302 on /app means
	// the session is gone and will not fix itself within seconds, so retrying on
	// every request just floods the log; back off instead.
	xsrfFailedAt   time.Time
	xsrfFailReason string
	quota          *gemini.Quota
	quotaErr       string
	exhaustedUntil time.Time
	lastErr        string
	lastUsed       time.Time
}

// Quota returns a copy of the last known usage state.
func (a *Account) Quota() *gemini.Quota {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.quota == nil {
		return nil
	}
	cp := *a.quota
	return &cp
}

// XSRFToken returns the cached page token for this account.
func (a *Account) XSRFToken() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.xsrfToken
}

func (a *Account) setXSRFToken(tok string) {
	a.mu.Lock()
	a.xsrfToken = tok
	a.mu.Unlock()
	a.Client.Cfg.XSRFToken = tok
}

func (a *Account) setQuota(q *gemini.Quota, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.quotaErr = err.Error()
		return
	}
	a.quota = q
	a.quotaErr = ""
}

func (a *Account) markExhausted(until time.Time) {
	a.mu.Lock()
	a.exhaustedUntil = until
	a.mu.Unlock()
}

func (a *Account) clearExhausted() {
	a.mu.Lock()
	a.exhaustedUntil = time.Time{}
	a.mu.Unlock()
}

func (a *Account) recordErr(err error) {
	a.mu.Lock()
	if err != nil {
		a.lastErr = err.Error()
	} else {
		a.lastErr = ""
	}
	a.mu.Unlock()
}

func (a *Account) recordUse() {
	a.mu.Lock()
	a.lastUsed = time.Now()
	a.mu.Unlock()
}

// Available reports whether the account may serve a request right now.
// An account without quota data is treated as available: a failing usage probe
// must not take the whole service offline.
func (a *Account) Available(threshold float64, now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if !a.exhaustedUntil.IsZero() && now.Before(a.exhaustedUntil) {
		return false
	}
	if a.quota != nil {
		if used := a.quota.MaxUsed(); used >= threshold {
			// Still blocked until the window actually refreshes.
			if reset, ok := a.quota.NextReset(); ok && now.Before(reset) {
				return false
			}
		}
	}
	return true
}

// ExhaustedAccount is the status snapshot used for the 429 body and /v1/accounts.
type ExhaustedAccount struct {
	Index         int               `json:"index"`
	Label         string            `json:"label"`
	AuthUser      string            `json:"auth_user"`
	Available     bool              `json:"available"`
	Tier          string            `json:"tier,omitempty"`
	Used          float64           `json:"used"`
	FiveHour      *gemini.QuotaPool `json:"five_hour,omitempty"`
	Weekly        *gemini.QuotaPool `json:"weekly,omitempty"`
	ResetAt       *time.Time        `json:"reset_at,omitempty"`
	CooldownUntil *time.Time        `json:"cooldown_until,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	QuotaError    string            `json:"quota_error,omitempty"`
}

// ErrAllExhausted is returned when every account is over the usage threshold.
type ErrAllExhausted struct {
	Accounts []ExhaustedAccount
}

func (e *ErrAllExhausted) Error() string {
	return fmt.Sprintf("all %d accounts have reached the quota threshold", len(e.Accounts))
}

// Pool owns the per-account clients and picks which one serves a request.
type Pool struct {
	cfg      config.Config
	accounts []*Account
	req      gemini.Requester
	logf     func(format string, args ...any)

	// limiter serialises upstream generation across all accounts. Google's
	// anti-abuse heuristics expect one in-flight generation at a time; letting
	// several run concurrently is the main thing that gets an egress IP flagged.
	limiter *Limiter

	// convs caches the server-side conversation handles. Nil disables reuse.
	convs *conv.Store

	stopOnce sync.Once
	stop     chan struct{}
}

// NewPool builds one gemini.Client and one TokenCache per configured account.
// The accounts are ordered exactly as configured; Pick always prefers the first
// available entry, which gives sticky-primary behaviour: the secondary only
// takes traffic once the primary is over its threshold or cooling down.
func NewPool(cfg config.Config, req gemini.Requester, logf func(format string, args ...any)) *Pool {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	resolved := cfg.ResolvedAccounts()
	p := &Pool{
		cfg:     cfg,
		req:     req,
		logf:    logf,
		limiter: NewLimiter(cfg.MaxConcurrent, cfg.QueueTimeoutSec),
		stop:    make(chan struct{}),
	}

	httpClient := createHTTPClient(cfg)

	for i, ac := range resolved {
		acctCfg := cfg
		acctCfg.AuthUser = ac.AuthUser
		acctCfg.XSRFToken = ac.XSRFToken
		acctCfg.CookieFile = ac.CookieFile

		client := gemini.NewClient(acctCfg)
		// NewClient already wrapped the requester in the cookie-rotation recorder;
		// swapping in the shared requester here would drop that wrapper and the
		// session would silently stop refreshing.
		client.HTTP = gemini.NewRotatingRequester(req, client.Cookies)
		client.FailFast = true

		acct := &Account{
			Index:     i,
			Label:     ac.Label,
			AuthUser:  ac.AuthUser,
			Client:    client,
			Tokens:    multimodal.NewTokenCache(acctCfg, client.Cookies, httpClient),
			xsrfToken: ac.XSRFToken,
		}
		p.accounts = append(p.accounts, acct)
	}

	return p
}

// Accounts exposes the pool entries for status reporting.
func (p *Pool) Accounts() []*Account {
	return p.accounts
}

// Len reports how many accounts the pool manages.
func (p *Pool) Len() int {
	return len(p.accounts)
}

// Requester returns the shared outbound HTTP client, for image fetching.
func (p *Pool) Requester() gemini.Requester {
	return p.req
}

// Pick returns the first available account. Accounts listed in skip are ignored.
func (p *Pool) Pick(skip map[int]bool) (*Account, error) {
	now := time.Now()
	for _, a := range p.accounts {
		if skip != nil && skip[a.Index] {
			continue
		}
		if a.Available(p.threshold(), now) {
			return a, nil
		}
	}
	return nil, p.exhaustedError(now)
}

func (p *Pool) threshold() float64 {
	if p.cfg.QuotaThreshold <= 0 || p.cfg.QuotaThreshold > 1 {
		return 0.95
	}
	return p.cfg.QuotaThreshold
}

func (p *Pool) exhaustedError(now time.Time) error {
	out := make([]ExhaustedAccount, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, a.Snapshot(p.threshold(), now))
	}
	return &ErrAllExhausted{Accounts: out}
}

// Snapshot renders the current state of one account.
func (a *Account) Snapshot(threshold float64, now time.Time) ExhaustedAccount {
	a.mu.RLock()
	defer a.mu.RUnlock()

	s := ExhaustedAccount{
		Index:      a.Index,
		Label:      a.Label,
		AuthUser:   a.AuthUser,
		LastError:  a.lastErr,
		QuotaError: a.quotaErr,
	}
	if !a.exhaustedUntil.IsZero() && now.Before(a.exhaustedUntil) {
		t := a.exhaustedUntil
		s.CooldownUntil = &t
	}
	if q := a.quota; q != nil {
		s.Tier = q.Tier
		s.Used = q.MaxUsed()
		s.FiveHour = q.FiveHour
		s.Weekly = q.Weekly
		if reset, ok := q.NextReset(); ok {
			s.ResetAt = &reset
		}
	}
	s.Available = a.AvailableLocked(threshold, now)
	return s
}

// AvailableLocked mirrors Available for callers that already hold a.mu.
func (a *Account) AvailableLocked(threshold float64, now time.Time) bool {
	if !a.exhaustedUntil.IsZero() && now.Before(a.exhaustedUntil) {
		return false
	}
	if a.quota != nil {
		if used := a.quota.MaxUsed(); used >= threshold {
			if reset, ok := a.quota.NextReset(); ok && now.Before(reset) {
				return false
			}
		}
	}
	return true
}

// MarkExhausted records that upstream rejected this account for quota reasons.
func (p *Pool) MarkExhausted(a *Account, err error) {
	a.recordErr(err)
	a.markExhausted(time.Now().Add(exhaustedCooldown))
	p.logf("account %s exhausted, cooling down for %s: %v", a.Label, exhaustedCooldown, err)
}

// RefreshQuota fetches the usage state for one account.
func (p *Pool) RefreshQuota(ctx context.Context, a *Account) error {
	info, err := a.Client.Cookies.Load()
	if err != nil {
		a.setQuota(nil, err)
		return err
	}
	q, err := gemini.FetchQuota(ctx, p.req, info, a.AuthUser)
	a.setQuota(q, err)
	if err == nil {
		// Fresh usage data supersedes a stale cooldown.
		a.clearExhausted()
	}
	return err
}

// RefreshAllQuotas refreshes every account. Errors are logged, not returned,
// except when every account fails.
func (p *Pool) RefreshAllQuotas(ctx context.Context) error {
	var firstErr error
	for _, a := range p.accounts {
		if err := p.RefreshQuota(ctx, a); err != nil {
			p.logf("account %s: quota refresh failed: %v", a.Label, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil && p.quotaKnown() == 0 {
		return firstErr
	}
	return nil
}

func (p *Pool) quotaKnown() int {
	n := 0
	for _, a := range p.accounts {
		if a.Quota() != nil {
			n++
		}
	}
	return n
}

// RefreshXSRFToken reloads the page token for one account.
func (p *Pool) RefreshXSRFToken(a *Account) error {
	info, err := a.Client.Cookies.Load()
	if err != nil {
		return err
	}
	tok, _, err := gemini.FetchPageAuth(p.req, info, a.AuthUser)
	if err != nil {
		return err
	}
	a.setXSRFToken(tok)
	p.logf("account %s: xsrf token refreshed", a.Label)
	return nil
}

// StartBackground launches the quota refresh loop. Cancel ctx to stop it.
func (p *Pool) StartBackground(ctx context.Context) {
	interval := time.Duration(p.cfg.QuotaRefreshSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case <-ticker.C:
				// Refresh tokens that are missing or were rejected, then quotas.
				for _, a := range p.accounts {
					if p.cfg.AutoXSRF && a.XSRFToken() == "" {
						_ = p.RefreshXSRFToken(a)
					}
				}
				// Google rotates SIDCC roughly every 10 minutes and invalidates the
				// previous chain, so keep it moving even with no traffic.
				p.RotateCookiesAll(ctx)
				_ = p.RefreshAllQuotas(ctx)
			}
		}
	}()
}

// Close stops background work.
func (p *Pool) Close() {
	p.stopOnce.Do(func() { close(p.stop) })
}

// xsrfRetryCooldown is how long a failed page-token fetch suppresses further
// attempts. A dead session returns 302 on every try, so retrying per request
// only floods the log without ever succeeding.
const xsrfRetryCooldown = 10 * time.Minute

// ensureXSRF makes sure the account carries a usable page token, fetching one on
// first use. It is a no-op when the account already has a token.
//
// The token is optional for generation: when absent, BuildBody simply omits the
// `at` field and upstream still answers. So a failed refresh must never block a
// request — it is logged once and then backed off.
func (p *Pool) ensureXSRF(a *Account) {
	if a.XSRFToken() != "" || !p.cfg.AutoXSRF {
		return
	}

	a.mu.RLock()
	failedAt := a.xsrfFailedAt
	reason := a.xsrfFailReason
	a.mu.RUnlock()

	if !failedAt.IsZero() && time.Since(failedAt) < xsrfRetryCooldown {
		// Already failing recently; stay quiet until the cooldown expires.
		_ = reason
		return
	}

	if err := p.RefreshXSRFToken(a); err != nil {
		a.mu.Lock()
		a.xsrfFailedAt = time.Now()
		a.xsrfFailReason = err.Error()
		a.mu.Unlock()
		p.logf("account %s: xsrf refresh failed (%v); will retry in %s. "+
			"Generation still works without the token, but a 302 on /app usually means the cookie needs re-exporting",
			a.Label, err, xsrfRetryCooldown)
		return
	}

	a.mu.Lock()
	a.xsrfFailedAt = time.Time{}
	a.xsrfFailReason = ""
	a.mu.Unlock()
}

// uploadFor uploads the request images through one specific account.
func (p *Pool) uploadFor(a *Account, images []format.Image) ([]string, error) {
	if len(images) == 0 {
		return nil, nil
	}
	return uploadImagesWith(a.Tokens, a.Client, p.req, images)
}

// Generate runs a non-streaming completion with account failover.
//
// conv continues an existing upstream conversation; on success the returned meta
// is what the caller should cache for the next turn. The admission limiter wraps
// the whole failover loop, so retries and account switches never overlap another
// request.
func (p *Pool) Generate(ctx context.Context, prompt string, opt gemini.ModelOptions, images []format.Image, convMeta []any) (string, *gemini.ConversationMeta, error) {
	if err := p.limiter.Acquire(ctx); err != nil {
		return "", nil, err
	}
	defer p.limiter.Release()

	text, meta, err := p.generateOnce(prompt, opt, images, convMeta)
	if err == nil || convMeta == nil || !gemini.IsCredentialError(err) {
		return text, meta, err
	}

	// The cached conversation handle was rejected. Drop it and retry once from
	// scratch with the full history, so a stale or incompatible cid degrades to
	// a fresh conversation instead of surfacing an error.
	p.logf("conversation handle rejected (%v); retrying as a new conversation", err)
	return p.generateOnce(prompt, opt, images, nil)
}

func (p *Pool) generateOnce(prompt string, opt gemini.ModelOptions, images []format.Image, convMeta []any) (string, *gemini.ConversationMeta, error) {
	tried := make(map[int]bool)
	var lastErr error

	for len(tried) < len(p.accounts) {
		acct, err := p.Pick(tried)
		if err != nil {
			if lastErr != nil {
				return "", nil, lastErr
			}
			return "", nil, err
		}
		tried[acct.Index] = true

		p.ensureXSRF(acct)

		fileRefs, err := p.uploadFor(acct, images)
		if err != nil {
			acct.recordErr(err)
			lastErr = fmt.Errorf("account %s: image upload failed: %w", acct.Label, err)
			continue
		}

		text, meta, err := acct.Client.Generate(prompt, opt, fileRefs, convMeta)
		if err == nil {
			acct.recordErr(nil)
			acct.recordUse()
			if meta != nil {
				meta.Account = acct.AuthUser
			}
			return text, meta, nil
		}

		lastErr = err
		if gemini.IsQuotaError(err) {
			p.MarkExhausted(acct, err)
			continue
		}
		if gemini.IsCredentialError(err) {
			acct.recordErr(err)
			if p.cfg.AutoXSRF {
				if rerr := p.RefreshXSRFToken(acct); rerr == nil {
					if text, meta, rerr := acct.Client.Generate(prompt, opt, fileRefs, convMeta); rerr == nil {
						acct.recordErr(nil)
						acct.recordUse()
						if meta != nil {
							meta.Account = acct.AuthUser
						}
						return text, meta, nil
					}
				}
			}
			continue
		}
		return "", nil, err
	}

	if lastErr != nil {
		return "", nil, lastErr
	}
	return "", nil, p.exhaustedError(time.Now())
}

// GenerateStream runs a streaming completion with account failover. Failover is
// only attempted while nothing has been emitted to the client: once a delta is
// out, the stream is committed and the error is returned as-is.
func (p *Pool) GenerateStream(ctx context.Context, prompt string, opt gemini.ModelOptions, images []format.Image, convMeta []any, emit func(string) error) (*gemini.ConversationMeta, error) {
	if err := p.limiter.Acquire(ctx); err != nil {
		return nil, err
	}
	defer p.limiter.Release()

	meta, err := p.generateStreamOnce(prompt, opt, images, convMeta, emit)
	if err == nil || convMeta == nil || !gemini.IsCredentialError(err) {
		return meta, err
	}

	p.logf("conversation handle rejected (%v); retrying stream as a new conversation", err)
	return p.generateStreamOnce(prompt, opt, images, nil, emit)
}

func (p *Pool) generateStreamOnce(prompt string, opt gemini.ModelOptions, images []format.Image, convMeta []any, emit func(string) error) (*gemini.ConversationMeta, error) {
	tried := make(map[int]bool)
	var lastErr error

	for len(tried) < len(p.accounts) {
		acct, err := p.Pick(tried)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		tried[acct.Index] = true

		p.ensureXSRF(acct)

		fileRefs, err := p.uploadFor(acct, images)
		if err != nil {
			acct.recordErr(err)
			lastErr = fmt.Errorf("account %s: image upload failed: %w", acct.Label, err)
			continue
		}

		emitted := false
		tracked := func(delta string) error {
			emitted = true
			return emit(delta)
		}

		meta, err := acct.Client.GenerateStream(prompt, opt, fileRefs, convMeta, tracked)
		if err == nil {
			acct.recordErr(nil)
			acct.recordUse()
			if meta != nil {
				meta.Account = acct.AuthUser
			}
			return meta, nil
		}
		if emitted {
			// Client already has partial output; a different account would
			// restart the text, so surface the error instead.
			acct.recordErr(err)
			return nil, err
		}

		lastErr = err
		if gemini.IsQuotaError(err) {
			p.MarkExhausted(acct, err)
			continue
		}
		if gemini.IsCredentialError(err) {
			acct.recordErr(err)
			if p.cfg.AutoXSRF {
				if rerr := p.RefreshXSRFToken(acct); rerr == nil {
					emitted = false
					meta, err = acct.Client.GenerateStream(prompt, opt, fileRefs, convMeta, tracked)
					if err == nil {
						acct.recordErr(nil)
						acct.recordUse()
						if meta != nil {
							meta.Account = acct.AuthUser
						}
						return meta, nil
					}
					if emitted {
						acct.recordErr(err)
						return nil, err
					}
					lastErr = err
				}
			}
			continue
		}
		return nil, err
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, p.exhaustedError(time.Now())
}

// IsAllExhausted reports whether err means the pool had nothing to serve with.
func IsAllExhausted(err error) bool {
	var e *ErrAllExhausted
	return errors.As(err, &e)
}

// writeExhaustedJSON renders the 429 response for a fully spent pool.
func writeExhaustedJSON(w http.ResponseWriter, err error) bool {
	var e *ErrAllExhausted
	if !errors.As(err, &e) {
		return false
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"error": map[string]any{
			"message":  e.Error(),
			"type":     "insufficient_quota",
			"code":     "all_accounts_exhausted",
			"accounts": e.Accounts,
		},
	})
	return true
}

// CloseConversations flushes and stops the conversation cache.
func (p *Pool) CloseConversations() {
	if p.convs != nil {
		if err := p.convs.Close(); err != nil {
			p.logf("conversation cache flush failed: %v", err)
		}
	}
}

// ConversationStats reports cache size for the status endpoint.
func (p *Pool) ConversationStats() (entries int, enabled bool) {
	if p.convs == nil {
		return 0, false
	}
	return p.convs.Len(), true
}

// LimiterStats exposes the admission counters.
func (p *Pool) LimiterStats() (inflight, waiting int) {
	if p.limiter == nil {
		return 0, 0
	}
	return p.limiter.Stats()
}

// ConvResolver decides, once an account is chosen, whether a cached
// conversation handle may be replayed on that account.
//
// It has to be a callback rather than a lookup done by the handler: the account
// that will serve the request is only known after Pick(), and a conversation id
// is only reusable on the cookie that created it.
//
// It returns the metadata to continue with (nil for a fresh chat), the prompt to
// send, and an invalidation key for the cache entry.
type ConvResolver func(account string) (convMeta []any, prompt string, convKey string)

// GenerateWithConv is Generate with per-account conversation resolution.
func (p *Pool) GenerateWithConv(ctx context.Context, fullPrompt string, opt gemini.ModelOptions, images []format.Image, resolve ConvResolver) (string, *gemini.ConversationMeta, error) {
	if resolve == nil {
		return p.Generate(ctx, fullPrompt, opt, images, nil)
	}
	if err := p.limiter.Acquire(ctx); err != nil {
		return "", nil, err
	}
	defer p.limiter.Release()

	text, meta, err := p.generateWithConv(fullPrompt, opt, images, resolve)
	if err == nil || !gemini.IsCredentialError(err) {
		return text, meta, err
	}
	p.logf("conversation handle rejected (%v); retrying as a new conversation", err)
	return p.generateOnce(fullPrompt, opt, images, nil)
}

func (p *Pool) generateWithConv(fullPrompt string, opt gemini.ModelOptions, images []format.Image, resolve ConvResolver) (string, *gemini.ConversationMeta, error) {
	tried := make(map[int]bool)
	var lastErr error

	for len(tried) < len(p.accounts) {
		acct, err := p.Pick(tried)
		if err != nil {
			if lastErr != nil {
				return "", nil, lastErr
			}
			return "", nil, err
		}
		tried[acct.Index] = true

		convMeta, prompt, _ := resolve(acct.AuthUser)
		p.ensureXSRF(acct)

		fileRefs, err := p.uploadFor(acct, images)
		if err != nil {
			acct.recordErr(err)
			lastErr = fmt.Errorf("account %s: image upload failed: %w", acct.Label, err)
			continue
		}

		text, meta, err := acct.Client.Generate(prompt, opt, fileRefs, convMeta)
		if err == nil {
			acct.recordErr(nil)
			acct.recordUse()
			if meta != nil {
				meta.Account = acct.AuthUser
			}
			return text, meta, nil
		}

		lastErr = err
		if gemini.IsQuotaError(err) {
			p.MarkExhausted(acct, err)
			continue
		}
		if gemini.IsCredentialError(err) {
			acct.recordErr(err)
			if p.cfg.AutoXSRF {
				if rerr := p.RefreshXSRFToken(acct); rerr == nil {
					if text, meta, rerr := acct.Client.Generate(prompt, opt, fileRefs, convMeta); rerr == nil {
						acct.recordErr(nil)
						acct.recordUse()
						if meta != nil {
							meta.Account = acct.AuthUser
						}
						return text, meta, nil
					}
				}
			}
			continue
		}
		return "", nil, err
	}

	if lastErr != nil {
		return "", nil, lastErr
	}
	return "", nil, p.exhaustedError(time.Now())
}

// GenerateStreamWithConv is GenerateStream with per-account conversation
// resolution. It returns the resolved account label so the caller can tag the
// cache entry correctly.
func (p *Pool) GenerateStreamWithConv(ctx context.Context, fullPrompt string, opt gemini.ModelOptions, images []format.Image, resolve ConvResolver, emit func(string) error) (*gemini.ConversationMeta, error) {
	if resolve == nil {
		return p.GenerateStream(ctx, fullPrompt, opt, images, nil, emit)
	}
	if err := p.limiter.Acquire(ctx); err != nil {
		return nil, err
	}
	defer p.limiter.Release()

	meta, err := p.generateStreamWithConv(fullPrompt, opt, images, resolve, emit)
	if err == nil || !gemini.IsCredentialError(err) {
		return meta, err
	}
	p.logf("conversation handle rejected (%v); retrying stream as a new conversation", err)
	return p.generateStreamOnce(fullPrompt, opt, images, nil, emit)
}

func (p *Pool) generateStreamWithConv(fullPrompt string, opt gemini.ModelOptions, images []format.Image, resolve ConvResolver, emit func(string) error) (*gemini.ConversationMeta, error) {
	tried := make(map[int]bool)
	var lastErr error

	for len(tried) < len(p.accounts) {
		acct, err := p.Pick(tried)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		tried[acct.Index] = true

		convMeta, prompt, _ := resolve(acct.AuthUser)
		p.ensureXSRF(acct)

		fileRefs, err := p.uploadFor(acct, images)
		if err != nil {
			acct.recordErr(err)
			lastErr = fmt.Errorf("account %s: image upload failed: %w", acct.Label, err)
			continue
		}

		emitted := false
		tracked := func(delta string) error {
			emitted = true
			return emit(delta)
		}

		meta, err := acct.Client.GenerateStream(prompt, opt, fileRefs, convMeta, tracked)
		if err == nil {
			acct.recordErr(nil)
			acct.recordUse()
			if meta != nil {
				meta.Account = acct.AuthUser
			}
			return meta, nil
		}
		if emitted {
			acct.recordErr(err)
			return nil, err
		}

		lastErr = err
		if gemini.IsQuotaError(err) {
			p.MarkExhausted(acct, err)
			continue
		}
		if gemini.IsCredentialError(err) {
			acct.recordErr(err)
			if p.cfg.AutoXSRF {
				if rerr := p.RefreshXSRFToken(acct); rerr == nil {
					emitted = false
					meta, err = acct.Client.GenerateStream(prompt, opt, fileRefs, convMeta, tracked)
					if err == nil {
						acct.recordErr(nil)
						acct.recordUse()
						if meta != nil {
							meta.Account = acct.AuthUser
						}
						return meta, nil
					}
					if emitted {
						acct.recordErr(err)
						return nil, err
					}
					lastErr = err
				}
			}
			continue
		}
		return nil, err
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, p.exhaustedError(time.Now())
}

// writePoolError renders the non-upstream failures the pool can produce.
//
// A fully spent pool is 429 (retrying will not help until a window refreshes),
// while a queue timeout is 503 (transient: the service is busy, the request
// never reached Google). Both carry a machine-readable code so clients can tell
// them apart without parsing prose.
func writePoolError(w http.ResponseWriter, err error, queueTimeout time.Duration) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, ErrQueueTimeout) {
		retryAfter := int(queueTimeout.Seconds())
		if retryAfter <= 0 {
			retryAfter = 30
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"message": "another generation is still running; timed out waiting for a free slot",
				"type":    "service_unavailable",
				"code":    "queue_timeout",
			},
		})
		return true
	}

	var e *ErrAllExhausted
	if errors.As(err, &e) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message":  e.Error(),
				"type":     "insufficient_quota",
				"code":     "all_accounts_exhausted",
				"accounts": e.Accounts,
			},
		})
		return true
	}

	return false
}

// QueueTimeout returns the configured slot-wait budget, for error reporting.
func (p *Pool) QueueTimeout() time.Duration {
	if p.cfg.QueueTimeoutSec > 0 {
		return time.Duration(p.cfg.QueueTimeoutSec) * time.Second
	}
	return 0
}

// RotateCookiesAll runs one rotation cycle for every account.
//
// Passive rotation (replaying Set-Cookie) only happens while requests are in
// flight, so an idle service would still let its SIDCC chain go stale. This is
// the active half: it keeps the session fresh even with no traffic.
func (p *Pool) RotateCookiesAll(ctx context.Context) {
	for _, a := range p.accounts {
		if a.Client == nil {
			continue
		}
		before, _ := a.Client.Cookies.JarStats()
		if err := a.Client.RotateCookies(ctx); err != nil {
			p.logf("account %s: cookie rotation failed: %v", a.Label, err)
			continue
		}
		after, names := a.Client.Cookies.JarStats()
		if after > before {
			// Only log when something actually moved, otherwise the periodic
			// cycle would be pure noise.
			p.logf("account %s: cookie rotated (%d values refreshed, %d tracked)",
				a.Label, after-before, names)
		}
	}
}

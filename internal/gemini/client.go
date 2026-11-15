package gemini

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

type UpstreamError struct {
	Status int
	Kind   string
	Msg    string
}

func (e *UpstreamError) Error() string {
	return e.Msg
}

type Requester interface {
	Do(req *http.Request) (*http.Response, error)
}

type ModelOptions struct {
	ModelName   string
	ModelID     int
	ThinkMode   int
	ModelHeader string
	Extra       map[int]any
}

type Client struct {
	Cfg     config.Config
	HTTP    Requester
	Cookies *CookieCache
	Logf    func(format string, args ...any)
	blMu    sync.Mutex
	bl      string

	// FailFast makes credential/quota failures (400, 401, 403, 429) return
	// immediately instead of burning the retry budget on the same account, so
	// the caller can fail over to another account.
	FailFast bool

	modelsMu      sync.RWMutex
	discovered    []DiscoveredModel
	accountCap    int
	accountCapFld int
	discMu        sync.Mutex
	lastDiscFail  time.Time
}

func NewClient(cfg config.Config) *Client {
	var req Requester
	if cfg.Impersonate != "" && cfg.Proxy == "" {
		tlsAdapter, err := getTLSClient(cfg.Impersonate, cfg.RequestTimeoutSec)
		if err != nil {
			log.Printf("Failed to create TLS client for profile %s: %v, falling back to stdlib", cfg.Impersonate, err)
		} else {
			req = tlsAdapter
		}
	}

	if req == nil {
		transport := &http.Transport{
			DisableCompression: true,
		}

		if cfg.Proxy != "" {
			if proxyURL, err := url.Parse(cfg.Proxy); err == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
			}
		} else {
			transport.Proxy = http.ProxyFromEnvironment
		}

		req = &http.Client{
			Transport: transport,
			Timeout:   time.Duration(cfg.RequestTimeoutSec) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	logFn := func(format string, args ...any) {
		if cfg.LogRequests {
			log.Printf(format, args...)
		}
	}

	return &Client{
		Cfg:     cfg,
		HTTP:    req,
		Cookies: NewCookieCache(cfg.CookieFile),
		Logf:    logFn,
		bl:      cfg.GeminiBL,
	}
}

// CurrentBL returns the current BL value, preferring the mutable field over config.
func (c *Client) CurrentBL() string {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	if c.bl != "" {
		return c.bl
	}
	return c.Cfg.GeminiBL
}

// SetBL updates the BL value.
func (c *Client) SetBL(bl string) {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	c.bl = bl
}

// UpdateBLIfNeeded fetches the latest BL from Gemini and updates it if different from current.
// Returns (newBL, changed, error).
func (c *Client) UpdateBLIfNeeded() (string, bool, error) {
	newBL, err := FetchLatestBL(c.HTTP)
	if err != nil {
		return "", false, err
	}
	if !IsValidBL(newBL) {
		return "", false, fmt.Errorf("fetched BL %q is not a valid build id", newBL)
	}
	oldBL := c.CurrentBL()
	if newBL == oldBL {
		return newBL, false, nil
	}
	c.SetBL(newBL)
	return newBL, true, nil
}

func (c *Client) buildHeaders() http.Header {
	cookieInfo, _ := c.Cookies.Load()
	authUser := c.Cfg.AuthUser
	if authUser == "" && cookieInfo.AuthUser != "" {
		authUser = cookieInfo.AuthUser
	}

	prefix := AccountPrefix(authUser)
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("Origin", "https://gemini.google.com")
	h.Set("Referer", fmt.Sprintf("https://gemini.google.com%s/app", prefix))
	h.Set("X-Same-Domain", "1")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	if prefix != "" && authUser != "" {
		h.Set("X-Goog-AuthUser", authUser)
	}

	if cookieInfo.Cookie != "" {
		h.Set("Cookie", cookieInfo.Cookie)
	}
	if cookieInfo.SAPISID != "" {
		hash := SAPISIDHash(cookieInfo.SAPISID)
		if hash != "" {
			h.Set("Authorization", hash)
		}
	}

	if c.Cfg.Impersonate != "" {
		h.Set("Accept-Encoding", "identity")
	}

	return h
}

func (c *Client) triageStatus(resp *http.Response) error {
	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   "http",
			Msg:    "upstream blocked (redirected to sorry/index); IP may be flagged - retry later or use a different proxy/IP",
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   "http",
			Msg:    "upstream rate limited (HTTP 429); retry later",
		}
	}
	if resp.StatusCode != http.StatusOK {
		return &UpstreamError{
			Status: resp.StatusCode,
			Kind:   "http",
			Msg:    fmt.Sprintf("upstream HTTP %d", resp.StatusCode),
		}
	}
	return nil
}

func (c *Client) Generate(prompt string, opt ModelOptions, fileRefs []string) (string, error) {
	c.EnsureModelsDiscovered()

	cookieInfo, _ := c.Cookies.Load()
	cfg := c.Cfg
	if cfg.XSRFToken == "" && cookieInfo.XSRFToken != "" {
		cfg.XSRFToken = cookieInfo.XSRFToken
	}
	if cfg.AuthUser == "" && cookieInfo.AuthUser != "" {
		cfg.AuthUser = cookieInfo.AuthUser
	}

	reqUUID := strings.ToUpper(uuidV4())
	extra := make(map[int]any, len(opt.Extra)+1)
	for k, v := range opt.Extra {
		extra[k] = v
	}
	extra[59] = reqUUID

	modelHdr, effectiveMode := c.resolveModelHeader(opt, reqUUID)
	bodyStr := BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
	reqURL := BuildURL(cfg, c.CurrentBL())
	headers := c.buildHeaders()
	headers.Set(HeaderUUIDJspb, fmt.Sprintf(`["%s",1]`, reqUUID))
	if modelHdr != "" {
		headers.Set(ModelHeaderKey, modelHdr)
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	}

	blRefreshed := false
	var lastErr error

	for attempt := 0; attempt < c.Cfg.RetryAttempts; attempt++ {
		req, err := http.NewRequest("POST", reqURL, strings.NewReader(bodyStr))
		if err != nil {
			return "", err
		}
		req.Header = headers.Clone()

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: "transport", Msg: err.Error()}
		} else {
			if err := c.triageStatus(resp); err != nil {
				_ = resp.Body.Close()
				// 405 means the endpoint did not accept this verb/state. Refresh
				// the build id and retry once regardless of whether it changed:
				// a 405 is often transient, and the previous code only retried
				// when the BL value moved, so a stuck refresh left 405 fatal.
				if !blRefreshed && resp.StatusCode == http.StatusMethodNotAllowed {
					blRefreshed = true
					newBL, changed, blErr := c.UpdateBLIfNeeded()
					if blErr != nil {
						c.Logf("BL refresh on 405 failed, retrying with current BL: %v", blErr)
					} else if changed {
						c.Logf("BL auto-updated to %s, retrying request", newBL)
					} else {
						c.Logf("BL unchanged (%s), retrying request once anyway", newBL)
					}
					bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
					reqURL = BuildURL(cfg, c.CurrentBL())
					attempt-- // retry without consuming an attempt
					continue
				}
				lastErr = err
				// Hand credential/quota failures straight back to the pool so it
				// can switch accounts instead of hammering this one.
				if c.FailFast && (IsCredentialError(err) || IsQuotaError(err)) {
					return "", err
				}
			} else {
				rawBytes, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					lastErr = &UpstreamError{Kind: "transport", Msg: err.Error()}
				} else {
					text, err := ExtractResponseText(string(rawBytes))
					if err != nil {
						lastErr = &UpstreamError{Kind: "bard", Msg: err.Error()}
					} else {
						return text, nil
					}
				}
			}
		}

		if attempt < c.Cfg.RetryAttempts-1 {
			c.Logf("Retry %d/%d: %v", attempt+1, c.Cfg.RetryAttempts, lastErr)
			time.Sleep(time.Duration(c.Cfg.RetryDelaySec) * time.Second)
		}
	}

	return "", lastErr
}

func (c *Client) GenerateStream(prompt string, opt ModelOptions, fileRefs []string, emit func(string) error) error {
	c.EnsureModelsDiscovered()

	cookieInfo, _ := c.Cookies.Load()
	cfg := c.Cfg
	if cfg.XSRFToken == "" && cookieInfo.XSRFToken != "" {
		cfg.XSRFToken = cookieInfo.XSRFToken
	}
	if cfg.AuthUser == "" && cookieInfo.AuthUser != "" {
		cfg.AuthUser = cookieInfo.AuthUser
	}

	reqUUID := strings.ToUpper(uuidV4())
	extra := make(map[int]any, len(opt.Extra)+1)
	for k, v := range opt.Extra {
		extra[k] = v
	}
	extra[59] = reqUUID

	modelHdr, effectiveMode := c.resolveModelHeader(opt, reqUUID)
	bodyStr := BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
	reqURL := BuildURL(cfg, c.CurrentBL())
	headers := c.buildHeaders()
	headers.Set(HeaderUUIDJspb, fmt.Sprintf(`["%s",1]`, reqUUID))
	if modelHdr != "" {
		headers.Set(ModelHeaderKey, modelHdr)
		headers.Set(HeaderJspb89, "[0]")
		headers.Set(HeaderJspb90, "[0,0,0]")
	}

	blRefreshed := false
	parser := NewStreamParser()
	var lastErr error

	for attempt := 0; attempt < c.Cfg.RetryAttempts; attempt++ {
		parser.ResetBuffer()

		req, err := http.NewRequest("POST", reqURL, strings.NewReader(bodyStr))
		if err != nil {
			return err
		}
		req.Header = headers.Clone()

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = &UpstreamError{Kind: "transport", Msg: err.Error()}
		} else {
			if err := c.triageStatus(resp); err != nil {
				_ = resp.Body.Close()
				// Same 405 handling as the non-streaming path: refresh the build id
				// and retry once even when the value did not change.
				if !blRefreshed && resp.StatusCode == http.StatusMethodNotAllowed {
					blRefreshed = true
					newBL, changed, blErr := c.UpdateBLIfNeeded()
					if blErr != nil {
						c.Logf("BL refresh on 405 failed, retrying stream with current BL: %v", blErr)
					} else if changed {
						c.Logf("BL auto-updated to %s, retrying stream request", newBL)
					} else {
						c.Logf("BL unchanged (%s), retrying stream request once anyway", newBL)
					}
					bodyStr = BuildBody(prompt, effectiveMode, opt.ThinkMode, fileRefs, extra, cfg)
					reqURL = BuildURL(cfg, c.CurrentBL())
					attempt-- // retry without consuming an attempt
					continue
				}
				lastErr = err
				if c.FailFast && (IsCredentialError(err) || IsQuotaError(err)) {
					return err
				}
			} else {
				err = c.streamAttempt(resp.Body, parser, emit)
				_ = resp.Body.Close()
				if err == nil {
					return nil
				}
				if isClientDisconnect(err) {
					return err
				}
				lastErr = err
			}
		}

		if attempt < c.Cfg.RetryAttempts-1 {
			c.Logf("Stream retry %d/%d: %v", attempt+1, c.Cfg.RetryAttempts, lastErr)
			time.Sleep(time.Duration(c.Cfg.RetryDelaySec) * time.Second)
		}
	}

	return lastErr
}

func (c *Client) streamAttempt(body io.Reader, parser *StreamParser, emit func(string) error) error {
	reader := bufio.NewReader(body)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			deltas, parseErr := parser.Feed(string(lineBytes))
			if parseErr != nil {
				return &UpstreamError{Kind: "bard", Msg: parseErr.Error()}
			}
			for _, delta := range deltas {
				if err := emit(delta); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &UpstreamError{Kind: "transport", Msg: err.Error()}
		}
	}
}

// isClientDisconnect helps distinguish between upstream server errors (which we might want to retry)
// and actual client-side disconnections or non-retryable transport errors.
// If the client disconnected, we should abort the retry loop immediately to save resources.
func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*UpstreamError); ok {
		return false
	}
	return true
}

// IsQuotaError reports whether err looks like an account-level limit rather
// than a transient or request-level failure. Such errors are worth trying on a
// different account; a transport error or a 5xx is not.
func IsQuotaError(err error) bool {
	ue, ok := err.(*UpstreamError)
	if !ok {
		return false
	}
	switch ue.Status {
	case http.StatusTooManyRequests, // 429: explicit rate limit / quota
		http.StatusForbidden: // 403: upstream refuses this session
		return true
	}
	if ue.Kind == "bard" {
		// A body-level rejection can be a quota notice too; the prompt itself
		// being refused would also land here, but trying the next account is
		// cheap compared to returning a hard failure.
		return strings.Contains(ue.Msg, "BardErrorInfo")
	}
	return false
}

// IsCredentialError reports whether err points at a broken session for this
// account (expired XSRF token, revoked cookie), which is worth refreshing.
func IsCredentialError(err error) bool {
	ue, ok := err.(*UpstreamError)
	if !ok {
		return false
	}
	switch ue.Status {
	case http.StatusBadRequest, // 400: usually a stale `at` (XSRF) token
		http.StatusUnauthorized,
		http.StatusForbidden:
		return true
	}
	return false
}

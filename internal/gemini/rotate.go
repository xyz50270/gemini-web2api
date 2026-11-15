package gemini

import (
	"context"
"encoding/hex"
"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// rotateCookiesPageURL returns the page the browser loads to obtain a rotation
// token. og_pid 658 is the Gemini web app; rot=3 and exp_id=0 appear verbatim in
// the requests Chrome makes.
const rotateCookiesPageURL = "https://accounts.google.com/RotateCookiesPage?og_pid=658&rot=3&origin=https%3A%2F%2Fgemini.google.com&exp_id=0"

// rotateCookiesURL is the endpoint that actually refreshes SIDCC and the PSIDTS
// family.
const rotateCookiesURL = "https://accounts.google.com/RotateCookies"

// reRotateInit extracts the bootstrap call the page embeds:
//
//	init('-1026585615271262910',  658.0 ,  0.0 ,  0.0 ,  600.0 )
//
// The first argument is a *signed* 64-bit token, the second the product id and
// the last the interval in seconds after which the rotation should repeat.
var reRotateInit = regexp.MustCompile(`init\(\s*'(-?\d+)'\s*,\s*([\d.]+)\s*,\s*[\d.]+\s*,\s*[\d.]+\s*,\s*([\d.]+)`)

// rotateBootstrap is what the page told us to do.
type rotateBootstrap struct {
	Token    string
	Product  string
	Interval time.Duration
}

// fetchRotateBootstrap loads RotateCookiesPage and parses its init() call.
func fetchRotateBootstrap(ctx context.Context, req Requester, cookie string) (*rotateBootstrap, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", rotateCookiesPageURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(httpReq, cookie, "https://accounts.google.com/")

	resp, err := req.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rotate page status %d", resp.StatusCode)
	}

	m := reRotateInit.FindSubmatch(body)
	if len(m) < 4 {
		return nil, fmt.Errorf("rotate token not found in page")
	}

	pid, _ := strconv.ParseFloat(string(m[2]), 64)
	interval, _ := strconv.ParseFloat(string(m[3]), 64)

	b := &rotateBootstrap{
		Token:    string(m[1]),
		Product:  strconv.Itoa(int(pid)),
		Interval: time.Duration(interval) * time.Second,
	}
	if b.Interval <= 0 {
		b.Interval = 600 * time.Second
	}
	return b, nil
}

// RotateCookies performs one rotation cycle: fetch a fresh token, post it, and
// let the response's Set-Cookie headers flow into the jar through the wrapped
// Requester.
//
// This is what keeps the session alive when the service is idle. Passive
// rotation only happens while requests are in flight, so a quiet hour would
// still let SIDCC go stale.
func (c *Client) RotateCookies(ctx context.Context) error {
	if c.Cookies == nil {
		return nil
	}
	cookie := c.Cookies.Current()
	if cookie == "" {
		return nil
	}

	bootstrap, err := fetchRotateBootstrap(ctx, c.HTTP, cookie)
	if err != nil {
		return fmt.Errorf("rotate bootstrap: %w", err)
	}

	// The body is a two-element array: [product, "<signed token>"]. Both are
	// echoed verbatim; Chrome sends the token as a JSON string.
	payload := fmt.Sprintf(`[%s,"%s"]`, bootstrap.Product, bootstrap.Token)

	req, err := http.NewRequestWithContext(ctx, "POST", rotateCookiesURL, strings.NewReader(payload))
	if err != nil {
		return err
	}
	setBrowserHeaders(req, cookie, "https://accounts.google.com/RotateCookiesPage?og_pid=658")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://accounts.google.com")
	// Google rejects the rotation without this header (HTTP 400).
	if consistency := c.consistencyHeader(); consistency != "" {
		req.Header.Set("X-Chrome-Id-Consistency-Request", consistency)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rotate status %d", resp.StatusCode)
	}
	// Set-Cookie was already replayed into the jar by rotatingRequester.
	return nil
}

// consistencyHeader builds the x-chrome-id-consistency-request value Google
// expects: a client id, a stable device id and the account's gaia id.
func (c *Client) consistencyHeader() string {
	info, err := c.Cookies.Load()
	if err != nil {
		return ""
	}

	deviceID := c.Cfg.ChromeDeviceID
	if deviceID == "" {
		// Derive a stable id from the account so rotations keep working across
		// restarts without extra configuration.
		deviceID = stableDeviceID(info.SAPISID)
	}

	parts := []string{
		"version=2",
		"client_id=" + defaultChromeClientID,
		"device_id=" + deviceID,
	}
	if gaia := c.Cfg.GaiaID; gaia != "" {
		parts = append(parts, "sync_account_id="+gaia)
	}
	parts = append(parts, "signin_mode=all_accounts", "signout_mode=show_confirmation")
	return strings.Join(parts, ",")
}

const defaultChromeClientID = "77185425430.apps.googleusercontent.com"

// setBrowserHeaders applies the headers Chrome sends on these account requests.
func setBrowserHeaders(req *http.Request, cookie, referer string) {
	req.Header.Set("User-Agent", chromeUserAgent)
	req.Header.Set("Accept", "*/*")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

const chromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"

// stableDeviceID derives a deterministic UUID-shaped device id from the account
// so the consistency header stays valid across restarts without configuration.
func stableDeviceID(seed string) string {
	if seed == "" {
		seed = "gemini-web2api"
	}
	sum := sha1.Sum([]byte(seed))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

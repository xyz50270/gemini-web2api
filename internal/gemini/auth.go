package gemini

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type CookieInfo struct {
	Cookie    string
	SAPISID   string
	AuthUser  string
	XSRFToken string
	GeminiBL  string
}

type CookieCache struct {
	mu    sync.Mutex
	file  string
	mtime time.Time
	info  CookieInfo

	// jar mirrors the live cookie string. Google rotates SIDCC/PSIDTS on an
	// ordinary request, so replaying those Set-Cookie headers is what keeps a
	// static export from decaying within the hour.
	jar *cookieJar
}

func NewCookieCache(file string) *CookieCache {
	return &CookieCache{file: file}
}

func (c *CookieCache) Load() (CookieInfo, error) {
	if c.file == "" {
		return CookieInfo{}, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	stat, err := os.Stat(c.file)
	if err != nil {
		if c.info.Cookie != "" {
			log.Printf("Cookie load error: %v", err)
			return c.info, nil
		}
		return CookieInfo{}, err
	}

	if stat.ModTime().Equal(c.mtime) && c.info.Cookie != "" {
		return c.info, nil
	}

	contentBytes, err := os.ReadFile(c.file)
	if err != nil {
		log.Printf("Cookie load error: %v", err)
		return c.info, nil
	}

	content := strings.TrimSpace(string(contentBytes))
	var cookieStr string
	var sapisid string
	var authUser string
	var xsrfToken string
	var geminiBL string

	if strings.HasPrefix(content, "{") {
		var data struct {
			Cookie    string          `json:"cookie"`
			SAPISID   string          `json:"sapisid"`
			AuthUser  json.RawMessage `json:"auth_user"`
			XSRFToken *string         `json:"xsrf_token"`
			GeminiBL  *string         `json:"gemini_bl"`
		}
		if err := json.Unmarshal([]byte(content), &data); err == nil {
			cookieStr = data.Cookie
			sapisid = data.SAPISID
			if len(data.AuthUser) > 0 {
				u := strings.Trim(strings.TrimSpace(string(data.AuthUser)), `"`)
				if u != "null" {
					authUser = u
				}
			}
			if data.XSRFToken != nil {
				xsrfToken = *data.XSRFToken
			}
			if data.GeminiBL != nil {
				geminiBL = *data.GeminiBL
			}
		}
	} else {
		cookieStr = content
	}

	if sapisid == "" && cookieStr != "" {
		pairs := strings.Split(cookieStr, "; ")
		for _, pair := range pairs {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 && parts[0] == "SAPISID" {
				sapisid = parts[1]
				break
			}
		}
	}

	c.mtime = stat.ModTime()
	c.info = CookieInfo{
		Cookie:    cookieStr,
		SAPISID:   sapisid,
		AuthUser:  authUser,
		XSRFToken: xsrfToken,
		GeminiBL:  geminiBL,
	}

	// Rebuild the jar from the file on every reload so an operator editing the
	// file by hand still wins over anything the service had accumulated.
	if c.jar == nil {
		c.jar = newCookieJar(cookieStr)
		c.jar.onFlush = c.persistJar
	} else {
		c.jar = newCookieJar(cookieStr)
		c.jar.onFlush = c.persistJar
	}

	return c.info, nil
}

// Current returns the live cookie header value, which includes any rotations the
// server has applied since the file was read.
func (c *CookieCache) Current() string {
	c.mu.Lock()
	jar := c.jar
	info := c.info
	c.mu.Unlock()

	if jar != nil {
		if s := jar.String(); s != "" {
			return s
		}
	}
	return info.Cookie
}

// noteResponse feeds a response's Set-Cookie headers into the live jar.
func (c *CookieCache) noteResponse(resp *http.Response) {
	c.mu.Lock()
	jar := c.jar
	if jar == nil && c.info.Cookie != "" {
		jar = newCookieJar(c.info.Cookie)
		jar.onFlush = c.persistJar
		c.jar = jar
	}
	c.mu.Unlock()

	if jar != nil {
		jar.applySetCookie(resp)
	}
}

// persistJar writes the rotated cookie string back to the file so a restart
// picks up where it left off.
func (c *CookieCache) persistJar(raw string) {
	if c.file == "" || raw == "" {
		return
	}

	c.mu.Lock()
	info := c.info
	info.Cookie = raw
	c.info = info
	file := c.file
	c.mu.Unlock()

	payload := struct {
		Cookie    string  `json:"cookie"`
		SAPISID   string  `json:"sapisid"`
		AuthUser  *string `json:"auth_user"`
		XSRFToken *string `json:"xsrf_token"`
		GeminiBL  *string `json:"gemini_bl"`
	}{
		Cookie:  raw,
		SAPISID: info.SAPISID,
	}
	if info.AuthUser != "" {
		v := info.AuthUser
		payload.AuthUser = &v
	}
	if info.XSRFToken != "" {
		v := info.XSRFToken
		payload.XSRFToken = &v
	}
	if info.GeminiBL != "" {
		v := info.GeminiBL
		payload.GeminiBL = &v
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("cookie rotate: marshal failed: %v", err)
		return
	}

	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("cookie rotate: write failed: %v", err)
		return
	}
	// WriteFile only applies the mode when creating; an existing temp file keeps
	// its old permissions, and on some platforms the mode is ignored entirely.
	if err := os.Chmod(tmp, 0o600); err != nil {
		log.Printf("cookie rotate: chmod failed: %v", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		log.Printf("cookie rotate: rename failed: %v", err)
		return
	}
	_ = os.Chmod(file, 0o600)
	// Keep the cached mtime in sync so the next Load does not re-read and clobber
	// the rotation we just persisted.
	if st, err := os.Stat(file); err == nil {
		c.mu.Lock()
		c.mtime = st.ModTime()
		c.mu.Unlock()
	}
}

// JarStats reports rotations applied so far, for logging.
func (c *CookieCache) JarStats() (updates, names int) {
	c.mu.Lock()
	jar := c.jar
	c.mu.Unlock()
	if jar == nil {
		return 0, 0
	}
	return jar.Stats()
}

func SAPISIDHash(sapisid string) string {
	if sapisid == "" {
		return ""
	}
	ts := time.Now().Unix()
	input := fmt.Sprintf("%d %s https://gemini.google.com", ts, sapisid)
	h := sha1.Sum([]byte(input))
	return fmt.Sprintf("SAPISIDHASH %d_%s", ts, hex.EncodeToString(h[:]))
}

func AccountPrefix(authUser string) string {
	if authUser == "" {
		return ""
	}
	return "/u/" + authUser
}

var (
	reSNlM0e = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)
	reFdrFJe = regexp.MustCompile(`"FdrFJe":"([^"]+)"`)
)

// FetchPageAuth logs into the Gemini app page for one account and extracts the
// tokens the StreamGenerate endpoint needs: SNlM0e (sent as the `at` form field)
// and FdrFJe (sent as `f.sid`). The XSRF token rotates, so this is called again
// whenever a request comes back with a 400.
func FetchPageAuth(req Requester, info CookieInfo, authUser string) (xsrfToken, fSid string, err error) {
	if req == nil {
		return "", "", fmt.Errorf("no requester configured")
	}

	pageURL := fmt.Sprintf("https://gemini.google.com%s/app", AccountPrefix(authUser))
	httpReq, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	if info.Cookie != "" {
		httpReq.Header.Set("Cookie", info.Cookie)
	}
	if authUser != "" {
		httpReq.Header.Set("X-Goog-AuthUser", authUser)
	}

	resp, err := req.Do(httpReq)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("app page status %d", resp.StatusCode)
	}

	if m := reSNlM0e.FindSubmatch(body); len(m) > 1 {
		xsrfToken = string(m[1])
	}
	if m := reFdrFJe.FindSubmatch(body); len(m) > 1 {
		fSid = string(m[1])
	}
	if xsrfToken == "" {
		return "", "", fmt.Errorf("SNlM0e not found in app page")
	}
	return xsrfToken, fSid, nil
}

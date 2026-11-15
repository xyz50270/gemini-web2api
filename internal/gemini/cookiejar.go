package gemini

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// cookieJar keeps the live cookie string in sync with what Google sends back.
//
// Google rotates SIDCC and the PSIDTS family continuously: every response may
// carry fresh values, and the old ones stop being accepted. A static snapshot
// copied out of a browser therefore decays within an hour or so. Replaying the
// Set-Cookie headers the service already receives keeps the session alive
// without asking the operator to re-export anything.
//
// Only cookies the server actually re-issues are updated; everything else in the
// original export is preserved verbatim, because some of them (notably
// __Secure-1PSID) never appear in Set-Cookie and would be lost if the jar were
// rebuilt from responses alone.
type cookieJar struct {
	mu      sync.RWMutex
	order   []string          // stable ordering, taken from the original string
	values  map[string]string // name -> value
	dirty   bool              // something changed since the last flush
	updates int               // how many values the server has refreshed

	// onFlush is called (outside the lock) whenever the jar has new values.
	onFlush func(raw string)
}

// newCookieJar parses a cookie header value such as "A=1; B=2".
func newCookieJar(raw string) *cookieJar {
	j := &cookieJar{values: make(map[string]string)}
	j.merge(raw)
	return j
}

// String renders the jar back to a Cookie header value.
func (j *cookieJar) String() string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.renderLocked()
}

func (j *cookieJar) renderLocked() string {
	parts := make([]string, 0, len(j.values))
	for _, name := range j.order {
		if v, ok := j.values[name]; ok {
			parts = append(parts, name+"="+v)
		}
	}
	// Names that arrived after the initial parse go last, alphabetically, so the
	// output stays deterministic.
	var extra []string
	for name := range j.values {
		found := false
		for _, o := range j.order {
			if o == name {
				found = true
				break
			}
		}
		if !found {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		parts = append(parts, name+"="+j.values[name])
	}
	return strings.Join(parts, "; ")
}

// merge folds a cookie header value into the jar, keeping the first-seen order.
func (j *cookieJar) merge(raw string) {
	if raw == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, pair := range strings.Split(raw, "; ") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			continue
		}
		if _, seen := j.values[name]; !seen {
			j.order = append(j.order, name)
		}
		j.values[name] = value
	}
}

// applySetCookie updates the jar from a response's Set-Cookie headers.
//
// It reports whether anything changed, so callers can avoid pointless writes.
// Cookies marked for deletion (an expiry in the past, or an empty value with
// "EXPIRED") are dropped, matching browser behaviour.
func (j *cookieJar) applySetCookie(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	raws := resp.Header.Values("Set-Cookie")
	if len(raws) == 0 {
		return false
	}

	var changed []string
	j.mu.Lock()
	for _, raw := range raws {
		name, value, expired := parseSetCookie(raw)
		if name == "" {
			continue
		}
		// Only touch cookies that are already part of the session, or that
		// Google explicitly issues for this account. Blindly adopting every
		// unnamed tracking cookie would bloat the header for no benefit.
		_, known := j.values[name]
		if !known && !isSessionCookie(name) {
			continue
		}
		if expired {
			if _, ok := j.values[name]; ok {
				delete(j.values, name)
				changed = append(changed, name)
			}
			continue
		}
		if j.values[name] == value {
			continue
		}
		if !known {
			j.order = append(j.order, name)
		}
		j.values[name] = value
		changed = append(changed, name)
	}
	if len(changed) == 0 {
		j.mu.Unlock()
		return false
	}
	j.dirty = true
	j.updates += len(changed)
	raw := j.renderLocked()
	flush := j.onFlush
	j.mu.Unlock()

	if flush != nil {
		flush(raw)
	}
	return true
}

// parseSetCookie extracts the name, value and expiry state from one header.
func parseSetCookie(raw string) (name, value string, expired bool) {
	first := raw
	if idx := strings.IndexByte(raw, ';'); idx >= 0 {
		first = raw[:idx]
	}
	n, v, ok := strings.Cut(strings.TrimSpace(first), "=")
	if !ok || n == "" {
		return "", "", false
	}
	name = strings.TrimSpace(n)
	value = v
	if value == "" || value == "EXPIRED" || strings.HasPrefix(value, "EXPIRED") {
		return name, "", true
	}
	// An explicit past expiry also means deletion.
	lower := strings.ToLower(raw)
	if idx := strings.Index(lower, "expires="); idx >= 0 {
		rest := raw[idx+len("expires="):]
		if semi := strings.IndexByte(rest, ';'); semi >= 0 {
			rest = rest[:semi]
		}
		if t, err := http.ParseTime(strings.TrimSpace(rest)); err == nil && t.Before(timeNow()) {
			return name, "", true
		}
	}
	return name, value, false
}

// isSessionCookie reports whether a name belongs to the Google session set that
// is worth keeping even when it was not in the original export.
func isSessionCookie(name string) bool {
	switch name {
	case "SID", "HSID", "SSID", "APISID", "SAPISID",
		"SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC",
		"__Secure-1PSIDTS", "__Secure-3PSIDTS",
		"__Secure-1PSID", "__Secure-3PSID", "OSID", "__Secure-OSID",
		"COMPASS", "ACCOUNT_CHOOSER", "LSID":
		return true
	}
	return false
}

// Stats reports how many values the server has refreshed so far.
func (j *cookieJar) Stats() (updates int, names int) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.updates, len(j.values)
}

// timeNow is a seam for tests.
var timeNow = func() time.Time { return time.Now() }

// rotatingRequester wraps a Requester so every response feeds the cookie jar.
//
// Doing it here rather than at each call site means no request path can forget
// to record a rotation: BL fetch, discovery, StreamGenerate and image upload all
// go through the same Do.
type rotatingRequester struct {
	inner Requester
	cache *CookieCache
	onRotate func(updates int)
}

func newRotatingRequester(inner Requester, cache *CookieCache) *rotatingRequester {
	return &rotatingRequester{inner: inner, cache: cache}
}

func (r *rotatingRequester) Do(req *http.Request) (*http.Response, error) {
	resp, err := r.inner.Do(req)
	if err != nil || resp == nil || r.cache == nil {
		return resp, err
	}

	before, _ := r.cache.JarStats()
	r.cache.noteResponse(resp)
	after, names := r.cache.JarStats()

	if after > before && r.onRotate != nil {
		r.onRotate(after - before)
		_ = names
	}
	return resp, err
}

// NewRotatingRequester exposes the wrapper for callers that need to substitute
// the underlying requester (the account pool shares one transport) without
// losing Set-Cookie capture.
func NewRotatingRequester(inner Requester, cache *CookieCache) Requester {
	return newRotatingRequester(inner, cache)
}

// Unwrap exposes the wrapped requester so callers (and tests) can inspect the
// underlying transport, e.g. to assert the proxy is configured.
func (r *rotatingRequester) Unwrap() Requester { return r.inner }

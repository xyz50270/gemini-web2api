package gemini

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const sampleCookie = "SID=abc; HSID=def; SSID=ghi; APISID=jkl; SAPISID=mno; __Secure-1PSID=pqr"

func TestCookieJarParsesAndRenders(t *testing.T) {
	j := newCookieJar(sampleCookie)
	got := j.String()
	if got != sampleCookie {
		t.Errorf("round-trip = %q, want %q", got, sampleCookie)
	}
}

func TestCookieJarAppliesRotation(t *testing.T) {
	j := newCookieJar(sampleCookie + "; SIDCC=OLD")

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "SIDCC=NEW; expires=Mon, 15-Nov-2027 15:34:45 GMT; path=/; domain=.google.com")
	if !j.applySetCookie(resp) {
		t.Fatal("applySetCookie should report a change")
	}

	got := j.String()
	if !strings.Contains(got, "SIDCC=NEW") {
		t.Errorf("SIDCC not rotated: %q", got)
	}
	if strings.Contains(got, "SIDCC=OLD") {
		t.Errorf("old SIDCC still present: %q", got)
	}
	// Untouched cookies must survive verbatim: they never appear in Set-Cookie.
	for _, name := range []string{"SID=abc", "SAPISID=mno", "__Secure-1PSID=pqr"} {
		if !strings.Contains(got, name) {
			t.Errorf("%s lost during rotation: %q", name, got)
		}
	}
}

func TestCookieJarIgnoresUnchangedValue(t *testing.T) {
	j := newCookieJar(sampleCookie + "; SIDCC=SAME")
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "SIDCC=SAME; path=/")
	if j.applySetCookie(resp) {
		t.Error("identical value should not count as a change")
	}
}

func TestCookieJarDropsExpiredCookie(t *testing.T) {
	j := newCookieJar(sampleCookie + "; __Secure-1PSIDCC=OLDPCC")
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "__Secure-1PSIDCC=EXPIRED; expires=Mon, 01-Jan-1990 00:00:00 GMT; path=/")
	if !j.applySetCookie(resp) {
		t.Fatal("expiry should be reported as a change")
	}
	if strings.Contains(j.String(), "__Secure-1PSIDCC") {
		t.Errorf("expired cookie still present: %q", j.String())
	}
}

func TestCookieJarAdoptsNewSessionCookie(t *testing.T) {
	j := newCookieJar(sampleCookie)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "__Secure-1PSIDTS=sidts-NEW; path=/; Secure")
	j.applySetCookie(resp)

	if !strings.Contains(j.String(), "__Secure-1PSIDTS=sidts-NEW") {
		t.Errorf("new session cookie not adopted: %q", j.String())
	}
}

func TestCookieJarIgnoresTrackingCookies(t *testing.T) {
	j := newCookieJar(sampleCookie)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "_ga=GA1.2.3; path=/")
	resp.Header.Add("Set-Cookie", "some_random_tracker=xyz; path=/")
	j.applySetCookie(resp)

	got := j.String()
	if strings.Contains(got, "_ga") || strings.Contains(got, "some_random_tracker") {
		t.Errorf("tracking cookies should be ignored: %q", got)
	}
}

func TestCookieJarOrderIsStable(t *testing.T) {
	j := newCookieJar("Z=1; A=2; M=3")
	first := j.String()

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "A=9; path=/")
	j.applySetCookie(resp)

	if !strings.HasPrefix(j.String(), "Z=1; A=9; M=3") {
		t.Errorf("original order not preserved: %q", j.String())
	}
	if first == j.String() {
		t.Error("value should have changed")
	}
}

// The whole point: a rotated cookie must be written back so a restart resumes
// with the live session instead of the stale export.
func TestCookieCachePersistsRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookie.json")
	seed := `{"cookie":"` + sampleCookie + `; SIDCC=OLD","sapisid":"mno"}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	cache := NewCookieCache(path)
	if _, err := cache.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "SIDCC=ROTATED; expires=Mon, 15-Nov-2027 15:34:45 GMT; path=/")
	cache.noteResponse(resp)

	if live := cache.Current(); !strings.Contains(live, "SIDCC=ROTATED") {
		t.Fatalf("Current() = %q, want the rotated value", live)
	}

	// The file must be updated too.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "SIDCC=ROTATED") {
		t.Errorf("persisted file missing rotation: %s", data)
	}
	if !strings.Contains(string(data), `"sapisid": "mno"`) {
		t.Errorf("sapisid lost on persist: %s", data)
	}
	// Permissions must stay private on POSIX. Windows does not map UNIX mode
	// bits onto NTFS, so the check only applies where the semantics exist.
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err == nil {
			if perm := st.Mode().Perm(); perm != 0o600 {
				t.Errorf("perm = %o, want 600", perm)
			}
		}
	}

	// A fresh cache over the same file resumes from the rotated value.
	cache2 := NewCookieCache(path)
	info, err := cache2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Cookie, "SIDCC=ROTATED") {
		t.Errorf("reloaded cookie = %q", info.Cookie)
	}
}

func TestCookieCacheHandEditWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookie.json")

	write := func(ck string) {
		if err := os.WriteFile(path, []byte(`{"cookie":"`+ck+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		// Ensure a distinct mtime so the mtime-cache does not short-circuit.
		future := time.Now().Add(2 * time.Second)
		_ = os.Chtimes(path, future, future)
	}

	write("SID=v1; SIDCC=C1")
	cache := NewCookieCache(path)
	cache.Load()

	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "SIDCC=C2; expires=Mon, 15-Nov-2027 15:34:45 GMT; path=/")
	cache.noteResponse(resp)
	if !strings.Contains(cache.Current(), "SIDCC=C2") {
		t.Fatal("rotation not applied")
	}

	// Operator replaces the file by hand: their value must win.
	write("SID=v2; SIDCC=MANUAL")
	info, err := cache.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(info.Cookie, "SIDCC=MANUAL") {
		t.Errorf("hand edit ignored: %q", info.Cookie)
	}
	if !strings.Contains(cache.Current(), "SIDCC=MANUAL") {
		t.Errorf("Current() should follow the file: %q", cache.Current())
	}
}

func TestRotatingRequesterCapturesSetCookie(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookie.json")
	if err := os.WriteFile(path, []byte(`{"cookie":"`+sampleCookie+`; SIDCC=OLD"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewCookieCache(path)
	cache.Load()

	inner := &rotStub{
		body:       "ok",
		setCookies: []string{"SIDCC=FRESH; expires=Mon, 15-Nov-2027 15:34:45 GMT; path=/"},
	}

	rot := NewRotatingRequester(inner, cache)
	req, _ := http.NewRequest("GET", "https://gemini.google.com/app", nil)
	if _, err := rot.Do(req); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if !strings.Contains(cache.Current(), "SIDCC=FRESH") {
		t.Errorf("requester did not feed the jar: %q", cache.Current())
	}
}

// stubRequester is shared with the other gemini tests; this variant adds the
// Set-Cookie plumbing the rotation tests need.
type rotStub struct {
	status     int
	body       string
	setCookies []string
}

func (s *rotStub) Do(req *http.Request) (*http.Response, error) {
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	h := make(http.Header)
	for _, c := range s.setCookies {
		h.Add("Set-Cookie", c)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     h,
		Request:    req,
	}, nil
}

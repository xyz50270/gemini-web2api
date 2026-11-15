package gemini

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

// rotPage is the real bootstrap payload, including the negative token.
const rotPage = `<!DOCTYPE html><html><body><script src='https://www.gstatic.com/_/mss/boq-identity/_/js/k=boq-identity.IdentityRotateCookiesHttp.en.abc/d=1/rs=xyz/m=hfcr' nonce="abc"></script><script nonce="abc">init('-1026585615271262910',  658.0 ,  0.0 ,  0.0 ,  600.0 )</script></body></html>`

type rotateStub struct {
	page       string
	pageCode   int
	postCode   int
	setCookies []string
	posts      []string
	gets       int
}

func (s *rotateStub) Do(req *http.Request) (*http.Response, error) {
	h := make(http.Header)
	code := s.pageCode
	if code == 0 {
		code = http.StatusOK
	}

	if req.Method == http.MethodGet {
		s.gets++
		body := s.page
		for _, c := range s.setCookies {
			h.Add("Set-Cookie", c)
		}
		return &http.Response{StatusCode: code, Status: http.StatusText(code),
			Body: io.NopCloser(strings.NewReader(body)), Header: h, Request: req}, nil
	}

	buf, _ := io.ReadAll(req.Body)
	s.posts = append(s.posts, string(buf))
	if s.postCode == 0 {
		s.postCode = http.StatusOK
	}
	for _, c := range s.setCookies {
		h.Add("Set-Cookie", c)
	}
	return &http.Response{StatusCode: s.postCode, Status: http.StatusText(s.postCode),
		Body: io.NopCloser(strings.NewReader(`[["identity.hfcr",600]]`)), Header: h, Request: req}, nil
}

func TestFetchRotateBootstrap(t *testing.T) {
	stub := &rotateStub{page: rotPage}
	b, err := fetchRotateBootstrap(context.Background(), stub, "SID=x")
	if err != nil {
		t.Fatalf("fetchRotateBootstrap: %v", err)
	}
	if b.Token != "-1026585615271262910" {
		t.Errorf("Token = %q", b.Token)
	}
	if b.Product != "658" {
		t.Errorf("Product = %q", b.Product)
	}
	if b.Interval != 600*1e9 {
		t.Errorf("Interval = %v, want 600s", b.Interval)
	}
}

func TestFetchRotateBootstrapMissingToken(t *testing.T) {
	stub := &rotateStub{page: "<html>no init here</html>"}
	if _, err := fetchRotateBootstrap(context.Background(), stub, "SID=x"); err == nil {
		t.Error("expected an error when the page has no init() call")
	}
}

func TestRotateCookiesPostsSignedToken(t *testing.T) {
	stub := &rotateStub{page: rotPage}
	c := &Client{Cfg: configDefaultForRotate(), HTTP: stub, Cookies: NewCookieCache("")}
	c.Cookies.jar = newCookieJar("SID=x; SAPISID=y; SIDCC=OLD")

	if err := c.RotateCookies(context.Background()); err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	if len(stub.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(stub.posts))
	}
	// The signed token must survive verbatim as a JSON string.
	if !strings.Contains(stub.posts[0], `"-1026585615271262910"`) {
		t.Errorf("payload = %s, want the signed token quoted", stub.posts[0])
	}
	if !strings.HasPrefix(stub.posts[0], "[658,") {
		t.Errorf("payload = %s, want it to start with the product id", stub.posts[0])
	}
}

func TestRotateCookiesRequiresConsistencyHeader(t *testing.T) {
	stub := &headerCapturingStub{page: rotPage, setCookies: []string{"SIDCC=NEW; path=/"}}
	c := &Client{Cfg: configDefaultForRotate(), HTTP: newRotatingRequester(stub, nil), Cookies: NewCookieCache("")}
	c.Cookies.jar = newCookieJar("SID=x")
	c.HTTP = newRotatingRequester(stub, c.Cookies)

	if err := c.RotateCookies(context.Background()); err != nil {
		t.Fatalf("RotateCookies: %v", err)
	}
	if stub.postHeader.Get("X-Chrome-Id-Consistency-Request") == "" {
		t.Error("rotation must send x-chrome-id-consistency-request; Google returns 400 without it")
	}
}

func TestRotateCookiesNoCookie(t *testing.T) {
	c := &Client{Cfg: configDefaultForRotate(), Cookies: NewCookieCache("")}
	if err := c.RotateCookies(context.Background()); err != nil {
		t.Errorf("an empty cookie should be a no-op, got %v", err)
	}
}

func TestStableDeviceIDDeterministic(t *testing.T) {
	a := stableDeviceID("sapisid-abc")
	b := stableDeviceID("sapisid-abc")
	if a != b {
		t.Error("device id must be stable for the same seed")
	}
	if a == stableDeviceID("other") {
		t.Error("different accounts should get different device ids")
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Errorf("device id %q is not UUID shaped", a)
	}
}

func configDefaultForRotate() config.Config {
	cfg := config.Default()
	cfg.LogRequests = false
	return cfg
}

// headerCapturingStub records headers from the POST.
type headerCapturingStub struct {
	page       string
	setCookies []string
	postHeader http.Header
}

func (s *headerCapturingStub) Do(req *http.Request) (*http.Response, error) {
	h := make(http.Header)
	if req.Method == http.MethodGet {
		for _, c := range s.setCookies {
			h.Add("Set-Cookie", c)
		}
		return &http.Response{StatusCode: 200, Status: "200 OK",
			Body: io.NopCloser(strings.NewReader(s.page)), Header: h, Request: req}, nil
	}
	s.postHeader = req.Header.Clone()
	for _, c := range s.setCookies {
		h.Add("Set-Cookie", c)
	}
	return &http.Response{StatusCode: 200, Status: "200 OK",
		Body: io.NopCloser(strings.NewReader("ok")), Header: h, Request: req}, nil
}

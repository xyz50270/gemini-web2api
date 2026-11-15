package gemini

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

type stubRequester struct {
	lastReq *http.Request
	body    string
	err     error
}

func (s *stubRequester) Do(req *http.Request) (*http.Response, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

func TestFetchLatestBLUsesRequester(t *testing.T) {
	stub := &stubRequester{body: `x boq_assistant-bard-web-server_20260716.08_p0 y`}
	got, err := FetchLatestBL(stub)
	if err != nil {
		t.Fatalf("FetchLatestBL error: %v", err)
	}
	if got != "boq_assistant-bard-web-server_20260716.08_p0" {
		t.Errorf("FetchLatestBL = %q, want BL", got)
	}
	if stub.lastReq == nil || stub.lastReq.URL.Host != "gemini.google.com" {
		t.Errorf("FetchLatestBL did not use the provided Requester, req=%v", stub.lastReq)
	}
	if _, ok := stub.lastReq.Context().Deadline(); !ok {
		t.Error("FetchLatestBL request should carry a timeout deadline")
	}
}

func TestBLRegex(t *testing.T) {
	tests := []struct {
		name  string
		html  string
		want  string
		found bool
	}{
		{
			name:  "new prefix uiserver",
			html:  `"cfb2h":"boq_gemini-web-uiserver_20261113.11_p0"`,
			want:  "boq_gemini-web-uiserver_20261113.11_p0",
			found: true,
		},
		{
			name:  "legacy prefix still accepted",
			html:  `some garbage boq_assistant-bard-web-server_20260716.08_p0 more garbage`,
			want:  "boq_assistant-bard-web-server_20260716.08_p0",
			found: true,
		},
		{
			name:  "not found",
			html:  `no matching pattern here`,
			want:  "",
			found: false,
		},
		{
			name:  "multiple matches returns first",
			html:  `boq_assistant-bard-web-server_20260101.01_p1 and boq_assistant-bard-web-server_20260716.08_p0`,
			want:  "boq_assistant-bard-web-server_20260101.01_p1",
			found: true,
		},
		{
			name:  "rejects malformed date",
			html:  `boq_gemini-web-uiserver_2026.11_p0`,
			want:  "",
			found: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reBL.FindString(tt.html)
			if tt.found {
				if got != tt.want {
					t.Errorf("reBL.FindString() = %q, want %q", got, tt.want)
				}
			} else {
				if got != "" {
					t.Errorf("reBL.FindString() = %q, want empty", got)
				}
			}
		})
	}
}

func TestExtractBLPrefersCfb2h(t *testing.T) {
	// A decoy build id appears before the canonical cfb2h binding; the latter
	// is what the frontend actually uses, so it must win.
	html := `{"some":"boq_other-service_20260101.01_p0","cfb2h":"boq_gemini-web-uiserver_20261113.11_p0"}`
	got, err := ExtractBL(html)
	if err != nil {
		t.Fatalf("ExtractBL: %v", err)
	}
	if want := "boq_gemini-web-uiserver_20261113.11_p0"; got != want {
		t.Errorf("ExtractBL = %q, want %q", got, want)
	}
}

func TestExtractBLFallsBackToFirstMatch(t *testing.T) {
	html := `nothing canonical here boq_gemini-web-uiserver_20261113.11_p0 tail`
	got, err := ExtractBL(html)
	if err != nil {
		t.Fatalf("ExtractBL: %v", err)
	}
	if want := "boq_gemini-web-uiserver_20261113.11_p0"; got != want {
		t.Errorf("ExtractBL = %q, want %q", got, want)
	}
}

func TestExtractBLMissing(t *testing.T) {
	if _, err := ExtractBL("<html>no build id</html>"); err == nil {
		t.Error("expected error when no BL is present")
	}
}

func TestIsValidBL(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"boq_gemini-web-uiserver_20261113.11_p0", true},
		{"boq_assistant-bard-web-server_20260716.08_p0", true},
		{"", false},
		{"boq_gemini-web-uiserver", false},
		{"garbage", false},
		{"prefix boq_gemini-web-uiserver_20261113.11_p0 suffix", false},
	}
	for _, tt := range tests {
		if got := IsValidBL(tt.in); got != tt.want {
			t.Errorf("IsValidBL(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFetchLatestBLNewFormat(t *testing.T) {
	stub := &stubRequester{
		body: `...,"cfb2h":"boq_gemini-web-uiserver_20261113.11_p0","d2zJAe":"AIza..."`,
	}
	got, err := FetchLatestBL(stub)
	if err != nil {
		t.Fatalf("FetchLatestBL error: %v", err)
	}
	if want := "boq_gemini-web-uiserver_20261113.11_p0"; got != want {
		t.Errorf("FetchLatestBL = %q, want %q", got, want)
	}
}

func TestCurrentBL(t *testing.T) {
	c := &Client{
		bl: "mutable-bl",
		Cfg: config.Config{
			GeminiBL: "config-bl",
		},
	}
	if got := c.CurrentBL(); got != "mutable-bl" {
		t.Errorf("CurrentBL() = %q, want %q", got, "mutable-bl")
	}

	c2 := &Client{
		Cfg: config.Config{
			GeminiBL: "config-bl",
		},
	}
	if got := c2.CurrentBL(); got != "config-bl" {
		t.Errorf("CurrentBL() empty bl = %q, want %q", got, "config-bl")
	}
}

func TestSetBL(t *testing.T) {
	c := &Client{
		Cfg: config.Config{
			GeminiBL: "old",
		},
	}
	c.SetBL("new")
	if got := c.CurrentBL(); got != "new" {
		t.Errorf("After SetBL, CurrentBL() = %q, want %q", got, "new")
	}
}

func TestUpdateBLIfNeededDetectsChange(t *testing.T) {
	stub := &stubRequester{body: `"cfb2h":"boq_gemini-web-uiserver_20261113.11_p0"`}
	c := NewClient(config.Default())
	c.HTTP = stub
	c.SetBL("boq_gemini-web-uiserver_20260101.01_p0")

	newBL, changed, err := c.UpdateBLIfNeeded()
	if err != nil {
		t.Fatalf("UpdateBLIfNeeded: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true when the page advertises a different build")
	}
	if want := "boq_gemini-web-uiserver_20261113.11_p0"; newBL != want {
		t.Errorf("newBL = %q, want %q", newBL, want)
	}
	if got := c.CurrentBL(); got != newBL {
		t.Errorf("CurrentBL = %q, want %q after update", got, newBL)
	}
}

func TestUpdateBLIfNeededNoChange(t *testing.T) {
	const bl = "boq_gemini-web-uiserver_20261113.11_p0"
	stub := &stubRequester{body: `"cfb2h":"` + bl + `"`}
	c := NewClient(config.Default())
	c.HTTP = stub
	c.SetBL(bl)

	newBL, changed, err := c.UpdateBLIfNeeded()
	if err != nil {
		t.Fatalf("UpdateBLIfNeeded: %v", err)
	}
	if changed {
		t.Error("changed = true, want false when the build id is identical")
	}
	if newBL != bl {
		t.Errorf("newBL = %q, want %q", newBL, bl)
	}
}

func TestUpdateBLIfNeededRejectsGarbage(t *testing.T) {
	stub := &stubRequester{body: `no build id in this page at all`}
	c := NewClient(config.Default())
	c.HTTP = stub
	before := c.CurrentBL()

	if _, _, err := c.UpdateBLIfNeeded(); err == nil {
		t.Fatal("expected an error when the page carries no build id")
	}
	if got := c.CurrentBL(); got != before {
		t.Errorf("CurrentBL changed to %q despite a failed refresh", got)
	}
}

func TestUpdateBLRecoversFromLegacyValue(t *testing.T) {
	// Regression: the shipped default used to be a legacy-format id, and the
	// old prefix-pinned regex could never see the new one, so this call failed
	// on every boot and the 405 recovery path stayed dead.
	stub := &stubRequester{
		body: `...,"cfb2h":"boq_gemini-web-uiserver_20261113.11_p0","d2zJAe":"AIza..."`,
	}
	c := NewClient(config.Default())
	c.HTTP = stub
	c.SetBL("boq_assistant-bard-web-server_20260716.08_p0")

	newBL, changed, err := c.UpdateBLIfNeeded()
	if err != nil {
		t.Fatalf("UpdateBLIfNeeded failed to parse the new build id: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true when upgrading from the legacy id")
	}
	if !IsValidBL(newBL) {
		t.Errorf("newBL %q is not a valid build id", newBL)
	}
}

package gemini

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// reBL matches a Gemini frontend build id as embedded in the app page, e.g.
//   "cfb2h":"boq_gemini-web-uiserver_20261113.11_p0"
//
// Google has used more than one prefix over time
// (boq_gemini-web-uiserver_, boq_assistant-bard-web-server_), so the regex is
// deliberately prefix-agnostic: it accepts any boq_<name>_<date>.<rev>_p<n>.
// Pinning a single prefix made BL auto-refresh silently stop matching once the
// frontend renamed the build, which also disabled the 405 recovery path.
var reBL = regexp.MustCompile(`boq_[A-Za-z0-9-]+_\d{8}\.\d+_p\d+`)

// FetchLatestBL fetches the latest BL identifier from the Gemini frontend page.
// It issues the GET through the provided Requester so proxy/TLS configuration
// is honored, with a 15s context timeout to avoid blocking startup too long,
// then extracts the first boq_ build id from the HTML.
//
// The page is fetched anonymously on purpose: the build id is the same for every
// account, so no cookie is needed and the request stays cheap.
func FetchLatestBL(client Requester) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "https://gemini.google.com/app", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("BL fetch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("BL fetch read failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("BL fetch status %d", resp.StatusCode)
	}

	return ExtractBL(string(bodyBytes))
}

// ExtractBL returns the build id found in page HTML, preferring the value bound
// to the `cfb2h` key the frontend itself uses.
func ExtractBL(html string) (string, error) {
	if m := reBL.FindString(html); m != "" {
		// Prefer the canonical "cfb2h":"<bl>" occurrence when present, but the
		// generic match is already the same value in practice.
		idx := strings.Index(html, `"cfb2h":"`)
		if idx >= 0 {
			rest := html[idx+len(`"cfb2h":"`):]
			if end := strings.Index(rest, `"`); end > 0 {
				candidate := rest[:end]
				if reBL.FindString(candidate) == candidate {
					return candidate, nil
				}
			}
		}
		return m, nil
	}
	return "", fmt.Errorf("BL pattern not found in response")
}

// IsValidBL reports whether s looks like a Gemini build id.
func IsValidBL(s string) bool {
	return s != "" && reBL.FindString(s) == s
}

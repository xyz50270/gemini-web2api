package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Pool type identifiers returned by the usage RPC.
const (
	PoolFiveHourly = "GXU_FIVE_HOURLY"
	PoolWeekly     = "GXU_WEEKLY"
)

// quotaQuerySignature and quotaQueryVariables are the parameters the Gemini web
// frontend sends for the usage-settings screen. They are stable for this build;
// if the screen starts returning 400 the pair below needs refreshing.
const (
	quotaQuerySignature = "2/r9A7h8PGYFROeibFhACp9h3mHf72UWS3JhgYaQYlPoo="
	quotaQueryVariables = `{"g1Request":{"entrypoints":["GEMINI_USAGE_SETTINGS_UPGRADE_BANNER","GEMINI_USAGE_SETTINGS_AI_CREDITS_UPSELL_CARD","GEMINI_USAGE_SETTINGS_UPGRADE_AND_CREDITS_BANNER"]},"workspaceRequest":{"recommendationsForSurfaceRequests":[{"recommendationSurface":{"rpOnramp":"GEMINI_CHAT_USAGE_LIMITS_UPSELL","version":"1"},"maxRecommendationCount":1}]}}`
)

// QuotaPool is one usage bucket (per-5-hour or weekly).
type QuotaPool struct {
	PoolType          string     `json:"poolType"`
	PercentageUsed    float64    `json:"percentageUsed"`
	WindowRefreshesAt *time.Time `json:"windowRefreshesAt,omitempty"`
}

// ResetsIn returns the remaining time until the window refreshes, or 0 when
// the upstream did not report a refresh time.
func (p *QuotaPool) ResetsIn(now time.Time) time.Duration {
	if p == nil || p.WindowRefreshesAt == nil {
		return 0
	}
	d := p.WindowRefreshesAt.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// Quota is the usage state of one account.
type Quota struct {
	Tier      string     `json:"tier"`
	FiveHour  *QuotaPool `json:"five_hour,omitempty"`
	Weekly    *QuotaPool `json:"weekly,omitempty"`
	FetchedAt time.Time  `json:"fetched_at"`
}

// MaxUsed returns the highest utilisation across all reported pools (0..1).
func (q *Quota) MaxUsed() float64 {
	if q == nil {
		return 0
	}
	max := 0.0
	for _, p := range []*QuotaPool{q.FiveHour, q.Weekly} {
		if p != nil && p.PercentageUsed > max {
			max = p.PercentageUsed
		}
	}
	return max
}

// NextReset returns the nearest window refresh across all pools.
func (q *Quota) NextReset() (time.Time, bool) {
	if q == nil {
		return time.Time{}, false
	}
	var best time.Time
	for _, p := range []*QuotaPool{q.FiveHour, q.Weekly} {
		if p == nil || p.WindowRefreshesAt == nil {
			continue
		}
		t := *p.WindowRefreshesAt
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	return best, !best.IsZero()
}

// usageResponse mirrors the GraphQL payload. The top level is a one-element
// array wrapper, hence the outer slice in the caller.
type usageResponse struct {
	Data struct {
		Viewer struct {
			Rpcs struct {
				Usage struct {
					SubscriptionTier string `json:"subscriptionTier"`
					Pools            []struct {
						PoolType       string  `json:"poolType"`
						PercentageUsed float64 `json:"percentageUsed"`
						GXU            struct {
							WindowRefreshesAt string `json:"windowRefreshesAt"`
						} `json:"gxu"`
					} `json:"pools"`
				} `json:"getUsageInfo"`
			} `json:"rpcs"`
		} `json:"viewer"`
	} `json:"data"`
}

// FetchQuota queries the Gemini usage-settings GraphQL endpoint for one account.
// It needs only the session cookie (no XSRF token), which makes it usable as a
// cheap background poll.
func FetchQuota(ctx context.Context, req Requester, info CookieInfo, authUser string) (*Quota, error) {
	if req == nil {
		return nil, fmt.Errorf("no requester configured")
	}
	if info.Cookie == "" {
		return nil, fmt.Errorf("no cookie configured")
	}

	endpoint := fmt.Sprintf(
		"https://gemini.google.com%s/_/BardChatUi/graphql/schemas/GEMINI_WEB_GRAPHQL/executeQuery?query_signature=%s&variables=%s",
		AccountPrefix(authUser),
		url.QueryEscape(quotaQuerySignature),
		url.QueryEscape(quotaQueryVariables),
	)

	httpReq, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("Referer", "https://gemini.google.com/")
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	httpReq.Header.Set("Cookie", info.Cookie)
	if authUser != "" {
		httpReq.Header.Set("X-Goog-AuthUser", authUser)
	}

	resp, err := req.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota query status %d", resp.StatusCode)
	}

	return parseQuota(body)
}

func parseQuota(body []byte) (*Quota, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("empty quota response")
	}

	// The endpoint answers with a one-element array; accept a bare object too.
	var raw json.RawMessage
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return nil, fmt.Errorf("quota response not a JSON array: %w", err)
		}
		if len(arr) == 0 {
			return nil, fmt.Errorf("empty quota array")
		}
		raw = arr[0]
	} else {
		raw = json.RawMessage(trimmed)
	}

	var parsed usageResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("quota response unmarshal: %w", err)
	}

	usage := parsed.Data.Viewer.Rpcs.Usage
	if usage.SubscriptionTier == "" && len(usage.Pools) == 0 {
		return nil, fmt.Errorf("quota response has no usage info")
	}

	q := &Quota{Tier: usage.SubscriptionTier, FetchedAt: time.Now()}
	for _, p := range usage.Pools {
		pool := &QuotaPool{PoolType: p.PoolType, PercentageUsed: p.PercentageUsed}
		if p.GXU.WindowRefreshesAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, p.GXU.WindowRefreshesAt); err == nil {
				pool.WindowRefreshesAt = &t
			}
		}
		switch p.PoolType {
		case PoolFiveHourly:
			q.FiveHour = pool
		case PoolWeekly:
			q.Weekly = pool
		}
	}
	return q, nil
}

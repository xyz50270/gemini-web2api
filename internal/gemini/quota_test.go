package gemini

import (
	"testing"
	"time"
)

// realCapture is a real response from GEMINI_WEB_GRAPHQL/executeQuery,
// trimmed to the fields the parser reads.
const realCapture = `[{
  "data": {
    "viewer": {
      "rpcs": {
        "getUserStatus": {
          "featureGates": ["CAN_SEE_USAGE_LIMITS"]
        },
        "getUsageInfo": {
          "subscriptionTier": "TIER1",
          "pools": [{
            "poolType": "GXU_FIVE_HOURLY",
            "percentageUsed": 0.03,
            "gxu": {
              "windowRefreshesAt": "2026-11-15T14:14:39.921480Z"
            }
          }, {
            "poolType": "GXU_WEEKLY",
            "percentageUsed": 0.00918175,
            "gxu": {
              "windowRefreshesAt": "2026-11-18T00:14:39.921654Z"
            }
          }]
        }
      }
    }
  }
}]`

func TestParseQuotaRealCapture(t *testing.T) {
	q, err := parseQuota([]byte(realCapture))
	if err != nil {
		t.Fatalf("parseQuota: %v", err)
	}
	if q.Tier != "TIER1" {
		t.Errorf("Tier = %q, want TIER1", q.Tier)
	}
	if q.FiveHour == nil {
		t.Fatal("FiveHour is nil")
	}
	if q.FiveHour.PoolType != PoolFiveHourly {
		t.Errorf("FiveHour.PoolType = %q", q.FiveHour.PoolType)
	}
	if got, want := q.FiveHour.PercentageUsed, 0.03; got != want {
		t.Errorf("FiveHour.PercentageUsed = %v, want %v", got, want)
	}
	if q.FiveHour.WindowRefreshesAt == nil {
		t.Fatal("FiveHour.WindowRefreshesAt is nil")
	}
	wantReset := time.Date(2026, 11, 15, 14, 14, 39, 921480000, time.UTC)
	if !q.FiveHour.WindowRefreshesAt.Equal(wantReset) {
		t.Errorf("FiveHour reset = %v, want %v", q.FiveHour.WindowRefreshesAt, wantReset)
	}
	if q.Weekly == nil {
		t.Fatal("Weekly is nil")
	}
	if got, want := q.Weekly.PoolType, PoolWeekly; got != want {
		t.Errorf("Weekly.PoolType = %q, want %q", got, want)
	}
}

func TestQuotaMaxUsed(t *testing.T) {
	tests := []struct {
		name string
		q    *Quota
		want float64
	}{
		{"nil", nil, 0},
		{"empty", &Quota{}, 0},
		{"five hour higher", &Quota{
			FiveHour: &QuotaPool{PercentageUsed: 0.9},
			Weekly:   &QuotaPool{PercentageUsed: 0.2},
		}, 0.9},
		{"weekly higher", &Quota{
			FiveHour: &QuotaPool{PercentageUsed: 0.1},
			Weekly:   &QuotaPool{PercentageUsed: 0.7},
		}, 0.7},
		{"only weekly", &Quota{
			Weekly: &QuotaPool{PercentageUsed: 0.42},
		}, 0.42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.q.MaxUsed(); got != tt.want {
				t.Errorf("MaxUsed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQuotaNextReset(t *testing.T) {
	early := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	late := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

	q := &Quota{
		FiveHour: &QuotaPool{WindowRefreshesAt: &early},
		Weekly:   &QuotaPool{WindowRefreshesAt: &late},
	}
	got, ok := q.NextReset()
	if !ok {
		t.Fatal("NextReset() ok = false")
	}
	if !got.Equal(early) {
		t.Errorf("NextReset() = %v, want %v", got, early)
	}

	if _, ok := (&Quota{}).NextReset(); ok {
		t.Error("empty quota should not report a reset time")
	}
	if _, ok := (*Quota)(nil).NextReset(); ok {
		t.Error("nil quota should not report a reset time")
	}
}

func TestParseQuotaErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"not json", "<html>nope</html>"},
		{"empty array", "[]"},
		{"no usage info", `[{"data":{"viewer":{"rpcs":{}}}}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseQuota([]byte(tt.body)); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestParseQuotaAcceptsBareObject(t *testing.T) {
	bare := `{"data":{"viewer":{"rpcs":{"getUsageInfo":{"subscriptionTier":"TIER2",` +
		`"pools":[{"poolType":"GXU_WEEKLY","percentageUsed":0.5}]}}}}}`
	q, err := parseQuota([]byte(bare))
	if err != nil {
		t.Fatalf("parseQuota: %v", err)
	}
	if q.Tier != "TIER2" {
		t.Errorf("Tier = %q, want TIER2", q.Tier)
	}
	if q.FiveHour != nil {
		t.Error("FiveHour should be nil when only weekly is reported")
	}
	if q.Weekly == nil || q.Weekly.PercentageUsed != 0.5 {
		t.Errorf("Weekly = %+v", q.Weekly)
	}
}

func TestQuotaPoolResetsIn(t *testing.T) {
	now := time.Now()
	future := now.Add(2 * time.Hour)
	if got := (&QuotaPool{WindowRefreshesAt: &future}).ResetsIn(now); got < time.Hour {
		t.Errorf("ResetsIn = %v, want about 2h", got)
	}
	past := now.Add(-time.Hour)
	if got := (&QuotaPool{WindowRefreshesAt: &past}).ResetsIn(now); got != 0 {
		t.Errorf("ResetsIn for past window = %v, want 0", got)
	}
	if got := (*QuotaPool)(nil).ResetsIn(now); got != 0 {
		t.Errorf("nil ResetsIn = %v, want 0", got)
	}
}

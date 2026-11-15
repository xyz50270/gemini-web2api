package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// AccountConfig describes one Google account slot served by the same cookie set.
// AuthUser is the /u/<N> index; the cookie itself usually carries every logged-in
// account of the browser profile, so several entries can share one CookieFile.
type AccountConfig struct {
	Label      string `json:"label"`
	AuthUser   string `json:"auth_user"`
	XSRFToken  string `json:"xsrf_token"`
	CookieFile string `json:"cookie_file"`
	Enabled    *bool  `json:"enabled"`
}

// IsEnabled reports whether the account participates in the pool. A nil Enabled
// means "enabled", so accounts work without spelling out the field.
func (a AccountConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

type Config struct {
	Port              int      `json:"port"`
	Host              string   `json:"host"`
	RetryAttempts     int      `json:"retry_attempts"`
	RetryDelaySec     int      `json:"retry_delay_sec"`
	RequestTimeoutSec int      `json:"request_timeout_sec"`
	GeminiBL          string   `json:"gemini_bl"`
	AuthUser          string   `json:"auth_user"`
	XSRFToken         string   `json:"xsrf_token"`
	DefaultModel      string   `json:"default_model"`
	LogRequests       bool     `json:"log_requests"`
	CookieFile        string   `json:"cookie_file"`
	Proxy             string   `json:"proxy"`
	APIKeys           []string `json:"api_keys"`
	Impersonate       string   `json:"impersonate"`
	TemporaryChats    bool     `json:"temporary_chats"`

	// Multi-account pool. When Accounts is empty the legacy single-account
	// fields above drive a one-entry pool, so existing configs keep working.
	Accounts        []AccountConfig `json:"accounts"`
	QuotaThreshold  float64         `json:"quota_threshold"`
	QuotaRefreshSec int             `json:"quota_refresh_sec"`
	AutoXSRF        bool            `json:"auto_xsrf"`

	// Admission and context reuse. MaxConcurrent serialises upstream generation
	// so the service never looks like a bot farm; 1 matches a single browser tab.
	MaxConcurrent   int    `json:"max_concurrent"`
	ConvCache       bool   `json:"conv_cache"`
	ConvCacheTTLDay int    `json:"conv_cache_ttl_days"`
	ConvCachePath   string `json:"conv_cache_path"`
}

func Default() Config {
	return Config{
		Port:              8081,
		Host:              "0.0.0.0",
		RetryAttempts:     3,
		RetryDelaySec:     2,
		RequestTimeoutSec: 180,
		GeminiBL:          "boq_gemini-web-uiserver_20261113.11_p0",
		AuthUser:          "",
		XSRFToken:         "",
		DefaultModel:      "gemini-4-hydrogen",
		LogRequests:       true,
		CookieFile:        "",
		Proxy:             "",
		APIKeys:           []string{},
		Impersonate:       "",
		TemporaryChats:    false,
		Accounts:          []AccountConfig{},
		QuotaThreshold:    0.95,
		QuotaRefreshSec:   300,
		AutoXSRF:          true,
		MaxConcurrent:     1,
		ConvCache:         true,
		ConvCacheTTLDay:   30,
		ConvCachePath:     "/app/data/conversations.json",
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	// Use temporary struct with pointer fields or interface to handle null values from JSON if needed,
	// but standard unmarshal over struct fields handles JSON null for strings by leaving existing or setting zero-value if unmarshaling map/struct.
	// Let's ensure string fields default properly if JSON has explicitly `null`.
	var aux struct {
		Port              *int      `json:"port"`
		Host              *string   `json:"host"`
		RetryAttempts     *int      `json:"retry_attempts"`
		RetryDelaySec     *int      `json:"retry_delay_sec"`
		RequestTimeoutSec *int      `json:"request_timeout_sec"`
		GeminiBL          *string   `json:"gemini_bl"`
		AuthUser          *string   `json:"auth_user"`
		XSRFToken         *string   `json:"xsrf_token"`
		DefaultModel      *string   `json:"default_model"`
		LogRequests       *bool     `json:"log_requests"`
		CookieFile        *string   `json:"cookie_file"`
		Proxy             *string   `json:"proxy"`
		APIKeys           *[]string         `json:"api_keys"`
		Impersonate       *string           `json:"impersonate"`
		TemporaryChats    *bool             `json:"temporary_chats"`
		Accounts          *[]AccountConfig  `json:"accounts"`
		QuotaThreshold    *float64          `json:"quota_threshold"`
		QuotaRefreshSec   *int              `json:"quota_refresh_sec"`
		AutoXSRF          *bool             `json:"auto_xsrf"`
		MaxConcurrent     *int              `json:"max_concurrent"`
		ConvCache         *bool             `json:"conv_cache"`
		ConvCacheTTLDay   *int              `json:"conv_cache_ttl_days"`
		ConvCachePath     *string           `json:"conv_cache_path"`
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return cfg, err
	}

	if aux.Port != nil {
		cfg.Port = *aux.Port
	}
	if aux.Host != nil {
		cfg.Host = *aux.Host
	}
	if aux.RetryAttempts != nil {
		cfg.RetryAttempts = *aux.RetryAttempts
	}
	if aux.RetryDelaySec != nil {
		cfg.RetryDelaySec = *aux.RetryDelaySec
	}
	if aux.RequestTimeoutSec != nil {
		cfg.RequestTimeoutSec = *aux.RequestTimeoutSec
	}
	if aux.GeminiBL != nil {
		cfg.GeminiBL = *aux.GeminiBL
	}
	if aux.AuthUser != nil {
		cfg.AuthUser = *aux.AuthUser
	}
	if aux.XSRFToken != nil {
		cfg.XSRFToken = *aux.XSRFToken
	}
	if aux.DefaultModel != nil {
		cfg.DefaultModel = *aux.DefaultModel
	}
	if aux.LogRequests != nil {
		cfg.LogRequests = *aux.LogRequests
	}
	if aux.CookieFile != nil {
		cfg.CookieFile = *aux.CookieFile
	}
	if aux.Proxy != nil {
		cfg.Proxy = *aux.Proxy
	}
	if aux.APIKeys != nil {
		cfg.APIKeys = *aux.APIKeys
	}
	if aux.Impersonate != nil {
		cfg.Impersonate = *aux.Impersonate
	}
	if aux.TemporaryChats != nil {
		cfg.TemporaryChats = *aux.TemporaryChats
	}
	if aux.Accounts != nil {
		cfg.Accounts = *aux.Accounts
	}
	if aux.QuotaThreshold != nil {
		cfg.QuotaThreshold = *aux.QuotaThreshold
	}
	if aux.QuotaRefreshSec != nil {
		cfg.QuotaRefreshSec = *aux.QuotaRefreshSec
	}
	if aux.AutoXSRF != nil {
		cfg.AutoXSRF = *aux.AutoXSRF
	}
	if aux.MaxConcurrent != nil {
		cfg.MaxConcurrent = *aux.MaxConcurrent
	}
	if aux.ConvCache != nil {
		cfg.ConvCache = *aux.ConvCache
	}
	if aux.ConvCacheTTLDay != nil {
		cfg.ConvCacheTTLDay = *aux.ConvCacheTTLDay
	}
	if aux.ConvCachePath != nil {
		cfg.ConvCachePath = *aux.ConvCachePath
	}

	return cfg, nil
}

// ResolvedAccounts returns the effective account list. When Accounts is empty the
// legacy single-account fields are turned into a one-entry list so callers can
// always iterate a pool.
func (c Config) ResolvedAccounts() []AccountConfig {
	var out []AccountConfig
	for _, a := range c.Accounts {
		if !a.IsEnabled() {
			continue
		}
		if a.Label == "" {
			a.Label = "account-" + a.AuthUser
			if a.AuthUser == "" {
				a.Label = "default"
			}
		}
		if a.CookieFile == "" {
			a.CookieFile = c.CookieFile
		}
		if a.XSRFToken == "" {
			a.XSRFToken = c.XSRFToken
		}
		out = append(out, a)
	}
	if len(out) > 0 {
		return out
	}
	return []AccountConfig{{
		Label:      "default",
		AuthUser:   c.AuthUser,
		XSRFToken:  c.XSRFToken,
		CookieFile: c.CookieFile,
	}}
}

func Find() string {
	paths := []string{"./config.json"}
	home, err := os.UserHomeDir()
	if err == nil {
		paths = append(paths, filepath.Join(home, ".config", "gemini-web2api", "config.json"))
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

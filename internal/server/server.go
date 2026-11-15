package server

import (
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/conv"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
)

type App struct {
	Cfg        config.Config
	Pool       *Pool
	HTTPClient *http.Client
	Logf       func(format string, args ...any)
	Version    string
}

func createHTTPClient(cfg config.Config) *http.Client {
	transport := &http.Transport{}
	if cfg.Proxy != "" {
		if proxyURL, err := url.Parse(cfg.Proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

func New(cfg config.Config, version string) *App {
	logFn := func(format string, args ...any) {
		if cfg.LogRequests {
			log.Printf(format, args...)
		}
	}

	httpClient := createHTTPClient(cfg)

	// The pool shares one outbound requester across accounts, so proxy and TLS
	// settings stay uniform.
	var requester gemini.Requester
	probe := gemini.NewClient(cfg)
	requester = probe.HTTP

	pool := NewPool(cfg, requester, logFn)

	// Conversation reuse needs the persisted handles; a failure to open the file
	// only disables caching, it must not prevent startup.
	if cfg.ConvCache && !cfg.TemporaryChats {
		store, err := conv.Open(cfg.ConvCachePath)
		if err != nil {
			logFn("conversation cache disabled: %v", err)
		} else {
			ttl := time.Duration(cfg.ConvCacheTTLDay) * 24 * time.Hour
			if n := store.PurgeOlderThan(ttl); n > 0 {
				logFn("conversation cache: purged %d entries older than %s", n, ttl)
			}
			store.StartBackground()
			pool.convs = store
			logFn("conversation cache: %d entries at %s (ttl %dd)", store.Len(), cfg.ConvCachePath, cfg.ConvCacheTTLDay)
		}
	} else if cfg.TemporaryChats {
		logFn("conversation cache disabled: temporary_chats is on, upstream discards those chats")
	}

	app := &App{
		Cfg:        cfg,
		Pool:       pool,
		HTTPClient: httpClient,
		Logf:       logFn,
		Version:    version,
	}

	return app
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", a.handleHealth)
	mux.HandleFunc("GET /v1/models", a.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", a.handleChat)
	mux.HandleFunc("POST /v1/responses", a.handleResponses)

	mux.HandleFunc("GET /v1beta/models", a.handleGoogleModels)
	mux.HandleFunc("POST /v1beta/models/{target}", a.handleGoogleGenerate)

	mux.HandleFunc("GET /v1/accounts", a.handleAccounts)

	handler := a.withAuthAndLogging(mux)
	handler = a.withCORS(handler)

	return handler
}

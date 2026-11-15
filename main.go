package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/gemini"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
	"github.com/ikhsan3adi/gemini-web2api/internal/server"
)

var Version = "dev"

func resolveVersion() string {
	if Version != "" && Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}

func main() {
	currentVersion := resolveVersion()

	portFlag := flag.Int("port", 0, "Server port")
	configFlag := flag.String("config", "", "Config file path")
	cookieFlag := flag.String("cookie-file", "", "Cookie file path")
	proxyFlag := flag.String("proxy", "", "HTTP proxy URL")
	impersonateFlag := flag.String("impersonate", "", "TLS impersonation profile")
	versionFlag := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gemini-web2api %s\n", currentVersion)
		os.Exit(0)
	}

	configPath := *configFlag
	if configPath == "" {
		configPath = os.Getenv("GEMINI_WEB2API_CONFIG")
	}
	if configPath == "" {
		configPath = config.Find()
	}

	cfg, err := config.Load(configPath)
	if err != nil && configPath != "" {
		log.Printf("Warning: failed to load config from %s: %v", configPath, err)
	}

	if *portFlag != 0 {
		cfg.Port = *portFlag
	}
	if *cookieFlag != "" {
		cfg.CookieFile = *cookieFlag
	}
	if *proxyFlag != "" {
		cfg.Proxy = *proxyFlag
	}
	if *impersonateFlag != "" {
		cfg.Impersonate = *impersonateFlag
	}

	app := server.New(cfg, currentVersion)

	// Startup work gets a bounded budget; the long-lived background loop must not
	// inherit it, or it would exit as soon as the startup phase is over.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelStartup()

	// backgroundCtx lives until shutdown. Cookie rotation and quota refresh run on
	// it, so tying them to startupCtx would silently stop them after 90s.
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	// BL refresh is global (one frontend build id for all accounts).
	probe := gemini.NewClient(cfg)
	probe.HTTP = app.Pool.Requester()
	if newBL, changed, blErr := probe.UpdateBLIfNeeded(); blErr != nil {
		log.Printf("BL auto-update failed (non-fatal): %v", blErr)
	} else if changed {
		log.Printf("BL auto-updated: %s -> %s", cfg.GeminiBL, newBL)
	}

	// Per-account startup: fetch the page token if missing, discover models and
	// read the current usage so the pool knows who is over threshold.
	for _, acct := range app.Pool.Accounts() {
		if cfg.AutoXSRF && acct.XSRFToken() == "" {
			if err := app.Pool.RefreshXSRFToken(acct); err != nil {
				log.Printf("account %s: xsrf token fetch failed: %v", acct.Label, err)
			}
		}
		acct.Client.EnsureModelsDiscovered()
		modelsFound := len(acct.Client.DiscoveredModels())

		quotaDesc := "unknown"
		if err := app.Pool.RefreshQuota(startupCtx, acct); err != nil {
			log.Printf("account %s: quota fetch failed: %v", acct.Label, err)
		} else if q := acct.Quota(); q != nil {
			quotaDesc = fmt.Sprintf("tier=%s five_hour=%.1f%% weekly=%.1f%%",
				q.Tier, poolPercent(q.FiveHour), poolPercent(q.Weekly))
		}

		log.Printf("account %s (u/%s): %s models=%d", acct.Label, acct.AuthUser, quotaDesc, modelsFound)
	}

	app.Pool.StartBackground(backgroundCtx)

	modelKeys := make([]string, 0, len(models.MODELS))
	for k := range models.MODELS {
		modelKeys = append(modelKeys, k)
	}

	accountStatus := fmt.Sprintf("%d (threshold %.0f%%, refresh %ds)", app.Pool.Len(), quotaThreshold(cfg), cfg.QuotaRefreshSec)

	cookieStatus := "none (anonymous)"
	if cfg.CookieFile != "" {
		cookieStatus = fmt.Sprintf("yes (%s)", cfg.CookieFile)
	}
	proxyStatus := cfg.Proxy
	if proxyStatus == "" {
		proxyStatus = "system env"
	}

	impersonateStatus := cfg.Impersonate
	if impersonateStatus == "" {
		impersonateStatus = "none (stdlib)"
	}

	temporaryStatus := "no"
	if cfg.TemporaryChats {
		temporaryStatus = "yes"
	}

	fmt.Printf("gemini-web2api %s\n", currentVersion)
	fmt.Printf("  Listening:   http://%s:%d\n", cfg.Host, cfg.Port)
	fmt.Printf("  Base URL:    http://localhost:%d/v1\n", cfg.Port)
	fmt.Printf("  Models:      %s\n", strings.Join(modelKeys, ", "))
	fmt.Printf("  Cookie:      %s\n", cookieStatus)
	fmt.Printf("  Accounts:    %s\n", accountStatus)
	fmt.Printf("  Proxy:       %s\n", proxyStatus)
	fmt.Printf("  Impersonate: %s\n", impersonateStatus)
	fmt.Printf("  Temporary:   %s\n", temporaryStatus)
	fmt.Println()

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
	}

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-stopCtx.Done()
	log.Println("Shutting down server gracefully...")
	app.Pool.Close()
	app.Pool.CloseConversations()
	cancelBackground()
	cancelStartup()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced shutdown: %v", err)
	}

	log.Println("Stopped.")
}

func poolPercent(p *gemini.QuotaPool) float64 {
	if p == nil {
		return 0
	}
	return p.PercentageUsed * 100
}

func quotaThreshold(cfg config.Config) float64 {
	if cfg.QuotaThreshold <= 0 || cfg.QuotaThreshold > 1 {
		return 95
	}
	return cfg.QuotaThreshold * 100
}

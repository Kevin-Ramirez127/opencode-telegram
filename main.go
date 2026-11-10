// opencode-telegram is a Telegram daemon for OpenCode.
//
// It subscribes to OpenCode's live event stream for every session and
// notifies allowlisted Telegram chats when a session finishes, fails, asks a
// question, or requests a permission. When explicitly enabled in the
// OpenCode plugin, it can relay typed answers and new prompts back to
// OpenCode.
//
// Usage:
//
//	TELEGRAM_BOT_TOKEN=... opencode-telegram
//
// Security posture (see README.md):
//   - user AND chat allowlist, fail closed;
//   - notifications disabled by default;
//   - arbitrary answers and new prompts disabled by default;
//   - one update per chat per second;
//   - no secret is ever logged.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/app"
	"opencode-telegram/internal/config"
	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
	"opencode-telegram/internal/whitelist"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		check       = flag.Bool("check", false, "validate configuration and connectivity, then exit")
		simulate    = flag.String("simulate", "", "run a local Telegram simulation scenario (use \"list\" to see them)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("opencode-telegram " + version)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if *simulate == "list" {
		for _, name := range app.SimulationScenarios() {
			fmt.Println(name)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.Version = version

	if *simulate != "" {
		runSimulation(cfg, *simulate)
		return
	}

	// Discover the OpenCode background service when explicit values are not
	// supplied. This is best-effort: the daemon keeps retrying while it runs.
	if cfg.OpenCodeURL == "" || cfg.OpenCodePassword == "" {
		if svc, ok := occlient.DiscoverService(); ok {
			if cfg.OpenCodeURL == "" {
				cfg.OpenCodeURL = svc.URL
			}
			if cfg.OpenCodePassword == "" {
				cfg.OpenCodePassword = svc.Password
			}
			log.Printf("using OpenCode service %s (version %s)", svc.URL, svc.Version)
		} else if cfg.OpenCodeURL == "" {
			log.Print("OpenCode service not found yet; will keep discovering")
		}
	}

	oc := occlient.New(cfg.OpenCodeURL, cfg.OpenCodePassword, cfg.HTTPTimeout)

	if *check {
		checkOnly(cfg, oc)
		return
	}

	bot, err := tgbotapi.NewBotAPI(cfg.Token)
	if err != nil {
		log.Fatalf("telegram authentication failed: %v", err)
	}
	log.Printf("authenticated as @%s", bot.Self.UserName)

	wl, err := whitelist.New(cfg.AllowlistFile)
	if err != nil {
		log.Printf("allowlist: %v (deny-all mode until fixed)", err)
	}
	st, err := settings.Open(cfg.SettingsFile)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	current := st.Get()

	a := app.New(cfg, bot, oc, wl, st, version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	log.Printf("config: %d allowlisted users, %d allowlisted chats, notifications=%s, custom answers=%s, new messages=%s",
		wl.LenUsers(), wl.LenChats(), onOff(current.Notifications.Enabled),
		onOff(current.Security.AllowCustomFormAnswers), onOff(current.Security.AllowNewMessages))

	a.Start(ctx, hup)
	select {
	case <-ctx.Done():
	case <-a.Done():
		log.Print("shutdown requested from Telegram")
	}
	log.Print("shutting down")
}

func runSimulation(cfg *config.Config, scenario string) {
	bot, err := tgbotapi.NewBotAPI(cfg.Token)
	if err != nil {
		log.Fatalf("telegram authentication failed: %v", err)
	}
	log.Printf("authenticated as @%s", bot.Self.UserName)

	wl, err := whitelist.New(cfg.AllowlistFile)
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}
	st, err := settings.Open(cfg.SettingsFile)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}

	a := app.New(cfg, bot, occlient.New("", "", cfg.HTTPTimeout), wl, st, version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.RunSimulation(ctx, scenario); err != nil {
		log.Fatalf("simulation: %v", err)
	}
}

func checkOnly(cfg *config.Config, oc *occlient.Client) {
	fmt.Printf("opencode-telegram %s\n", version)
	fmt.Printf("data dir:      %s\n", cfg.DataDir)
	fmt.Printf("plugin API:    http://%s\n", cfg.ListenAddr)
	if cfg.OpenCodeURL == "" {
		fmt.Println("opencode:      not discovered (start OpenCode or set OPENCODE_URL)")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		info, err := oc.Info(ctx)
		if err != nil {
			fmt.Printf("opencode:      %s (error: %v)\n", cfg.OpenCodeURL, err)
		} else {
			fmt.Printf("opencode:      %s (version %v)\n", cfg.OpenCodeURL, info["version"])
		}
	}
	if _, err := os.Stat(cfg.AllowlistFile); err != nil {
		fmt.Printf("allowlist:     %s (error: %v)\n", cfg.AllowlistFile, err)
	} else {
		fmt.Printf("allowlist:     %s\n", cfg.AllowlistFile)
	}
	fmt.Printf("settings:      %s\n", cfg.SettingsFile)
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

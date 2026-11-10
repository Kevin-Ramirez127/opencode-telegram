// Package config loads runtime configuration for the OpenCode Telegram daemon.
//
// Secrets come from the environment or an optional .env file and are never
// logged. The Telegram bot token is mandatory; the OpenCode API endpoint and
// password may be discovered from the background service registration when
// they are not supplied explicitly.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the fully-resolved daemon configuration.
type Config struct {
	// Token is the Telegram bot token from @BotFather.
	Token string

	// OpenCodeURL and OpenCodePassword point at the OpenCode HTTP API.
	// When either is empty, the daemon discovers the local background
	// service registration at startup and when reconnecting.
	OpenCodeURL      string
	OpenCodePassword string
	// OpenCodeURLExplicit / OpenCodePasswordExplicit record whether those
	// values came from the environment (and must not be replaced by service
	// discovery).
	OpenCodeURLExplicit      bool
	OpenCodePasswordExplicit bool

	// ListenAddr is the loopback address of the local settings API used by
	// the OpenCode plugin.
	ListenAddr string
	// PluginToken optionally protects the settings API with a bearer token.
	PluginToken string

	// DataDir holds the allowlist and settings files.
	DataDir       string
	AllowlistFile string
	SettingsFile  string

	// ReloadEvery controls how often the allowlist and settings files are
	// checked for external edits.
	ReloadEvery time.Duration
	// HTTPTimeout bounds a single OpenCode API request.
	HTTPTimeout time.Duration

	// Version is injected by main for /health and /status.
	Version string
}

// Load reads the environment (plus an optional .env) and returns the config.
func Load() (*Config, error) {
	if err := loadDotEnv(); err != nil {
		return nil, fmt.Errorf("loading .env: %w", err)
	}

	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is not set (get one from @BotFather)")
	}

	dataDir := env("DATA_DIR", "data")
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("bad DATA_DIR %q: %w", dataDir, err)
	}

	listen := env("LISTEN_ADDR", "127.0.0.1:8787")
	if !strings.Contains(listen, ":") {
		return nil, fmt.Errorf("LISTEN_ADDR %q must be host:port", listen)
	}

	reloadEvery, err := envDuration("RELOAD_SECONDS", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("RELOAD_SECONDS: %w", err)
	}
	httpTimeout, err := envDuration("HTTP_TIMEOUT_SECONDS", 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf("HTTP_TIMEOUT_SECONDS: %w", err)
	}

	return &Config{
		Token:                    token,
		OpenCodeURL:              strings.TrimRight(strings.TrimSpace(os.Getenv("OPENCODE_URL")), "/"),
		OpenCodePassword:         strings.TrimSpace(os.Getenv("OPENCODE_PASSWORD")),
		OpenCodeURLExplicit:      strings.TrimSpace(os.Getenv("OPENCODE_URL")) != "",
		OpenCodePasswordExplicit: strings.TrimSpace(os.Getenv("OPENCODE_PASSWORD")) != "",
		ListenAddr:               listen,
		PluginToken:              strings.TrimSpace(os.Getenv("PLUGIN_TOKEN")),
		DataDir:                  abs,
		AllowlistFile:            filepath.Join(abs, "allowlist.json"),
		SettingsFile:             filepath.Join(abs, "settings.json"),
		ReloadEvery:              reloadEvery,
		HTTPTimeout:              httpTimeout,
	}, nil
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return d, nil
}

// loadDotEnv applies an optional .env file. Existing environment variables
// always win, so a service manager's EnvironmentFile cannot be overridden by
// a stray .env on disk.
func loadDotEnv() error {
	data, err := os.ReadFile(".env")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opencode-telegram/internal/config"
	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
	"opencode-telegram/internal/whitelist"
)

func testApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	allowPath := filepath.Join(dir, "allowlist.json")
	allow := `{"users":[123],"chats":[123]}`
	if err := os.WriteFile(allowPath, []byte(allow), 0o600); err != nil {
		t.Fatal(err)
	}
	wl, err := whitelist.New(allowPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:       dir,
		AllowlistFile: allowPath,
		SettingsFile:  filepath.Join(dir, "settings.json"),
		ListenAddr:    "127.0.0.1:0",
		PluginToken:   "test-token",
	}
	a := New(cfg, nil, occlient.New("", "", time.Second), wl, st, "test")
	srv := httptest.NewServer(a.routes())
	t.Cleanup(srv.Close)
	return a, srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&data)
	resp.Body.Close()
	return resp, data
}

func TestSettingsAPIRoundTrip(t *testing.T) {
	a, srv := testApp(t)

	resp, data := do(t, srv, http.MethodGet, "/api/plugin/settings", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d (%v)", resp.StatusCode, data)
	}
	settingsObj, _ := data["settings"].(map[string]any)
	notif, _ := settingsObj["notifications"].(map[string]any)
	if notif["enabled"] != false {
		t.Fatalf("default enabled = %v", notif["enabled"])
	}

	resp, data = do(t, srv, http.MethodPatch, "/api/plugin/settings",
		`{"notifications":{"enabled":true},"security":{"allow_new_messages":true}}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d (%v)", resp.StatusCode, data)
	}
	if !a.settings.Get().Notifications.Enabled {
		t.Fatal("settings change was not applied")
	}
	if !a.settings.Get().Security.AllowNewMessages {
		t.Fatal("security change was not applied")
	}
}

func TestSettingsAPIUpdatesDeliveryAndThinking(t *testing.T) {
	a, srv := testApp(t)

	resp, data := do(t, srv, http.MethodPatch, "/api/plugin/settings",
		`{"notifications":{"long_message_mode":"summary","show_raw_thinking":true,"resume_thinking_mode":"summary"},`+
			`"security":{"allow_resume_thinking":true}}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d (%v)", resp.StatusCode, data)
	}
	got := a.settings.Get()
	if got.Notifications.LongMessageMode != "summary" ||
		!got.Notifications.ShowRawThinking ||
		got.Notifications.ResumeThinkingMode != "summary" ||
		!got.Security.AllowResumeThinking {
		t.Fatalf("settings = %+v", got)
	}
}

func TestSettingsAPIRejectsUnknownChat(t *testing.T) {
	_, srv := testApp(t)
	resp, data := do(t, srv, http.MethodPatch, "/api/plugin/settings",
		`{"notifications":{"notify_chats":[999]}}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", resp.StatusCode, data)
	}
}

func TestSettingsAPIAuth(t *testing.T) {
	_, srv := testApp(t)
	resp, _ := do(t, srv, http.MethodGet, "/api/plugin/settings", "", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	_, srv := testApp(t)
	resp, data := do(t, srv, http.MethodGet, "/health", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	if data["version"] != "test" {
		t.Fatalf("version = %v", data["version"])
	}
}

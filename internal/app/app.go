// Package app is the OpenCode Telegram daemon.
//
// It listens to OpenCode's event stream for every session, notifies the
// allowlisted Telegram chats when a session finishes or when the agent asks a
// question / permission, and can relay user replies back to OpenCode when the
// operator enables those actions in the plugin settings.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/config"
	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/ratelimit"
	"opencode-telegram/internal/settings"
	"opencode-telegram/internal/whitelist"
)

// App owns all daemon state. Telegram updates, the SSE event loop, and the
// local plugin API run concurrently; the mutex protects the mutable maps.
type App struct {
	cfg       *config.Config
	bot       *tgbotapi.BotAPI
	oc        *occlient.Client
	whitelist *whitelist.Whitelist
	settings  *settings.Store
	limit     *ratelimit.Limiter

	version          string
	started          time.Time
	sem              chan struct{}
	updates          chan occlient.Event
	syncMu           sync.Mutex
	explicitURL      bool
	explicitPassword bool

	mu           sync.Mutex
	formMu       sync.Mutex
	sessions     map[string]*sessionState
	bindings     map[int64]string
	actions      map[string]*action
	forms        map[string]*formFlow
	pendingInput map[int64]*pendingInput
	notified     map[string]time.Time
	permMsgs     map[string][]sentMessage

	allowlistStat os.FileInfo
	settingsStat  os.FileInfo

	// eventStatus is read by the local HTTP API.
	ocConnected bool
	ocVersion   string
	lastEventAt time.Time
	lastError   string
}

func New(cfg *config.Config, bot *tgbotapi.BotAPI, oc *occlient.Client, wl *whitelist.Whitelist, st *settings.Store, version string) *App {
	return &App{
		cfg:              cfg,
		bot:              bot,
		oc:               oc,
		whitelist:        wl,
		settings:         st,
		limit:            ratelimit.New(),
		version:          version,
		started:          time.Now(),
		sem:              make(chan struct{}, 8),
		updates:          make(chan occlient.Event, 4096),
		explicitURL:      cfg.OpenCodeURLExplicit,
		explicitPassword: cfg.OpenCodePasswordExplicit,
		sessions:         make(map[string]*sessionState),
		bindings:         make(map[int64]string),
		actions:          make(map[string]*action),
		forms:            make(map[string]*formFlow),
		pendingInput:     make(map[int64]*pendingInput),
		notified:         make(map[string]time.Time),
		permMsgs:         make(map[string][]sentMessage),
	}
}

// Start launches the daemon goroutines. ctx cancellation stops them.
func (a *App) Start(ctx context.Context, hup <-chan os.Signal) {
	go a.eventLoop(ctx)
	go a.telegramLoop(ctx)
	go a.pruneLoop(ctx)
	go a.reloadLoop(ctx, hup)
	go a.serveHTTP(ctx)

	log.Printf("daemon started: opencode=%s listen=%s", a.endpointLabel(), a.cfg.ListenAddr)
}

func (a *App) endpointLabel() string {
	base, _ := a.oc.Endpoint()
	if base == "" {
		return "(discovering)"
	}
	return base
}

// ---------------------------------------------------------------------------
// Small state helpers
// ---------------------------------------------------------------------------

type sessionState struct {
	ID          string
	Title       string
	Directory   string
	Busy        bool
	NotifiedRun bool
	Outcome     string
	Updated     int64
	LastSeen    time.Time
}

func (a *App) rememberSession(s occlient.Session) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[s.ID]
	if st == nil {
		st = &sessionState{ID: s.ID}
		a.sessions[s.ID] = st
	}
	if s.Title != "" {
		st.Title = s.Title
	}
	if s.Location.Directory != "" {
		st.Directory = s.Location.Directory
	}
	if s.Outcome != "" {
		st.Outcome = s.Outcome
	}
	if s.Time.Updated > st.Updated {
		st.Updated = s.Time.Updated
	}
	st.LastSeen = time.Now()
	return st
}

func (a *App) sessionSnapshot(id string) sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.sessions[id]; st != nil {
		return *st
	}
	return sessionState{ID: id}
}

func (a *App) setBusy(id string, busy bool) *sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[id]
	if st == nil {
		st = &sessionState{ID: id}
		a.sessions[id] = st
	}
	st.Busy = busy
	st.LastSeen = time.Now()
	if busy {
		st.NotifiedRun = false
	}
	return st
}

// markOnce returns true the first time key is seen within ttl.
func (a *App) markOnce(key string, ttl time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if t, ok := a.notified[key]; ok && now.Sub(t) < ttl {
		return false
	}
	a.notified[key] = now
	return true
}

func (a *App) clearNotified(prefix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.notified {
		if strings.HasPrefix(k, prefix) {
			delete(a.notified, k)
		}
	}
}

func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Local plugin API state helpers
// ---------------------------------------------------------------------------

type statusSnapshot struct {
	OK          bool   `json:"ok"`
	Version     string `json:"version"`
	UptimeSec   int64  `json:"uptime_sec"`
	OpenCodeURL string `json:"opencode_url,omitempty"`
	OpenCodeOK  bool   `json:"opencode_ok"`
	OpenCodeVer string `json:"opencode_version,omitempty"`
	LastEvent   int64  `json:"last_event,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	Users       int    `json:"users"`
	Chats       int    `json:"chats"`
	Enabled     bool   `json:"notifications_enabled"`
	Active      int    `json:"active_sessions"`
	PendingForm int    `json:"pending_forms"`
	PendingPerm int    `json:"pending_permissions"`
}

func (a *App) snapshot() statusSnapshot {
	base, _ := a.oc.Endpoint()
	a.mu.Lock()
	active := 0
	for _, st := range a.sessions {
		if st.Busy {
			active++
		}
	}
	forms := len(a.forms)
	a.mu.Unlock()

	perms, _ := a.pendingPermissionCount(context.Background())
	st := a.settings.Get()
	return statusSnapshot{
		OK:          a.ocStatusOK(),
		Version:     a.version,
		UptimeSec:   int64(time.Since(a.started).Seconds()),
		OpenCodeURL: base,
		OpenCodeOK:  a.ocStatusOK(),
		OpenCodeVer: a.ocStatusVersion(),
		LastEvent:   a.ocStatusLastEvent(),
		LastError:   a.ocStatusError(),
		Users:       a.whitelist.LenUsers(),
		Chats:       a.whitelist.LenChats(),
		Enabled:     st.Notifications.Enabled,
		Active:      active,
		PendingForm: forms,
		PendingPerm: perms,
	}
}

func (a *App) ocStatusOK() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ocConnected
}

func (a *App) ocStatusVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ocVersion
}

func (a *App) ocStatusLastEvent() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastEventAt.IsZero() {
		return 0
	}
	return a.lastEventAt.UnixMilli()
}

func (a *App) ocStatusError() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastError
}

func (a *App) setOCStatus(ok bool, version string, err error) {
	a.mu.Lock()
	a.ocConnected = ok
	if version != "" {
		a.ocVersion = version
	}
	if err != nil {
		a.lastError = err.Error()
	} else {
		a.lastError = ""
	}
	a.mu.Unlock()
}

func (a *App) noteEvent() {
	a.mu.Lock()
	a.lastEventAt = time.Now()
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Periodic maintenance
// ---------------------------------------------------------------------------

func (a *App) pruneLoop(ctx context.Context) {
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.limit.Prune(30 * time.Minute)
			a.pruneActions(24 * time.Hour)
			a.pruneNotified(48 * time.Hour)
			a.pruneSessions(7 * 24 * time.Hour)
		}
	}
}

func (a *App) pruneActions(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, act := range a.actions {
		if act.Expires.Before(cutoff) {
			delete(a.actions, token)
		}
	}
}

func (a *App) pruneNotified(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, t := range a.notified {
		if t.Before(cutoff) {
			delete(a.notified, k)
		}
	}
}

func (a *App) pruneSessions(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, st := range a.sessions {
		if st.Busy {
			continue
		}
		if !st.LastSeen.IsZero() && st.LastSeen.Before(cutoff) {
			delete(a.sessions, id)
		}
	}
}

func (a *App) reloadLoop(ctx context.Context, hup <-chan os.Signal) {
	tick := time.NewTicker(a.cfg.ReloadEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			log.Print("SIGHUP: reloading allowlist and settings")
			a.reloadAll()
		case <-tick.C:
			a.reloadIfChanged()
		}
	}
}

func (a *App) reloadAll() {
	if err := a.whitelist.Load(); err != nil {
		log.Printf("allowlist reload failed (keeping previous): %v", err)
	} else {
		log.Printf("allowlist reloaded: %d users, %d chats", a.whitelist.LenUsers(), a.whitelist.LenChats())
	}
	if err := a.settings.Load(); err != nil {
		log.Printf("settings reload failed (keeping previous): %v", err)
	} else {
		log.Print("settings reloaded")
	}
}

func (a *App) reloadIfChanged() {
	if changed := fileChanged(a.cfg.AllowlistFile, &a.allowlistStat); changed {
		if err := a.whitelist.Load(); err != nil {
			log.Printf("allowlist reload failed (keeping previous): %v", err)
		} else {
			log.Printf("allowlist reloaded: %d users, %d chats", a.whitelist.LenUsers(), a.whitelist.LenChats())
		}
	}
	if changed := fileChanged(a.cfg.SettingsFile, &a.settingsStat); changed {
		if err := a.settings.Load(); err != nil {
			log.Printf("settings reload failed (keeping previous): %v", err)
		} else {
			log.Print("settings reloaded")
		}
	}
}

func fileChanged(path string, cached *os.FileInfo) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if *cached != nil && fi.Size() == (*cached).Size() && fi.ModTime().Equal((*cached).ModTime()) {
		return false
	}
	*cached = fi
	return true
}

// ---------------------------------------------------------------------------
// Action tokens (Telegram callback_data must stay under 64 bytes)
// ---------------------------------------------------------------------------

type action struct {
	Kind      string
	SessionID string
	FormID    string
	FieldKey  string
	RequestID string
	Value     string
	Decision  string
	ChatID    int64
	Expires   time.Time
}

func (a *App) addAction(act action) string {
	if act.Expires.IsZero() {
		act.Expires = time.Now().Add(24 * time.Hour)
	}
	token := randomToken()
	a.mu.Lock()
	a.actions[token] = &act
	a.mu.Unlock()
	return token
}

func (a *App) getAction(token string) (action, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	act := a.actions[token]
	if act == nil {
		return action{}, false
	}
	if time.Now().After(act.Expires) {
		delete(a.actions, token)
		return action{}, false
	}
	return *act, true
}

func (a *App) delAction(token string) {
	a.mu.Lock()
	delete(a.actions, token)
	a.mu.Unlock()
}

func (a *App) delFormActions(formID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, act := range a.actions {
		if act.FormID == formID {
			delete(a.actions, token)
		}
	}
}

func callbackData(token string) string {
	return "a:" + token
}

func parseCallbackData(data string) (string, bool) {
	if !strings.HasPrefix(data, "a:") {
		return "", false
	}
	return strings.TrimPrefix(data, "a:"), true
}

// ---------------------------------------------------------------------------
// Session list helpers used by Telegram and the plugin API
// ---------------------------------------------------------------------------

func (a *App) recentSessions(ctx context.Context, limit int) ([]occlient.Session, error) {
	sessions, err := a.oc.ListSessions(ctx, limit)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		a.rememberSession(s)
	}
	return sessions, nil
}

func (a *App) sortedSessions() []sessionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]sessionState, 0, len(a.sessions))
	for _, st := range a.sessions {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

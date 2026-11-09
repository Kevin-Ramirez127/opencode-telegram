package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/config"
	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
	"opencode-telegram/internal/whitelist"
)

type fakeTelegram struct {
	mu     sync.Mutex
	texts  []string
	nextID int
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if method == "getMe" {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
		return
	}
	_ = r.ParseForm()
	text := r.FormValue("text")
	f.mu.Lock()
	if text != "" {
		f.texts = append(f.texts, text)
	}
	f.nextID++
	id := f.nextID
	f.mu.Unlock()

	_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":`+itoa(id)+`,"date":1,"chat":{"id":123,"type":"private"},"text":"ok"}}`)
}

func (f *fakeTelegram) lastText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.texts) == 0 {
		return ""
	}
	return f.texts[len(f.texts)-1]
}

func (f *fakeTelegram) allText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.texts, "\n---\n")
}

type fakeOpenCode struct {
	mu            sync.Mutex
	formAnswer    map[string]any
	permission    string
	sessionTitle  string
	lastAssistant string
}

func (f *fakeOpenCode) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_1":
		writeFakeJSON(w, map[string]any{"data": map[string]any{
			"id":       "ses_1",
			"title":    f.sessionTitle,
			"outcome":  "succeeded",
			"location": map[string]any{"directory": "/tmp/project"},
			"time":     map[string]any{"created": 1, "updated": 61001},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_1/message":
		writeFakeJSON(w, map[string]any{"data": []any{map[string]any{
			"id":   "msg_1",
			"type": "assistant",
			"time": map[string]any{"created": 2},
			"content": []any{map[string]any{
				"type": "text",
				"text": f.lastAssistant,
			}},
		}}})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/ses_1/form/"):
		var body struct {
			Answer map[string]any `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.formAnswer = body.Answer
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/ses_1/permission/"):
		var body struct {
			Decision string `json:"decision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.permission = body.Decision
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func newIntegrationApp(t *testing.T) (*App, *fakeTelegram, *fakeOpenCode) {
	t.Helper()
	ftg := &fakeTelegram{}
	tgSrv := httptest.NewServer(http.HandlerFunc(ftg.handler))
	t.Cleanup(tgSrv.Close)

	foc := &fakeOpenCode{sessionTitle: "Build the thing", lastAssistant: "All changes are in."}
	ocSrv := httptest.NewServer(http.HandlerFunc(foc.handler))
	t.Cleanup(ocSrv.Close)

	dir := t.TempDir()
	allowPath := filepath.Join(dir, "allowlist.json")
	if err := os.WriteFile(allowPath, []byte(`{"users":[123],"chats":[123]}`), 0o600); err != nil {
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
	on := true
	if _, err := st.Patch(settings.Patch{Notifications: &settings.NotificationsPatch{Enabled: &on}}); err != nil {
		t.Fatal(err)
	}

	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("123:TEST", tgSrv.URL+"/bot%s/%s")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:                  dir,
		AllowlistFile:            allowPath,
		SettingsFile:             filepath.Join(dir, "settings.json"),
		ListenAddr:               "127.0.0.1:0",
		OpenCodeURL:              ocSrv.URL,
		OpenCodeURLExplicit:      true,
		OpenCodePasswordExplicit: true,
	}
	a := New(cfg, bot, occlient.New(ocSrv.URL, "", 5*time.Second), wl, st, "test")
	return a, ftg, foc
}

func findAction(t *testing.T, a *App, kind, value string) (string, action) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, act := range a.actions {
		if act.Kind == kind && (value == "" || act.Value == value) {
			return token, *act
		}
	}
	t.Fatalf("action kind=%q value=%q not found", kind, value)
	return "", action{}
}

func TestSessionDoneNotification(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	all := ftg.allText()
	for _, want := range []string{"session finished", "Build the thing", "All changes are in."} {
		if !strings.Contains(all, want) {
			t.Fatalf("notification missing %q:\n%s", want, all)
		}
	}
	if st := a.sessionSnapshot("ses_1"); st.Busy || st.Outcome != "succeeded" {
		t.Fatalf("state = %+v", st)
	}
}

func TestFormOptionFlow(t *testing.T) {
	a, _, foc := newIntegrationApp(t)
	form := `{"form":{"id":"frm_1","sessionID":"ses_1","title":"Pick a mode","fields":[` +
		`{"key":"mode","title":"Mode","type":"string","required":true,"options":[` +
		`{"value":"fast","label":"Fast"},{"value":"safe","label":"Safe"}]}]}}`
	a.handleEvent(occlient.Event{Type: "form.created", Data: []byte(form)})
	if a.formFlow("frm_1") == nil {
		t.Fatal("form flow was not created")
	}
	token, _ := findAction(t, a, "form_opt", "safe")
	a.handleCallback(123, callbackData(token))

	foc.mu.Lock()
	answer := foc.formAnswer
	foc.mu.Unlock()
	if answer["mode"] != "safe" {
		t.Fatalf("form answer = %#v", answer)
	}
	if a.formFlow("frm_1") != nil {
		t.Fatal("form flow should be removed after submission")
	}
}

func TestCustomFormAnswerRequiresSetting(t *testing.T) {
	a, _, foc := newIntegrationApp(t)
	form := `{"form":{"id":"frm_2","sessionID":"ses_1","title":"Free text","fields":[` +
		`{"key":"note","title":"Note","type":"string","required":true}]}}`

	a.handleEvent(occlient.Event{Type: "form.created", Data: []byte(form)})
	a.mu.Lock()
	for _, act := range a.actions {
		if act.Kind == "form_custom" {
			a.mu.Unlock()
			t.Fatal("custom answer button must not exist while the setting is off")
		}
	}
	a.mu.Unlock()
	a.handleFormSettled("frm_2", "cancelled")

	on := true
	if _, err := a.settings.Patch(settings.Patch{Security: &settings.SecurityPatch{AllowCustomFormAnswers: &on}}); err != nil {
		t.Fatal(err)
	}
	form = `{"form":{"id":"frm_3","sessionID":"ses_1","title":"Free text","fields":[` +
		`{"key":"note","title":"Note","type":"string","required":true}]}}`
	a.handleEvent(occlient.Event{Type: "form.created", Data: []byte(form)})
	token, act := findAction(t, a, "form_custom", "")
	if act.FormID != "frm_3" {
		t.Fatalf("action = %+v", act)
	}
	a.handleCallback(123, callbackData(token))
	a.handleFormText(123, "typed answer")

	foc.mu.Lock()
	answer := foc.formAnswer
	foc.mu.Unlock()
	if answer["note"] != "typed answer" {
		t.Fatalf("form answer = %#v", answer)
	}
}

func TestDoneNotificationSuppressedWhileQuestionPending(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	form := `{"form":{"id":"frm_wait","sessionID":"ses_1","title":"Need input","fields":[` +
		`{"key":"ok","title":"OK?","type":"boolean","required":true}]}}`
	a.handleEvent(occlient.Event{Type: "form.created", Data: []byte(form)})
	a.handleEvent(occlient.Event{Type: "session.execution.succeeded", Data: []byte(`{"sessionID":"ses_1"}`)})
	if strings.Contains(ftg.allText(), "session finished") {
		t.Fatal("session-finished notification must wait while a question is pending")
	}

	a.handleFormSettled("frm_wait", "cancelled")
	a.handleEvent(occlient.Event{Type: "session.execution.succeeded", Data: []byte(`{"sessionID":"ses_1"}`)})
	if !strings.Contains(ftg.allText(), "session finished") {
		t.Fatal("expected a session-finished notification after the question settled")
	}
}

func TestPermissionFlow(t *testing.T) {
	a, _, foc := newIntegrationApp(t)
	a.handleEvent(occlient.Event{
		Type: "permission.asked",
		Data: []byte(`{"id":"per_1","sessionID":"ses_1","action":"bash","resources":["rm -rf /tmp/x"]}`),
	})
	token, _ := findAction(t, a, "perm", "")
	a.handleCallback(123, callbackData(token))

	foc.mu.Lock()
	decision := foc.permission
	foc.mu.Unlock()
	if decision != "once" && decision != "always" && decision != "reject" {
		t.Fatalf("permission decision = %q", decision)
	}
	a.mu.Lock()
	for _, act := range a.actions {
		if act.Kind == "perm" && act.RequestID == "per_1" {
			a.mu.Unlock()
			t.Fatal("permission actions should be removed after a reply")
		}
	}
	a.mu.Unlock()
}

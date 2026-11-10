package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/config"
	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
	"opencode-telegram/internal/whitelist"
)

type fakeTelegram struct {
	mu         sync.Mutex
	texts      []string
	richTexts  []string
	deleted    []int
	lastMarkup string
	nextID     int
	commands   []tgbotapi.BotCommand
	registered []tgbotapi.BotCommand
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch method {
	case "getMe":
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"test_bot"}}`)
		return
	case "getMyCommands":
		f.mu.Lock()
		commands := append([]tgbotapi.BotCommand(nil), f.commands...)
		f.mu.Unlock()
		result, _ := json.Marshal(commands)
		_, _ = io.WriteString(w, `{"ok":true,"result":`+string(result)+`}`)
		return
	case "setMyCommands":
		_ = r.ParseForm()
		var commands []tgbotapi.BotCommand
		_ = json.Unmarshal([]byte(r.FormValue("commands")), &commands)
		f.mu.Lock()
		f.registered = commands
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		return
	case "deleteMessage":
		_ = r.ParseForm()
		var messageID int
		_, _ = fmt.Sscanf(r.FormValue("message_id"), "%d", &messageID)
		f.mu.Lock()
		f.deleted = append(f.deleted, messageID)
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		return
	}
	_ = r.ParseForm()
	text := r.FormValue("text")
	rich := r.FormValue("rich_message")
	markup := r.FormValue("reply_markup")
	if text == "" && rich != "" {
		var content struct {
			Markdown string `json:"markdown"`
			HTML     string `json:"html"`
		}
		if json.Unmarshal([]byte(rich), &content) == nil {
			text = content.Markdown
			if text == "" {
				text = content.HTML
			}
		}
	}
	f.mu.Lock()
	if text != "" {
		f.texts = append(f.texts, text)
	}
	if rich != "" {
		f.richTexts = append(f.richTexts, text)
	}
	if markup != "" {
		f.lastMarkup = markup
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

func (f *fakeTelegram) allTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

func (f *fakeTelegram) allRichTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.richTexts...)
}

func (f *fakeTelegram) deletedIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.deleted...)
}

func (f *fakeTelegram) lastReplyMarkup() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMarkup
}

func (f *fakeTelegram) registeredCommands() []tgbotapi.BotCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tgbotapi.BotCommand(nil), f.registered...)
}

type fakeOpenCode struct {
	mu            sync.Mutex
	formAnswer    map[string]any
	permission    string
	sessionTitle  string
	lastAssistant string
	lastParts     []string
	reasoning     string
	generated     string
	summaryPrompt string
	prompt        string
}

func (f *fakeOpenCode) message() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	texts := f.lastParts
	if len(texts) == 0 {
		texts = []string{f.lastAssistant}
	}
	content := make([]any, 0, len(texts)+1)
	if f.reasoning != "" {
		content = append(content, map[string]any{"type": "reasoning", "text": f.reasoning})
	}
	for _, text := range texts {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	return map[string]any{
		"id":      "msg_1",
		"type":    "assistant",
		"time":    map[string]any{"created": 2},
		"content": content,
	}
}

func (f *fakeOpenCode) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_1":
		writeFakeJSON(w, map[string]any{"data": map[string]any{
			"id":       "ses_1",
			"title":    f.sessionTitle,
			"outcome":  "succeeded",
			"location": map[string]any{"directory": "/tmp/project"},
			"time":     map[string]any{"created": 1, "busy": 58001, "updated": 61001},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_1/message":
		writeFakeJSON(w, map[string]any{"data": []any{f.message()}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_1/message/msg_1":
		writeFakeJSON(w, map[string]any{"data": f.message()})
	case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_1/generate":
		var body struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.summaryPrompt = body.Prompt
		text := f.generated
		f.mu.Unlock()
		writeFakeJSON(w, map[string]any{"data": map[string]any{"text": text}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_1/prompt":
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.prompt = body.Text
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
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

func TestSendAndEditUseRichMessages(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)

	a.send(123, "**bold** output")
	a.editMarkup(123, 1, "_edited_ output", nil)

	rich := ftg.allRichTexts()
	if len(rich) != 2 {
		t.Fatalf("rich messages = %v, want 2", rich)
	}
	if rich[0] != "**bold** output" || rich[1] != "_edited_ output" {
		t.Fatalf("rich messages = %v", rich)
	}
}

func TestEditMarkupClearsButtonsWithArray(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)

	a.editMarkup(123, 42, "Operation aborted.", nil)

	if got := ftg.lastReplyMarkup(); got != `{"inline_keyboard":[]}` {
		t.Fatalf("reply markup = %s, want an explicit empty array", got)
	}
}

func TestSessionDoneNotification(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	all := ftg.allText()
	for _, want := range []string{"session finished", "Build the thing", "All changes are in.", "Duration: 3s"} {
		if !strings.Contains(all, want) {
			t.Fatalf("notification missing %q:\n%s", want, all)
		}
	}
	if st := a.sessionSnapshot("ses_1"); st.Busy || st.Outcome != "succeeded" {
		t.Fatalf("state = %+v", st)
	}
}

func TestSessionDoneNotificationUTF8(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	foc.lastAssistant = strings.Repeat("é", 2000) // 4000 bytes, 2 bytes per rune
	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	if !strings.Contains(ftg.allText(), "session finished") {
		t.Fatalf("notification missing heading:\n%s", ftg.allText())
	}
	for _, text := range ftg.allTexts() {
		if !utf8.ValidString(text) {
			t.Fatalf("notification is not valid UTF-8: %q", text)
		}
		if len(text) > maxTelegramText {
			t.Fatalf("notification length = %d, want <= %d", len(text), maxTelegramText)
		}
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

func TestLongMessageSplitsTextBlocks(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	blockA := strings.Repeat("a", 3000)
	blockB := strings.Repeat("b", 3000)
	foc.lastParts = []string{blockA, blockB}

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})

	all := ftg.allText()
	if !strings.Contains(all, blockA) || !strings.Contains(all, blockB) {
		t.Fatalf("split notification lost a text block:\n%s", all)
	}
	for _, text := range ftg.allTexts() {
		if len(text) > maxTelegramText {
			t.Fatalf("message length = %d, want <= %d", len(text), maxTelegramText)
		}
	}
}

func TestLongMessageSummaryButton(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	mode := "summary"
	if _, err := a.settings.Patch(settings.Patch{Notifications: &settings.NotificationsPatch{
		LongMessageMode: &mode,
	}}); err != nil {
		t.Fatal(err)
	}
	foc.lastParts = []string{strings.Repeat("x", 5000)}
	foc.generated = "short summary"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	token, _ := findAction(t, a, "summarize_output", "")
	a.handleCallback(123, callbackData(token))

	if !strings.Contains(ftg.allText(), "short summary") {
		t.Fatalf("summary was not delivered:\n%s", ftg.allText())
	}
	foc.mu.Lock()
	prompt := foc.summaryPrompt
	foc.mu.Unlock()
	if !strings.Contains(prompt, "xxxxx") {
		t.Fatalf("summary prompt did not contain the output: %q", prompt)
	}
}

func TestSummarizeOutputButtonOnNormalMessage(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	foc.lastAssistant = "short message"
	foc.generated = "short summary"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	token, _ := findAction(t, a, "summarize_output", "")
	a.handleCallback(123, callbackData(token))

	if !strings.Contains(ftg.allText(), "short message") || !strings.Contains(ftg.allText(), "short summary") {
		t.Fatalf("missing message or summary:\n%s", ftg.allText())
	}
}

func TestShowThinkingButton(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	foc.reasoning = "raw secret reasoning"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	token, _ := findAction(t, a, "show_thinking", "")
	a.handleCallback(123, callbackData(token))

	if !strings.Contains(ftg.allText(), "raw secret reasoning") {
		t.Fatalf("raw thinking was not delivered:\n%s", ftg.allText())
	}
}

func TestShowThinkingSendsAndDeletesLoading(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	foc.reasoning = "raw secret reasoning"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	token, _ := findAction(t, a, "show_thinking", "")
	a.handleCallback(123, callbackData(token))

	all := ftg.allText()
	if !strings.Contains(all, "Loading thinking") || !strings.Contains(all, "raw secret reasoning") {
		t.Fatalf("missing loading or thinking content:\n%s", all)
	}
	if got := ftg.deletedIDs(); len(got) != 1 {
		t.Fatalf("deleted messages = %v, want one loading message", got)
	}
}

func TestRawThinkingOffersSummarizeButton(t *testing.T) {
	a, ftg, foc := newIntegrationApp(t)
	foc.reasoning = "raw reasoning to summarize"
	foc.generated = "summarized thinking"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	showToken, _ := findAction(t, a, "show_thinking", "")
	a.handleCallback(123, callbackData(showToken))
	if !strings.Contains(ftg.allText(), "raw reasoning to summarize") {
		t.Fatalf("raw thinking was not delivered:\n%s", ftg.allText())
	}

	sumToken, _ := findAction(t, a, "summarize_thinking", "")
	a.handleCallback(123, callbackData(sumToken))
	if !strings.Contains(ftg.allText(), "summarized thinking") {
		t.Fatalf("thinking summary was not delivered:\n%s", ftg.allText())
	}
	foc.mu.Lock()
	prompt := foc.summaryPrompt
	foc.mu.Unlock()
	if !strings.Contains(prompt, "raw reasoning to summarize") {
		t.Fatalf("summary prompt did not contain the reasoning: %q", prompt)
	}
}

func TestSummarizeThinkingCanBeDisabled(t *testing.T) {
	a, _, foc := newIntegrationApp(t)
	off := false
	if _, err := a.settings.Patch(settings.Patch{Notifications: &settings.NotificationsPatch{
		ShowSummarizeThinking: &off,
	}}); err != nil {
		t.Fatal(err)
	}
	foc.reasoning = "raw reasoning"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	showToken, _ := findAction(t, a, "show_thinking", "")
	a.handleCallback(123, callbackData(showToken))

	a.mu.Lock()
	defer a.mu.Unlock()
	for _, act := range a.actions {
		if act.Kind == "summarize_thinking" {
			t.Fatal("summarize thinking action must not be created when disabled")
		}
	}
}

func TestSummaryActionsAreSingleUse(t *testing.T) {
	a, _, foc := newIntegrationApp(t)
	foc.generated = "summary"

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})
	token, _ := findAction(t, a, "summarize_output", "")
	a.handleCallback(123, callbackData(token))

	if a.claimAction(token, 123) {
		t.Fatal("summary action must be single-use per chat")
	}
	if !a.claimAction(token, 456) {
		t.Fatal("another chat must still be able to use its own button")
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

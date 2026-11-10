package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
)

const (
	simulationSessionID = "ses_sim"
	simulationMessageID = "msg_sim_answer"
)

// SimulationScenarios lists the manual QA scenarios understood by
// App.RunSimulation. They never touch a real OpenCode session.
func SimulationScenarios() []string {
	return []string{
		"text", "done", "failed", "interrupted",
		"question1", "question2", "question3", "permission", "retry", "all",
	}
}

// RunSimulation serves premade OpenCode conversations to the real Telegram
// modules. It keeps listening for callback presses until ctx is cancelled, so
// question, permission, summary, and thinking buttons can be exercised.
func (a *App) RunSimulation(ctx context.Context, scenario string) error {
	server := newSimulationServer()
	defer server.Close()

	// Never call a real OpenCode service for a simulated conversation.
	a.oc.SetEndpoint(server.URL(), "")
	a.explicitURL = true
	a.explicitPassword = true

	simSettings, cleanup, err := newSimulationSettings()
	if err != nil {
		return err
	}
	defer cleanup()
	a.settings = simSettings

	a.rememberSession(server.session)
	if len(a.notifyChats()) == 0 {
		return fmt.Errorf("no allowlisted chats; add one with \"opencode-telegram config --adduser <id>\"")
	}
	// Telegram updates are single-consumer. A running daemon would eat the
	// callback presses before this process sees them.
	log.Print("simulation: if the real daemon is running, stop it first (opencode-telegram stop)")
	go a.telegramLoop(ctx)

	for _, name := range expandSimulationScenario(scenario) {
		if err := a.runSimulationScenario(name, server); err != nil {
			return err
		}
		log.Printf("simulation: sent %q to %d chat(s)", name, len(a.notifyChats()))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(700 * time.Millisecond):
		}
	}

	log.Print("simulation: messages sent. Buttons stay active while this process runs; press Ctrl+C to exit.")
	<-ctx.Done()
	return nil
}

func expandSimulationScenario(scenario string) []string {
	scenario = strings.TrimSpace(scenario)
	if scenario == "" || scenario == "all" {
		return []string{"text", "done", "failed", "interrupted", "question1", "question2", "question3", "permission", "retry"}
	}
	parts := strings.Split(scenario, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (a *App) runSimulationScenario(name string, server *simulationServer) error {
	switch name {
	case "text":
		for _, chatID := range a.notifyChats() {
			a.send(chatID, simulationRichText())
		}
		return nil

	case "done":
		a.clearNotified("session:" + simulationSessionID)
		a.handleEvent(occlient.Event{
			Type: "session.execution.succeeded",
			Data: []byte(`{"sessionID":"` + simulationSessionID + `"}`),
		})
		return nil

	case "failed":
		a.clearNotified("session:" + simulationSessionID)
		a.handleEvent(occlient.Event{
			Type: "session.execution.failed",
			Data: []byte(`{"sessionID":"` + simulationSessionID + `","error":{"type":"provider_error","message":"Simulated provider failure: rate limit exceeded."}}`),
		})
		return nil

	case "interrupted":
		a.clearNotified("session:" + simulationSessionID)
		a.handleEvent(occlient.Event{
			Type: "session.execution.interrupted",
			Data: []byte(`{"sessionID":"` + simulationSessionID + `","reason":"simulated interrupt"}`),
		})
		return nil

	case "question1", "question2", "question3":
		n := int(name[len(name)-1] - '0')
		form := simulationForm(n)
		a.clearNotified("form:" + form.ID)
		a.rememberSession(server.session)
		a.handleFormCreated(form)
		return nil

	case "permission":
		a.clearNotified("perm:perm_sim")
		a.handleEvent(occlient.Event{
			Type: "permission.asked",
			Data: []byte(`{"id":"perm_sim","sessionID":"` + simulationSessionID + `","action":"bash","resources":["git status","rm -rf /tmp/opencode-telegram-sim"],"message":"Simulated permission request."}`),
		})
		return nil

	case "retry":
		a.clearNotified("retry:" + simulationSessionID)
		a.notifyRetry(simulationSessionID, 1, "Simulated provider retry. Next attempt in 5 seconds.")
		return nil

	default:
		return fmt.Errorf("unknown simulation scenario %q (try: %s)", name, strings.Join(SimulationScenarios(), ", "))
	}
}

func newSimulationSettings() (*settings.Store, func(), error) {
	dir, err := os.MkdirTemp("", "opencode-telegram-sim-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	store, err := settings.Open(filepath.Join(dir, "settings.json"))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	next := settings.Defaults()
	next.Notifications.Enabled = true
	next.Notifications.SessionDone = true
	next.Notifications.SessionFailed = true
	next.Notifications.SessionInterrupted = true
	next.Notifications.QuestionAsked = true
	next.Notifications.PermissionAsked = true
	next.Notifications.Retry = true
	next.Notifications.IncludeLastMessage = true
	next.Notifications.IncludeDirectory = true
	next.Notifications.ShowSummarizeThinking = true
	next.Security.AllowCustomFormAnswers = true
	next.Security.AllowModelSwitching = true
	if _, err := store.Replace(next); err != nil {
		cleanup()
		return nil, nil, err
	}
	return store, cleanup, nil
}

func simulationRichText() string {
	return strings.Join([]string{
		"# Simulated OpenCode output",
		"",
		"This message exercises **bold**, _italic_, `inline code`, and [links](https://opencode.ai).",
		"",
		"## Changes",
		"",
		"- Added the request handler",
		"- Preserved UTF-8 output",
		"- Updated the documentation",
		"",
		"| Module | Status |",
		"|:-------|-------:|",
		"| Telegram | **ready** |",
		"| OpenCode | connected |",
		"",
		"```go",
		"func Example() string {",
		`	return "rich Telegram text"`,
		"}",
		"```",
		"",
		"> This is a simulated conversation. No real OpenCode session was created.",
	}, "\n")
}

func simulationForm(fieldCount int) occlient.Form {
	fields := []occlient.FormField{
		{
			Key:      "mode",
			Title:    "Which mode should the simulated agent use?",
			Type:     "string",
			Required: true,
			Options: []occlient.FormOption{
				{Value: "fast", Label: "Fast", Description: "Prefer speed"},
				{Value: "safe", Label: "Safe", Description: "Prefer review"},
			},
		},
		{
			Key:      "notify",
			Title:    "Notify when the simulated run finishes?",
			Type:     "boolean",
			Required: true,
		},
		{
			Key:         "features",
			Title:       "Pick features to include",
			Description: "Multiple choices are allowed.",
			Type:        "multiselect",
			Required:    false,
			Options: []occlient.FormOption{
				{Value: "tests", Label: "Tests"},
				{Value: "docs", Label: "Docs"},
				{Value: "telemetry", Label: "Telemetry"},
			},
		},
	}
	if fieldCount < 1 {
		fieldCount = 1
	}
	if fieldCount > len(fields) {
		fieldCount = len(fields)
	}
	return occlient.Form{
		ID:        fmt.Sprintf("frm_sim_q%d", fieldCount),
		SessionID: simulationSessionID,
		Title:     fmt.Sprintf("Simulated question with %d field(s)", fieldCount),
		Fields:    fields[:fieldCount],
	}
}

// ---------------------------------------------------------------------------
// Fake OpenCode server
// ---------------------------------------------------------------------------

type simulationServer struct {
	mu          sync.Mutex
	srv         *httptest.Server
	session     occlient.Session
	user        occlient.Message
	assistant   occlient.Message
	formAnswers map[string]map[string]any
	permission  string
}

func newSimulationServer() *simulationServer {
	now := time.Now().UnixMilli()
	user := occlient.Message{
		ID:   "msg_sim_prompt",
		Type: "user",
		Text: "Simulate a conversation without creating a real OpenCode session.",
	}
	user.Time.Created = now - 12_500

	assistant := occlient.Message{
		ID:   simulationMessageID,
		Type: "assistant",
		Content: []occlient.ContentPart{
			{
				Type: "reasoning",
				Text: "The user wants a manual check of the Telegram modules. " +
					"I should produce a rich markdown answer with a table, a list, code, " +
					"and enough text for the thinking and summary buttons.",
			},
			{Type: "text", Text: simulationRichText()},
		},
	}
	assistant.Time.Created = now - 12_000
	assistant.Time.Completed = now - 1_000

	s := &simulationServer{
		session: occlient.Session{
			ID:       simulationSessionID,
			Title:    "Simulated conversation",
			Agent:    "build",
			Model:    occlient.ModelRef{ID: "deepseek-v4.1-flash", ProviderID: "opencode-go"},
			Outcome:  "succeeded",
			Location: occlient.Location{Directory: "/tmp/opencode-telegram-sim"},
			Time: occlient.SessionTime{
				Created: now - 10*60*1000,
				Busy:    now - 12_500,
				Updated: now - 1_000,
				Idle:    now,
			},
		},
		user:        user,
		assistant:   assistant,
		formAnswers: make(map[string]map[string]any),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *simulationServer) URL() string { return s.srv.URL }

func (s *simulationServer) Close() { s.srv.Close() }

func (s *simulationServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/info":
		writeSimulationJSON(w, map[string]any{"data": map[string]any{"version": "simulated"}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/active":
		writeSimulationJSON(w, map[string]any{"data": map[string]any{}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session":
		writeSimulationJSON(w, map[string]any{"data": []any{s.session}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/"+simulationSessionID:
		writeSimulationJSON(w, map[string]any{"data": s.session})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/"+simulationSessionID+"/message":
		kind := r.URL.Query().Get("type")
		messages := make([]occlient.Message, 0, 2)
		switch kind {
		case "user":
			messages = append(messages, s.user)
		case "assistant":
			messages = append(messages, s.assistant)
		default:
			messages = append(messages, s.assistant, s.user)
		}
		writeSimulationJSON(w, map[string]any{"data": messages})
	case r.Method == http.MethodGet && r.URL.Path == "/api/session/"+simulationSessionID+"/message/"+simulationMessageID:
		writeSimulationJSON(w, map[string]any{"data": s.assistant})
	case r.Method == http.MethodPost && r.URL.Path == "/api/session/"+simulationSessionID+"/generate":
		writeSimulationJSON(w, map[string]any{"data": map[string]any{
			"text": "**Simulated summary**\n\n- The rich message rendered correctly.\n- The action completed without a real OpenCode session.",
		}})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/"+simulationSessionID+"/form/"):
		var body struct {
			Answer map[string]any `json:"answer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.formAnswers[r.URL.Path] = body.Answer
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/"+simulationSessionID+"/permission/"):
		var body struct {
			Decision string `json:"decision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.permission = body.Decision
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/api/session/"+simulationSessionID+"/prompt":
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func writeSimulationJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

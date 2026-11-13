package app

import (
	"strings"
	"testing"

	"opencode-telegram/internal/occlient"
)

func testLiveDelta(t *testing.T, a *App, eventType, data string) {
	t.Helper()
	a.handleEvent(occlient.Event{Type: eventType, Data: []byte(data)})
}

func TestLiveCommandStreamsAndStopsWhenSessionEnds(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.setBinding(123, "ses_1")

	a.handleCommand(123, 123, "/live")
	if !a.hasLive(123) {
		t.Fatal("/live must start a live stream")
	}

	testLiveDelta(t, a, "session.text.started", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0}`)
	testLiveDelta(t, a, "session.text.delta", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"Hello "}`)
	testLiveDelta(t, a, "session.text.delta", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"world"}`)

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_1"}`),
	})

	if a.hasLive(123) {
		t.Fatal("session completion must stop the live stream")
	}
	all := ftg.allText()
	if !strings.Contains(all, "Hello world") {
		t.Fatalf("live output missing the streamed text:\n%s", all)
	}
	if !strings.Contains(all, "Live ended") {
		t.Fatalf("live output missing the end marker:\n%s", all)
	}
}

func TestStopLiveCommandStopsStream(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.setBinding(123, "ses_1")

	a.handleCommand(123, 123, "/live")
	testLiveDelta(t, a, "session.text.delta", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"partial output"}`)

	a.handleCommand(123, 123, "/stoplive")
	if a.hasLive(123) {
		t.Fatal("/stoplive must stop the live stream")
	}
	all := ftg.allText()
	if !strings.Contains(all, "partial output") || !strings.Contains(all, "Live ended") {
		t.Fatalf("live output was not finalized:\n%s", all)
	}
}

func TestUnbindStopsLive(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.setBinding(123, "ses_1")

	a.handleCommand(123, 123, "/live")
	testLiveDelta(t, a, "session.text.delta", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"before unbind"}`)

	a.handleCommand(123, 123, "/unbind")
	if a.hasLive(123) {
		t.Fatal("/unbind must stop the live stream")
	}
	if a.binding(123) != "" {
		t.Fatalf("binding = %q, want empty", a.binding(123))
	}
	if all := ftg.allText(); !strings.Contains(all, "before unbind") || !strings.Contains(all, "Live ended") {
		t.Fatalf("live output was not finalized:\n%s", all)
	}
}

func TestLiveRendersShellCommandAsCodeBlock(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.setBinding(123, "ses_1")

	a.handleCommand(123, 123, "/live")
	testLiveDelta(t, a, "session.tool.input.started", `{"sessionID":"ses_1","assistantMessageID":"msg_1","id":"tool_1","name":"shell"}`)
	testLiveDelta(t, a, "session.shell.started", `{"sessionID":"ses_1","shell":{"id":"sh_1","status":"running","command":"printf 'hello'\necho done","cwd":"/tmp","shell":"/bin/bash","file":"/tmp/script.sh","metadata":{},"time":{"started":1}}}`)
	testLiveDelta(t, a, "session.tool.success", `{"sessionID":"ses_1","assistantMessageID":"msg_1","id":"tool_1","content":[],"executed":true}`)

	a.handleCommand(123, 123, "/stoplive")

	all := ftg.allText()
	if !strings.Contains(all, "```sh\nprintf 'hello'\necho done\n```") {
		t.Fatalf("shell command missing from live output:\n%s", all)
	}
	// The shell tool itself must not also be rendered as a bare name.
	if strings.Contains(all, "🔧 shell\n✅") {
		t.Fatalf("shell tool rendered without its command:\n%s", all)
	}
}

func TestLiveRequiresBinding(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)

	a.handleCommand(123, 123, "/live")
	if a.hasLive(123) {
		t.Fatal("live must not start without a bound session")
	}
	if !strings.Contains(ftg.lastText(), "No session bound") {
		t.Fatalf("response = %q", ftg.lastText())
	}
}

func TestLiveRendersLongOutputInChunks(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	a.setBinding(123, "ses_1")

	a.handleCommand(123, 123, "/live")
	chunk := strings.Repeat("x", maxTelegramText)
	// Two full chunks plus a partial third must produce separate messages.
	for i := 0; i < 2; i++ {
		testLiveDelta(t, a, "session.text.delta", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"`+chunk+`"}`)
	}
	testLiveDelta(t, a, "session.text.delta", `{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"delta":"tail"}`)

	a.handleCommand(123, 123, "/stoplive")

	if n := len(ftg.allTexts()); n < 4 {
		t.Fatalf("messages = %d, want separate chunks plus the stop acknowledgement", n)
	}
	if all := ftg.allText(); !strings.Contains(all, "tail") || !strings.Contains(all, "Live ended") {
		t.Fatalf("chunked live output is incomplete:\n%s", all)
	}
}

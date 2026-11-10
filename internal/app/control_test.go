package app

import (
	"context"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestExitCommandRequiresConfirmation(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)

	a.handleCommand(123, 123, "/exit")

	select {
	case <-a.Done():
		t.Fatal("daemon shut down before the confirmation button was pressed")
	default:
	}
	if got := ftg.lastText(); got == "" || !strings.Contains(got, "Stop the opencode-telegram daemon") {
		t.Fatalf("confirmation text = %q", got)
	}

	token, act := findAction(t, a, "daemon_exit", "")
	if act.ChatID != 123 {
		t.Fatalf("action chat = %d, want 123", act.ChatID)
	}
	a.handleCallbackFrom(123, 42, callbackData(token))

	select {
	case <-a.Done():
	case <-time.After(time.Second):
		t.Fatal("confirming /exit did not request shutdown")
	}
}

func TestExitCommandCancelKeepsDaemonRunning(t *testing.T) {
	a, _, _ := newIntegrationApp(t)

	a.handleCommand(123, 123, "/exit")
	token, _ := findAction(t, a, "confirm_cancel", "")
	a.handleCallbackFrom(123, 42, callbackData(token))

	select {
	case <-a.Done():
		t.Fatal("cancelling /exit stopped the daemon")
	default:
	}
}

func TestPowerOffRequiresConfirmation(t *testing.T) {
	a, _, _ := newIntegrationApp(t)
	called := make(chan struct{}, 1)
	a.poweroff = func(context.Context) error {
		called <- struct{}{}
		return nil
	}

	a.handleCommand(123, 123, "/poweroff")

	select {
	case <-called:
		t.Fatal("computer power-off started before confirmation")
	case <-time.After(50 * time.Millisecond):
	}

	token, act := findAction(t, a, "poweroff", "")
	if act.ChatID != 123 {
		t.Fatalf("action chat = %d, want 123", act.ChatID)
	}
	a.handleCallbackFrom(123, 42, callbackData(token))

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("confirming /poweroff did not run the power-off command")
	}
}

func TestPowerOffCancelDoesNotRun(t *testing.T) {
	a, _, _ := newIntegrationApp(t)
	called := make(chan struct{}, 1)
	a.poweroff = func(context.Context) error {
		called <- struct{}{}
		return nil
	}

	a.handleCommand(123, 123, "/poweroff")
	token, _ := findAction(t, a, "confirm_cancel", "")
	a.handleCallbackFrom(123, 42, callbackData(token))

	select {
	case <-called:
		t.Fatal("cancelling /poweroff ran the power-off command")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEnsureBotCommandsAddsMissingWithoutReplacing(t *testing.T) {
	a, ftg, _ := newIntegrationApp(t)
	ftg.commands = []tgbotapi.BotCommand{
		{Command: "start", Description: "Custom start description"},
		{Command: "custom", Description: "Keep this command"},
	}

	a.ensureBotCommands()

	got := ftg.registeredCommands()
	byName := make(map[string]string, len(got))
	for _, cmd := range got {
		byName[cmd.Command] = cmd.Description
	}
	if byName["start"] != "Custom start description" {
		t.Fatalf("existing command was replaced: %q", byName["start"])
	}
	if byName["custom"] != "Keep this command" {
		t.Fatalf("unrelated command was lost: %q", byName["custom"])
	}
	for _, name := range []string{"exit", "poweroff"} {
		if byName[name] == "" {
			t.Fatalf("command %q was not added: %v", name, byName)
		}
	}
}

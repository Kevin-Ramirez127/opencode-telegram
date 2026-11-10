package app

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// botCommands is the command menu advertised to Telegram clients. Existing
// commands with the same name are left untouched, so operators can keep their
// own descriptions and any extra commands they registered with BotFather.
var botCommands = []tgbotapi.BotCommand{
	{Command: "start", Description: "Show the command list"},
	{Command: "help", Description: "Show the command list"},
	{Command: "status", Description: "Daemon and OpenCode status"},
	{Command: "sessions", Description: "List recent sessions and bind one"},
	{Command: "bind", Description: "Bind this chat to a session"},
	{Command: "unbind", Description: "Remove the session binding"},
	{Command: "agents", Description: "List or switch the bound session's agent"},
	{Command: "models", Description: "List all available chat models"},
	{Command: "usage", Description: "Show OpenCode Go and Console usage limits"},
	{Command: "interrupt", Description: "Stop the bound session's current run"},
	{Command: "notify", Description: "Enable or disable notifications"},
	{Command: "settings", Description: "Show notification settings"},
	{Command: "test", Description: "Send a test notification"},
	{Command: "id", Description: "Show your Telegram user and chat ids"},
	{Command: "exit", Description: "Stop the opencode-telegram daemon"},
	{Command: "poweroff", Description: "Power off this computer"},
}

// RequestShutdown asks main to stop the daemon. It is safe to call repeatedly.
func (a *App) RequestShutdown() {
	a.quitOnce.Do(func() { close(a.quit) })
}

// Done is closed once a Telegram command requested daemon shutdown.
func (a *App) Done() <-chan struct{} {
	return a.quit
}

// ensureBotCommands adds missing commands to the bot menu without replacing
// commands that are already registered.
func (a *App) ensureBotCommands() {
	existing, err := a.bot.GetMyCommands()
	if err != nil {
		log.Printf("could not read bot commands: %v", err)
	}

	known := make(map[string]bool, len(existing))
	for _, cmd := range existing {
		known[cmd.Command] = true
	}

	merged := append([]tgbotapi.BotCommand(nil), existing...)
	for _, cmd := range botCommands {
		if known[cmd.Command] {
			continue
		}
		merged = append(merged, cmd)
		known[cmd.Command] = true
	}
	if len(merged) == len(existing) {
		return
	}

	if _, err := a.bot.Request(tgbotapi.NewSetMyCommands(merged...)); err != nil {
		log.Printf("could not register bot commands: %v", err)
		return
	}
	log.Printf("registered %d bot commands", len(merged))
}

func (a *App) commandExit(chatID int64) {
	token := a.addAction(action{Kind: "daemon_exit", ChatID: chatID})
	cancel := a.addAction(action{Kind: "confirm_cancel", ChatID: chatID})
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛑 Stop daemon", callbackData(token)),
			tgbotapi.NewInlineKeyboardButtonData("Cancel", callbackData(cancel)),
		),
	)
	a.sendMarkup(chatID,
		"⚠️ Stop the opencode-telegram daemon?\n\n"+
			"OpenCode keeps running; only the Telegram notifier stops.",
		markup)
}

func (a *App) commandPowerOff(chatID int64) {
	token := a.addAction(action{Kind: "poweroff", ChatID: chatID})
	cancel := a.addAction(action{Kind: "confirm_cancel", ChatID: chatID})
	markup := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏻ Power off", callbackData(token)),
			tgbotapi.NewInlineKeyboardButtonData("Cancel", callbackData(cancel)),
		),
	)
	a.sendMarkup(chatID,
		"⚠️ Power off this computer?\n\n"+
			"This will shut down the machine and everything running on it.",
		markup)
}

func (a *App) confirmDaemonExit(chatID int64, messageID int) {
	text := "✅ opencode-telegram daemon stopped."
	if messageID > 0 {
		a.editMarkup(chatID, messageID, text, nil)
	} else {
		a.send(chatID, text)
	}
	a.RequestShutdown()
}

func (a *App) confirmPowerOff(chatID int64, messageID int) {
	text := "⏻ Powering off the computer…"
	if messageID > 0 {
		a.editMarkup(chatID, messageID, text, nil)
	} else {
		a.send(chatID, text)
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := a.poweroff(ctx); err != nil {
			msg := fmt.Sprintf("❌ Could not power off the computer: %v", err)
			if messageID > 0 {
				a.editMarkup(chatID, messageID, msg, nil)
			} else {
				a.send(chatID, msg)
			}
			return
		}
		msg := "✅ Poweroff executed successfully."
		if messageID > 0 {
			a.editMarkup(chatID, messageID, msg, nil)
		} else {
			a.send(chatID, msg)
		}
	}()
}

func (a *App) cancelConfirmation(chatID int64, messageID int) {
	text := "🛑 Operation aborted. Nothing was changed."
	if messageID > 0 {
		a.editMarkup(chatID, messageID, text, nil)
		return
	}
	a.send(chatID, text)
}

// systemPowerOff tries the usual Linux power-off interfaces without going
// through a shell. The first one that exists is used.
func systemPowerOff(ctx context.Context) error {
	attempts := [][]string{
		{"systemctl", "poweroff"},
		{"loginctl", "poweroff"},
		{"shutdown", "-h", "now"},
		{"poweroff"},
	}

	var lastErr error
	for _, args := range attempts {
		path, err := exec.LookPath(args[0])
		if err != nil {
			lastErr = err
			continue
		}
		out, err := exec.CommandContext(ctx, path, args[1:]...).CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("%s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no power-off command found")
	}
	return lastErr
}

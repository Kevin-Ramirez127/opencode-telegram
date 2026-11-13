package app

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/occlient"
)

// liveFlushInterval is how often buffered streaming deltas are pushed to
// Telegram. Edits are rate-limited by Telegram, so rendering on every token
// would be both slow and unreliable.
const liveFlushInterval = 1200 * time.Millisecond

// liveSession streams one OpenCode session's output to one Telegram chat.
type liveSession struct {
	chatID    int64
	sessionID string
	title     string

	stop     chan struct{}
	stopOnce sync.Once

	mu       sync.Mutex
	text     string
	ended    bool
	started  map[string]bool
	sawDelta map[string]bool

	renderMu    sync.Mutex
	finalized   int
	activeID    int
	activeSeg   int
	lastText    string
	lastSegment string
}

func (ls *liveSession) snapshot() (string, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.text, ls.ended
}

func (ls *liveSession) markEnded() {
	ls.mu.Lock()
	ls.ended = true
	ls.mu.Unlock()
}

func (ls *liveSession) separator() {
	if ls.text == "" || strings.HasSuffix(ls.text, "\n\n") {
		return
	}
	if strings.HasSuffix(ls.text, "\n") {
		ls.text += "\n"
		return
	}
	ls.text += "\n\n"
}

func (ls *liveSession) startBlock(key string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.started == nil {
		ls.started = make(map[string]bool)
	}
	if ls.started[key] {
		return
	}
	ls.started[key] = true
	ls.separator()
}

func (ls *liveSession) appendDelta(key, delta string) {
	if delta == "" {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.started == nil {
		ls.started = make(map[string]bool)
	}
	if ls.sawDelta == nil {
		ls.sawDelta = make(map[string]bool)
	}
	if !ls.started[key] {
		ls.started[key] = true
		ls.separator()
	}
	ls.sawDelta[key] = true
	ls.text += delta
}

func (ls *liveSession) endBlock(key, text string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	hadDelta := ls.sawDelta[key]
	delete(ls.sawDelta, key)
	if strings.TrimSpace(text) == "" || hadDelta {
		return
	}
	if ls.started == nil {
		ls.started = make(map[string]bool)
	}
	if !ls.started[key] {
		ls.started[key] = true
		ls.separator()
	}
	ls.text += text
}

func (ls *liveSession) appendTool(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.separator()
	ls.text += "🔧 " + name + "\n"
}

func (ls *liveSession) appendShell(command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		return
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.separator()
	ls.text += "🔧 shell\n" + markdownCodeBlock("sh", command) + "\n"
}

// markdownCodeBlock wraps text in a fence that cannot be closed by the text
// itself, so commands containing backticks still render correctly.
func markdownCodeBlock(language, text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + language + "\n" + text + "\n" + fence
}

func isShellTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "shell", "bash", "sh", "zsh", "powershell", "cmd":
		return true
	}
	return false
}

func (ls *liveSession) appendToolResult(ok bool, message string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ok {
		ls.text += "✅\n"
		return
	}
	message = truncateOneLine(message, 160)
	if message == "" {
		message = "failed"
	}
	ls.text += "❌ " + message + "\n"
}

// handleLiveEvent feeds streaming deltas into the chats that follow the
// session. Events for sessions nobody is watching are dropped immediately.
func (a *App) handleLiveEvent(ev occlient.Event) {
	var data struct {
		SessionID          string                   `json:"sessionID"`
		AssistantMessageID string                   `json:"assistantMessageID"`
		Ordinal            int                      `json:"ordinal"`
		Delta              string                   `json:"delta"`
		Text               string                   `json:"text"`
		Name               string                   `json:"name"`
		Error              occlient.StructuredError `json:"error"`
		Shell              struct {
			Command string `json:"command"`
		} `json:"shell"`
	}
	if ev.DecodeData(&data) != nil || data.SessionID == "" {
		return
	}
	lives := a.liveSessionsFor(data.SessionID)
	if len(lives) == 0 {
		return
	}
	key := fmt.Sprintf("%s:%d", data.AssistantMessageID, data.Ordinal)

	switch ev.Type {
	case "session.text.started":
		for _, ls := range lives {
			ls.startBlock(key)
		}
	case "session.text.delta":
		for _, ls := range lives {
			ls.appendDelta(key, data.Delta)
		}
	case "session.text.ended":
		for _, ls := range lives {
			ls.endBlock(key, data.Text)
		}
	case "session.tool.input.started":
		// Shell commands are rendered by session.shell.started with their
		// command text; showing the bare tool name here would duplicate it.
		if isShellTool(data.Name) {
			return
		}
		for _, ls := range lives {
			ls.appendTool(data.Name)
		}
	case "session.shell.started":
		for _, ls := range lives {
			ls.appendShell(data.Shell.Command)
		}
	case "session.tool.success":
		for _, ls := range lives {
			ls.appendToolResult(true, "")
		}
	case "session.tool.failed":
		message := data.Error.Message
		if message == "" {
			message = data.Error.Type
		}
		for _, ls := range lives {
			ls.appendToolResult(false, message)
		}
	}
}

// hasLive reports whether a chat currently has an active live stream.
func (a *App) hasLive(chatID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.live[chatID] != nil
}

func (a *App) liveSessionsFor(sessionID string) []*liveSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*liveSession
	for _, ls := range a.live {
		if ls.sessionID == sessionID {
			out = append(out, ls)
		}
	}
	return out
}

func (a *App) removeLive(ls *liveSession) {
	a.mu.Lock()
	if a.live[ls.chatID] == ls {
		delete(a.live, ls.chatID)
	}
	a.mu.Unlock()
}

func (a *App) commandLive(chatID int64) {
	sessionID := a.binding(chatID)
	if sessionID == "" {
		a.send(chatID, "No session bound. Send `/sessions` to pick one.")
		return
	}
	if a.hasLive(chatID) {
		a.send(chatID, "🔴 Live output is already running for this chat. Send `/stoplive` to stop it.")
		return
	}

	ls := &liveSession{
		chatID:    chatID,
		sessionID: sessionID,
		title:     a.sessionLabel(sessionID),
		stop:      make(chan struct{}),
		started:   make(map[string]bool),
		sawDelta:  make(map[string]bool),
	}
	a.mu.Lock()
	a.live[chatID] = ls
	a.mu.Unlock()
	a.refreshLiveCommands(chatID)

	msg, _ := a.sendMarkup(chatID, fmt.Sprintf(
		"🔴 **Live output**\nStreaming `%s`.\nSend `/stoplive` to stop.",
		ls.title,
	), tgbotapi.InlineKeyboardMarkup{})
	if msg.MessageID != 0 {
		ls.activeID = msg.MessageID
	}
	go a.runLive(ls)
}

func (a *App) commandStopLive(chatID int64) {
	if !a.stopLive(chatID) {
		a.send(chatID, "No live output is running for this chat. Send `/live` to start one.")
		return
	}
	a.send(chatID, "⏹ Live output stopped.")
}

// stopLive ends the chat's live stream. The last message is finalized before
// this returns, so the transcript is complete even if the worker is still
// winding down.
func (a *App) stopLive(chatID int64) bool {
	a.mu.Lock()
	ls := a.live[chatID]
	if ls != nil {
		delete(a.live, chatID)
	}
	a.mu.Unlock()
	if ls == nil {
		return false
	}
	a.endLive(ls)
	return true
}

func (a *App) stopLiveForSession(sessionID string) {
	a.mu.Lock()
	var targets []*liveSession
	for chatID, ls := range a.live {
		if ls.sessionID == sessionID {
			targets = append(targets, ls)
			delete(a.live, chatID)
		}
	}
	a.mu.Unlock()
	for _, ls := range targets {
		a.endLive(ls)
	}
}

func (a *App) stopAllLive() {
	a.mu.Lock()
	targets := make([]*liveSession, 0, len(a.live))
	for chatID, ls := range a.live {
		targets = append(targets, ls)
		delete(a.live, chatID)
	}
	a.mu.Unlock()
	for _, ls := range targets {
		a.endLive(ls)
	}
}

func (a *App) endLive(ls *liveSession) {
	ls.markEnded()
	ls.stopOnce.Do(func() { close(ls.stop) })
	a.flushLive(ls, true)
	a.refreshLiveCommands(ls.chatID)
}

func (a *App) runLive(ls *liveSession) {
	ticker := time.NewTicker(liveFlushInterval)
	defer ticker.Stop()
	defer a.removeLive(ls)

	for {
		select {
		case <-ls.stop:
			a.flushLive(ls, true)
			return
		case <-ticker.C:
			a.flushLive(ls, false)
		}
	}
}

// flushLive renders the buffered text into a sequence of Telegram messages.
// Only the newest chunk is edited; completed chunks are left alone so earlier
// output stays readable.
func (a *App) flushLive(ls *liveSession, final bool) {
	ls.renderMu.Lock()
	defer ls.renderMu.Unlock()

	text, _ := ls.snapshot()
	display := text
	if final {
		if strings.TrimSpace(display) == "" {
			display = fmt.Sprintf(
				"🔴 **Live output**\nStreaming `%s`.\n\n_No output was streamed._\n⏹ **Live ended.**",
				ls.title,
			)
		} else {
			display += "\n\n⏹ **Live ended.**"
		}
	}
	if display == ls.lastText || strings.TrimSpace(display) == "" {
		return
	}

	segments := splitTelegramText(display, maxTelegramText)
	if len(segments) == 0 {
		return
	}
	lastIdx := len(segments) - 1

	// The message used for the active chunk becomes a completed chunk when
	// enough new text arrived to spill over into another Telegram message.
	if ls.activeID != 0 && ls.activeSeg < lastIdx {
		if err := a.editLive(ls.chatID, ls.activeID, segments[ls.activeSeg]); err != nil {
			return
		}
		ls.finalized = ls.activeSeg + 1
		ls.activeID = 0
	}

	for ls.finalized < lastIdx {
		if _, err := a.sendMarkup(ls.chatID, segments[ls.finalized], tgbotapi.InlineKeyboardMarkup{}); err != nil {
			return
		}
		ls.finalized++
	}

	if ls.activeID == 0 {
		msg, err := a.sendMarkup(ls.chatID, segments[lastIdx], tgbotapi.InlineKeyboardMarkup{})
		if err != nil {
			return
		}
		ls.activeID = msg.MessageID
		ls.activeSeg = lastIdx
	} else if segments[lastIdx] != ls.lastSegment {
		if err := a.editLive(ls.chatID, ls.activeID, segments[lastIdx]); err != nil {
			return
		}
	}

	ls.lastSegment = segments[lastIdx]
	ls.lastText = display
}

func (a *App) editLive(chatID int64, messageID int, text string) error {
	text = truncateText(text, maxTelegramText)
	if err := a.editRichMarkup(chatID, messageID, text, nil); err == nil {
		return nil
	} else {
		logRichFallback("edit", chatID, err)
	}

	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.DisableWebPagePreview = true
	empty := emptyInlineKeyboard()
	msg.ReplyMarkup = &empty
	if _, err := a.bot.Send(msg); err != nil {
		log.Printf("failed to edit live message %d in chat %d: %v", messageID, chatID, err)
		return err
	}
	return nil
}

// refreshLiveCommands swaps /live and /stoplive in this chat's command menu.
// Telegram command menus are global by default, so a chat-scoped list keeps
// the swap local to the chat that is actually streaming. The goroutine reads
// the current state when it runs, which also coalesces quick start/stop calls.
func (a *App) refreshLiveCommands(chatID int64) {
	if a.bot == nil {
		return
	}
	go func() {
		a.liveMenuMu.Lock()
		defer a.liveMenuMu.Unlock()

		scope := tgbotapi.NewBotCommandScopeChat(chatID)
		if !a.hasLive(chatID) {
			if _, err := a.bot.Request(tgbotapi.NewDeleteMyCommandsWithScope(scope)); err != nil {
				log.Printf("could not reset live commands for chat %d: %v", chatID, err)
			}
			return
		}

		commands := a.botCommandList()
		replaced := false
		for i := range commands {
			if commands[i].Command == "live" {
				commands[i] = tgbotapi.BotCommand{
					Command:     "stoplive",
					Description: "Stop live output",
				}
				replaced = true
			}
		}
		if !replaced {
			commands = append(commands, tgbotapi.BotCommand{
				Command:     "stoplive",
				Description: "Stop live output",
			})
		}
		if _, err := a.bot.Request(tgbotapi.NewSetMyCommandsWithScope(scope, commands...)); err != nil {
			log.Printf("could not set live commands for chat %d: %v", chatID, err)
		}
	}()
}

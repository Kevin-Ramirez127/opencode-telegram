package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
)

const maxTelegramText = 4096

type sentMessage struct {
	ChatID    int64
	MessageID int
}

// telegramLoop consumes Telegram updates with a bounded worker pool.
func (a *App) telegramLoop(ctx context.Context) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "callback_query"}
	updates := a.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			return
		case upd, ok := <-updates:
			if !ok {
				log.Print("telegram update channel closed")
				return
			}
			select {
			case a.sem <- struct{}{}:
				go func() {
					defer func() { <-a.sem }()
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic while handling update: %v", r)
						}
					}()
					a.handleUpdate(upd)
				}()
			default:
				log.Print("dropping update: worker limit reached")
			}
		}
	}
}

func (a *App) handleUpdate(upd tgbotapi.Update) {
	var userID, chatID int64
	var text string
	var isCallback bool

	switch {
	case upd.Message != nil && upd.Message.From != nil:
		userID, chatID = upd.Message.From.ID, upd.Message.Chat.ID
		text = upd.Message.Text
	case upd.CallbackQuery != nil:
		q := upd.CallbackQuery
		userID = q.From.ID
		if q.Message == nil {
			return
		}
		chatID = q.Message.Chat.ID
		text = q.Data
		isCallback = true
	default:
		return
	}

	// Gate 1: allowlist. Both the user and the chat must be listed. Unknown
	// updates are dropped silently so the bot never reveals itself.
	if !a.whitelist.IsAllowed(userID, chatID) {
		log.Printf("blocked update from user %d in chat %d (not allowlisted)", userID, chatID)
		return
	}

	// Gate 2: per-chat rate limit for typed messages. Callback presses are
	// acknowledged and not throttled here so multiselect forms stay usable;
	// they are already gated by the allowlist and Telegram's own limits.
	if isCallback {
		_, _ = a.bot.Request(tgbotapi.NewCallback(upd.CallbackQuery.ID, ""))
	}
	if !isCallback && !a.limit.Allow(chatID) {
		log.Printf("rate limited: ignored update from chat %d", chatID)
		return
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	if isCallback {
		a.handleCallbackFrom(chatID, upd.CallbackQuery.Message.MessageID, text)
		return
	}
	if strings.HasPrefix(text, "/") {
		a.handleCommand(chatID, userID, text)
		return
	}
	a.handleFreeText(chatID, text)
}

func (a *App) handleCommand(chatID, userID int64, text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "/start", "/help", "/menu":
		a.send(chatID, a.helpText())
	case "/status", "/where":
		a.showStatus(chatID)
	case "/sessions", "/session":
		if cmd == "/session" && len(args) > 0 {
			a.bindSession(chatID, args[0])
			return
		}
		a.showSessions(chatID)
	case "/bind":
		if len(args) == 0 {
			a.send(chatID, "Usage: `/bind <session-id>`\n\nSend `/sessions` to pick one.")
			return
		}
		a.bindSession(chatID, args[0])
	case "/new":
		a.commandNew(chatID)
	case "/unbind":
		a.setBinding(chatID, "")
		a.send(chatID, "🔓 Unbound. Session notifications are unaffected.")
	case "/agents", "/agent":
		sessionID := a.binding(chatID)
		if sessionID == "" {
			a.send(chatID, "No session bound. Send /sessions to pick one.")
			return
		}
		if len(args) > 0 {
			a.switchAgentFromCommand(chatID, sessionID, args[0])
			return
		}
		a.showAgents(chatID)
	case "/models", "/model":
		a.showModels(chatID)
	case "/usage":
		a.showUsage(chatID)
	case "/lastmessage":
		a.showLastMessage(chatID)
	case "/notify":
		a.commandNotify(chatID, args)
	case "/interrupt", "/stop":
		a.interruptBound(chatID)
	case "/id":
		a.send(chatID, fmt.Sprintf("Telegram user id: %d\nChat id: %d", userID, chatID))
	case "/test":
		a.sendTestNotification([]int64{chatID})
	case "/settings":
		a.showSettings(chatID)
	case "/exit":
		a.commandExit(chatID)
	case "/poweroff":
		a.commandPowerOff(chatID)
	default:
		a.send(chatID, fmt.Sprintf("Unknown command %q. Send /help for the command list.", cmd))
	}
}

func (a *App) helpText() string {
	lines := []string{
		"**OpenCode Telegram**",
		"",
		"I notify you when an OpenCode session finishes, fails, asks a question, or requests a permission.",
		"",
		"**Quick start**",
		"1. Send `/sessions` and tap **Bind this session** on the one you want.",
		"2. Send a message to prompt the bound session.",
		"3. Send `/notify off` to pause notifications.",
		"",
		"> Sending messages requires **Allow new messages** in the OpenCode Telegram plugin.",
		"",
		"**Commands**",
		"- `/start`, `/help` — show this guide",
		"- `/status` — daemon and OpenCode status",
		"- `/sessions` — list sessions with bind buttons",
		"- `/bind <session-id>` — bind this chat to a session",
		"- `/unbind` — remove the binding",
	}
	if a.settings.Get().Security.AllowNewMessages {
		lines = append(lines, "- `/new` — create a new session and bind it")
	}
	lines = append(lines,
		"- `/agents [agent-id]` — list or switch the bound session's agent",
		"- `/models` — list all available chat models",
		"- `/usage` — show OpenCode Go and Console usage limits",
		"- `/lastmessage` — show the bound session's last assistant message",
		"- `/interrupt` — stop the bound session's current run",
		"- `/notify` — show or change notification delivery",
		"- `/settings` — show notification and security settings",
		"- `/test` — send a test notification",
		"- `/id` — show your Telegram user and chat ids",
		"- `/exit` — stop the opencode-telegram daemon (asks first)",
		"- `/poweroff` — power off this computer (asks first)",
	)
	return strings.Join(lines, "\n")
}

func (a *App) showStatus(chatID int64) {
	snap := a.snapshot()
	state := "disconnected"
	if snap.OpenCodeOK {
		state = "connected"
	}
	icon := "🟢"
	if !snap.OpenCodeOK {
		icon = "🔴"
	}
	st := a.settings.Get()
	bound := a.binding(chatID)

	var b strings.Builder
	fmt.Fprintf(&b, "%s OpenCode %s\n", icon, state)
	fmt.Fprintf(&b, "Endpoint: %s\n", snap.OpenCodeURL)
	if snap.OpenCodeVer != "" {
		fmt.Fprintf(&b, "Server version: %s\n", snap.OpenCodeVer)
	}
	fmt.Fprintf(&b, "Daemon uptime: %s\n", time.Duration(snap.UptimeSec)*time.Second)
	fmt.Fprintf(&b, "Allowlist: %d users, %d chats\n", snap.Users, snap.Chats)
	fmt.Fprintf(&b, "Active sessions: %d\n", snap.Active)
	fmt.Fprintf(&b, "Pending questions: %d\n", snap.PendingForm)
	fmt.Fprintf(&b, "Pending permissions: %d\n", snap.PendingPerm)
	fmt.Fprintf(&b, "Notifications: %s\n", onOff(st.Notifications.Enabled))
	if bound == "" {
		b.WriteString("Bound session: none\n")
	} else {
		fmt.Fprintf(&b, "Bound session: %s\n", a.sessionLabel(bound))
	}
	if snap.LastError != "" {
		fmt.Fprintf(&b, "Last error: %s\n", snap.LastError)
	}
	a.send(chatID, b.String())
}

func (a *App) showSessions(chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sessions, err := a.recentSessions(ctx, 10)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not list sessions: %v", err))
		return
	}
	if len(sessions) == 0 {
		a.send(chatID, "No sessions found.")
		return
	}

	bound := a.binding(chatID)
	boundInList := false
	for _, s := range sessions {
		isBound := s.ID == bound
		if isBound {
			boundInList = true
		}

		title := truncateOneLine(s.DisplayTitle(), 80)
		if title == "" {
			title = s.ID
		}
		directory := strings.TrimSpace(s.Location.Directory)
		if directory == "" {
			directory = "default location"
		}

		var b strings.Builder
		fmt.Fprintf(&b, "**%s**\n", title)
		fmt.Fprintf(&b, "📁 `%s`\n", directory)
		if st := a.sessionSnapshot(s.ID); st.Busy {
			b.WriteString("⚡ **Running now**\n")
		}
		if isBound {
			b.WriteString("✅ **Currently bound**\n")
		}

		var button tgbotapi.InlineKeyboardButton
		if isBound {
			token := a.addAction(action{Kind: "unbind", ChatID: chatID})
			button = tgbotapi.NewInlineKeyboardButtonData("🔓 Unbind", callbackData(token))
		} else {
			token := a.addAction(action{Kind: "bind", SessionID: s.ID, ChatID: chatID})
			button = tgbotapi.NewInlineKeyboardButtonData("🔗 Bind this session", callbackData(token))
		}
		markup := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(button))
		a.sendMarkup(chatID, strings.TrimSpace(b.String()), markup)
	}

	if bound != "" && !boundInList {
		token := a.addAction(action{Kind: "unbind", ChatID: chatID})
		markup := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔓 Unbind", callbackData(token)),
		))
		a.sendMarkup(chatID, fmt.Sprintf("**📌 Currently bound**\n%s", a.sessionLabel(bound)), markup)
	}
}

func (a *App) bindSession(chatID int64, sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		a.send(chatID, "Usage: /bind <session-id>")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	sess, err := a.oc.GetSession(ctx, sessionID)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Session %s not found: %v", sessionID, err))
		return
	}
	a.rememberSession(sess)
	a.setBinding(chatID, sess.ID)
	a.send(chatID, fmt.Sprintf("🔗 Bound to: %s\n%s", sess.DisplayTitle(), sess.Location.Directory))
}

func (a *App) showLastMessage(chatID int64) {
	sessionID := a.binding(chatID)
	if sessionID == "" {
		a.send(chatID, "No session bound. Send `/sessions` to pick one.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if sess, err := a.oc.GetSession(ctx, sessionID); err == nil {
		a.rememberSession(sess)
	}
	text, err := a.oc.LastAssistantText(ctx, sessionID)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not read the last message: %v", err))
		return
	}
	if strings.TrimSpace(text) == "" {
		a.send(chatID, "No assistant message found for this session yet.")
		return
	}

	header := fmt.Sprintf("**📨 Last message**\nSession: %s", a.sessionLabel(sessionID))
	a.sendPartsToChat(chatID, contentMessages(header, []string{text}))
}

const newSessionDisabled = "⚠️ **New sessions are disabled.**\n\n" +
	"Enable **Allow new messages** in the OpenCode Telegram plugin to create sessions from Telegram."

func (a *App) commandNew(chatID int64) {
	if !a.settings.Get().Security.AllowNewMessages {
		a.send(chatID, newSessionDisabled)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := a.startDelayedLoading(chatID, "⏳ Creating a new session…", 250*time.Millisecond)
	sess, err := a.oc.CreateSession(ctx, a.newSessionDirectory(ctx, chatID))
	done()
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not create a session: %v", err))
		return
	}
	if strings.TrimSpace(sess.ID) == "" {
		a.send(chatID, "❌ OpenCode returned an empty session.")
		return
	}

	a.rememberSession(sess)
	a.setBinding(chatID, sess.ID)
	a.send(chatID, newSessionText(sess))
}

// newSessionDirectory keeps /new in the same project as the bound session.
// Without a binding it falls back to the most recent session, and finally to
// the OpenCode server's default location.
func (a *App) newSessionDirectory(ctx context.Context, chatID int64) string {
	if sessionID := a.binding(chatID); sessionID != "" {
		if sess, err := a.oc.GetSession(ctx, sessionID); err == nil {
			a.rememberSession(sess)
			if directory := strings.TrimSpace(sess.Location.Directory); directory != "" {
				return directory
			}
		}
	}
	if sessions, err := a.recentSessions(ctx, 1); err == nil {
		for _, sess := range sessions {
			if directory := strings.TrimSpace(sess.Location.Directory); directory != "" {
				return directory
			}
		}
	}
	return ""
}

func newSessionText(sess occlient.Session) string {
	title := truncateOneLine(sess.DisplayTitle(), 80)
	if title == "" {
		title = sess.ID
	}
	directory := strings.TrimSpace(sess.Location.Directory)
	if directory == "" {
		directory = "default location"
	}
	return fmt.Sprintf("**🆕 New session bound**\n\n**%s**\n📁 `%s`\n\nSend a message to start working in this session.", title, directory)
}

func (a *App) binding(chatID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bindings[chatID]
}

func (a *App) setBinding(chatID int64, sessionID string) {
	a.mu.Lock()
	if sessionID == "" {
		delete(a.bindings, chatID)
	} else {
		a.bindings[chatID] = sessionID
	}
	a.mu.Unlock()
}

func (a *App) sessionLabel(sessionID string) string {
	st := a.sessionSnapshot(sessionID)
	if st.Title != "" {
		return st.Title
	}
	return sessionID
}

func (a *App) commandNotify(chatID int64, args []string) {
	if len(args) == 0 {
		a.send(chatID, notifyStatusText(a.settings.Get().Notifications.Enabled, false))
		return
	}
	if len(args) != 1 {
		a.send(chatID, "Usage: `/notify on|off`")
		return
	}
	var enabled bool
	switch strings.ToLower(args[0]) {
	case "on", "true", "1", "yes":
		enabled = true
	case "off", "false", "0", "no":
		enabled = false
	default:
		a.send(chatID, "Usage: `/notify on|off`")
		return
	}
	st, err := a.settings.Patch(settings.Patch{
		Notifications: &settings.NotificationsPatch{Enabled: &enabled},
	})
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not update settings: %v", err))
		return
	}
	a.send(chatID, notifyStatusText(st.Notifications.Enabled, true))
}

func notifyStatusText(enabled, changed bool) string {
	next := "on"
	action := "enable"
	if enabled {
		next = "off"
		action = "disable"
	}
	state := "are"
	if changed {
		state = "are now"
	}
	return fmt.Sprintf(
		"**🔔 Notifications %s %s.**\n\nSend `/notify %s` to %s them.",
		state, onOff(enabled), next, action,
	)
}

func (a *App) interruptBound(chatID int64) {
	id := a.binding(chatID)
	if id == "" {
		a.send(chatID, "No session bound. Send /sessions to pick one.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.oc.Interrupt(ctx, id); err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not interrupt %s: %v", a.sessionLabel(id), err))
		return
	}
	a.send(chatID, fmt.Sprintf("⏹ Interrupt requested for %s.", a.sessionLabel(id)))
}

func (a *App) showSettings(chatID int64) {
	current := a.settings.Get()
	st := current.Notifications
	sec := current.Security
	text := strings.Join([]string{
		"**⚙️ Settings**",
		"",
		"**Notifications**",
		fmt.Sprintf("- Enabled: **%s**", onOff(st.Enabled)),
		fmt.Sprintf("- Session finished: **%s**", onOff(st.SessionDone)),
		fmt.Sprintf("- Session failed: **%s**", onOff(st.SessionFailed)),
		fmt.Sprintf("- Session interrupted: **%s**", onOff(st.SessionInterrupted)),
		fmt.Sprintf("- Question asked: **%s**", onOff(st.QuestionAsked)),
		fmt.Sprintf("- Permission asked: **%s**", onOff(st.PermissionAsked)),
		fmt.Sprintf("- Retry status: **%s**", onOff(st.Retry)),
		fmt.Sprintf("- Include last message: **%s**", onOff(st.IncludeLastMessage)),
		fmt.Sprintf("- Include directory: **%s**", onOff(st.IncludeDirectory)),
		fmt.Sprintf("- Long messages: **%s**", st.LongMessageMode),
		fmt.Sprintf("- Summarize thinking button: **%s**", onOff(st.ShowSummarizeThinking)),
		"",
		"**Security**",
		"_Change these from the OpenCode plugin, not Telegram._",
		fmt.Sprintf("- Allow custom question answers: **%s**", onOff(sec.AllowCustomFormAnswers)),
		fmt.Sprintf("- Allow new messages: **%s**", onOff(sec.AllowNewMessages)),
		fmt.Sprintf("- Allow model switching: **%s**", onOff(sec.AllowModelSwitching)),
		"",
		"Use `/notify` to see delivery status or `/notify on|off` to change it.",
	}, "\n")
	a.send(chatID, text)
}

func (a *App) handleFreeText(chatID int64, text string) {
	if a.hasPendingFormInput(chatID) {
		a.handleFormText(chatID, text)
		return
	}

	st := a.settings.Get()
	if !st.Security.AllowNewMessages {
		a.send(chatID, "Free-text messages are disabled. Enable \"Allow new messages\" in the OpenCode Telegram plugin if you want to prompt OpenCode from Telegram.\n\nSend /help for the command list.")
		return
	}
	sessionID := a.binding(chatID)
	if sessionID == "" {
		a.send(chatID, "No session bound. Send /sessions to pick one.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.oc.Prompt(ctx, sessionID, text); err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not send message to %s: %v", a.sessionLabel(sessionID), err))
		return
	}
	a.send(chatID, fmt.Sprintf("📨 Sent to %s.", a.sessionLabel(sessionID)))
}

// ---------------------------------------------------------------------------
// Callback handling
// ---------------------------------------------------------------------------

func (a *App) handleCallback(chatID int64, data string) {
	a.handleCallbackFrom(chatID, 0, data)
}

func (a *App) handleCallbackFrom(chatID int64, messageID int, data string) {
	token, ok := parseCallbackData(data)
	if !ok {
		a.send(chatID, "⚠️ This button is outdated.")
		return
	}
	act, ok := a.getAction(token)
	if !ok {
		a.send(chatID, "⚠️ This button has expired.")
		return
	}
	if act.ChatID != 0 && act.ChatID != chatID {
		switch act.Kind {
		case "agent", "model", "daemon_exit", "poweroff", "confirm_cancel":
			a.send(chatID, "⚠️ This button belongs to another chat.")
			return
		}
	}

	// Summary/thinking buttons are single-use per chat so repeated taps cannot
	// start multiple expensive generations, while other notify chats can still
	// use their own copy of the same broadcast button.
	switch act.Kind {
	case "show_thinking", "summarize_thinking", "summarize_output", "daemon_exit", "poweroff":
		if !a.claimAction(token, chatID) {
			a.send(chatID, "⚠️ This action was already used.")
			return
		}
	}

	switch act.Kind {
	case "bind":
		a.bindFromCallback(chatID, act)
	case "bind_reply":
		a.setBinding(chatID, act.SessionID)
		a.send(chatID, fmt.Sprintf("🔗 Bound to %s.\nType your message now.", a.sessionLabel(act.SessionID)))
	case "agent":
		a.switchAgent(chatID, messageID, act.SessionID, act.Value)
	case "model":
		a.switchModel(chatID, messageID, act.SessionID, act.Decision, act.Value)
	case "show_thinking":
		a.showThinking(chatID, act)
	case "summarize_thinking":
		a.summarizeThinking(chatID, act)
	case "summarize_output":
		a.summarizeOutput(chatID, act)
	case "unbind":
		a.setBinding(chatID, "")
		a.send(chatID, "🔓 Unbound.")
	case "perm":
		a.replyPermission(chatID, token, act)
	case "form_opt":
		a.formChoose(chatID, act, false)
	case "form_toggle":
		a.formToggle(chatID, act)
	case "form_done":
		a.formDone(chatID, act)
	case "form_skip":
		a.formSkip(chatID, act)
	case "form_custom":
		a.formPromptCustom(chatID, act)
	case "form_cancel_input":
		a.clearPendingInput(chatID)
		a.send(chatID, "Cancelled.")
	case "form_submit":
		a.formRetrySubmit(chatID, act)
	case "daemon_exit":
		a.confirmDaemonExit(chatID, messageID)
	case "poweroff":
		a.confirmPowerOff(chatID, messageID)
	case "confirm_cancel":
		a.cancelConfirmation(chatID, messageID)
	default:
		a.send(chatID, "⚠️ Unknown action.")
	}
}

func (a *App) bindFromCallback(chatID int64, act action) {
	a.setBinding(chatID, act.SessionID)
	a.send(chatID, fmt.Sprintf("🔗 Bound to: %s", a.sessionLabel(act.SessionID)))
}

// ---------------------------------------------------------------------------
// Sending helpers
// ---------------------------------------------------------------------------

func (a *App) send(chatID int64, text string) {
	a.sendMarkup(chatID, text, tgbotapi.InlineKeyboardMarkup{})
}

// startLoading sends a temporary status message and returns a function that
// removes it. Callers remove it before sending the real result so the chat does
// not keep both messages.
func (a *App) startLoading(chatID int64, text string) func() {
	msg, err := a.sendMarkup(chatID, text, tgbotapi.InlineKeyboardMarkup{})
	if err != nil {
		return func() {}
	}
	return func() { a.deleteMessage(chatID, msg.MessageID) }
}

// startDelayedLoading only shows a loading message when an operation takes
// longer than the delay. The returned stop function removes that message and
// waits for the delayed sender, so it is safe to call from a normal code path.
func (a *App) startDelayedLoading(chatID int64, text string, delay time.Duration) func() {
	cancel := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			msg, err := a.sendMarkup(chatID, text, tgbotapi.InlineKeyboardMarkup{})
			if err != nil {
				result <- 0
				return
			}
			result <- msg.MessageID
		case <-cancel:
			result <- 0
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(cancel)
			if id := <-result; id > 0 {
				a.deleteMessage(chatID, id)
			}
		})
	}
}

func (a *App) deleteMessage(chatID int64, messageID int) {
	if messageID == 0 {
		return
	}
	if _, err := a.bot.Request(tgbotapi.NewDeleteMessage(chatID, messageID)); err != nil {
		log.Printf("failed to delete message %d in chat %d: %v", messageID, chatID, err)
	}
}

func (a *App) sendMarkup(chatID int64, text string, markup tgbotapi.InlineKeyboardMarkup) (tgbotapi.Message, error) {
	text = truncateText(text, maxTelegramText)
	sent, err := a.sendRichMarkup(chatID, text, markup)
	if err == nil {
		return sent, nil
	}
	logRichFallback("send", chatID, err)
	return a.sendPlainMarkup(chatID, text, markup)
}

func (a *App) sendPlainMarkup(chatID int64, text string, markup tgbotapi.InlineKeyboardMarkup) (tgbotapi.Message, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	if len(markup.InlineKeyboard) > 0 {
		msg.ReplyMarkup = markup
	}
	sent, err := a.bot.Send(msg)
	if err != nil {
		log.Printf("failed to send to chat %d: %v", chatID, err)
	}
	return sent, err
}

func emptyInlineKeyboard() tgbotapi.InlineKeyboardMarkup {
	// Telegram rejects a null inline_keyboard; clearing buttons requires an
	// explicit empty array.
	return tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: make([][]tgbotapi.InlineKeyboardButton, 0),
	}
}

func (a *App) editMarkup(chatID int64, messageID int, text string, markup *tgbotapi.InlineKeyboardMarkup) {
	text = truncateText(text, maxTelegramText)
	if err := a.editRichMarkup(chatID, messageID, text, markup); err == nil {
		return
	} else {
		logRichFallback("edit", chatID, err)
	}

	msg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	msg.DisableWebPagePreview = true
	if markup != nil {
		msg.ReplyMarkup = markup
	} else {
		// nil explicitly clears the existing keyboard.
		empty := emptyInlineKeyboard()
		msg.ReplyMarkup = &empty
	}
	if _, err := a.bot.Send(msg); err != nil {
		log.Printf("failed to edit message %d in chat %d: %v", messageID, chatID, err)
	}
}

func (a *App) editReplyMarkup(chatID int64, messageID int) {
	empty := emptyInlineKeyboard()
	_, _ = a.bot.Request(tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, empty))
}

func (a *App) broadcast(text string, markup tgbotapi.InlineKeyboardMarkup) []sentMessage {
	chats := a.notifyChats()
	out := make([]sentMessage, 0, len(chats))
	for _, chatID := range chats {
		msg, err := a.sendMarkup(chatID, text, markup)
		if err == nil {
			out = append(out, sentMessage{ChatID: chatID, MessageID: msg.MessageID})
		}
	}
	return out
}

func (a *App) notifyChats() []int64 {
	st := a.settings.Get().Notifications
	if len(st.NotifyChats) > 0 {
		out := make([]int64, 0, len(st.NotifyChats))
		for _, id := range st.NotifyChats {
			if a.whitelist.ChatAllowed(id) {
				out = append(out, id)
			}
		}
		return out
	}
	return a.whitelist.ChatIDs()
}

func (a *App) sendTestNotification(chats []int64) {
	if len(chats) == 0 {
		log.Print("test notification skipped: no allowlisted chat")
		return
	}
	text := "🔔 Test notification from opencode-telegram.\nIf you can read this, Telegram delivery works."
	for _, id := range chats {
		a.send(id, text)
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func truncateText(s string, limit int) string {
	s = sanitizeUTF8(s)
	if len(s) <= limit {
		return s
	}
	const suffix = "\n…[truncated]"
	if limit <= len(suffix) {
		return truncateUTF8Bytes(s, limit)
	}
	return truncateUTF8Bytes(s, limit-len(suffix)) + suffix
}

func truncateOneLine(s string, limit int) string {
	s = sanitizeUTF8(strings.ReplaceAll(s, "\n", " "))
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	const suffix = "…"
	if limit <= len(suffix) {
		return truncateUTF8Bytes(s, limit)
	}
	return truncateUTF8Bytes(s, limit-len(suffix)) + suffix
}

// sanitizeUTF8 protects Telegram from invalid bytes (for example, a provider
// returning a broken string). Telegram rejects the whole message otherwise.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// truncateUTF8Bytes cuts s to at most max bytes without splitting a UTF-8
// rune. Byte slicing in the middle of a multi-byte character is what caused
// Telegram's "strings must be encoded in UTF-8" errors.
func truncateUTF8Bytes(s string, max int) string {
	if max >= len(s) {
		return s
	}
	if max <= 0 {
		return ""
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// sendToChat is used by HTTP handlers.
func (a *App) sendToChat(chatID int64, text string) error {
	_, err := a.sendMarkup(chatID, text, tgbotapi.InlineKeyboardMarkup{})
	return err
}

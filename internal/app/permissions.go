package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/occlient"
)

func (a *App) handlePermissionAsked(p occlient.Permission) {
	st := a.settings.Get()
	if !st.Notifications.Enabled || !st.Notifications.PermissionAsked {
		return
	}
	if p.ID == "" {
		return
	}
	if !a.markOnce("perm:"+p.ID, 24*time.Hour) {
		return
	}

	title := a.sessionLabel(p.SessionID)
	if sess, err := a.fetchSession(p.SessionID); err == nil {
		a.rememberSession(sess)
		title = sess.DisplayTitle()
	}

	var b strings.Builder
	b.WriteString("🔐 OpenCode permission request\n")
	fmt.Fprintf(&b, "Action: %s\n", blank(p.Action, "(unknown)"))
	fmt.Fprintf(&b, "Session: %s\n", title)
	if len(p.Resources) > 0 {
		b.WriteString("Resources:\n")
		for _, r := range p.Resources {
			fmt.Fprintf(&b, "  • %s\n", truncateOneLine(r, 200))
		}
	}
	if p.Message != "" {
		fmt.Fprintf(&b, "\n%s\n", truncateText(p.Message, 1000))
	}

	mk := func(label string, decision string) []tgbotapi.InlineKeyboardButton {
		token := a.addAction(action{
			Kind:      "perm",
			SessionID: p.SessionID,
			RequestID: p.ID,
			Decision:  decision,
		})
		return []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(label, callbackData(token)),
		}
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		mk("Allow once", "once"),
		mk("Always allow", "always"),
		mk("Reject", "reject"),
	}
	messages := a.broadcast(b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
	a.mu.Lock()
	a.permMsgs[p.ID] = messages
	a.mu.Unlock()
}

func (a *App) replyPermission(chatID int64, token string, act action) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.oc.ReplyPermission(ctx, act.SessionID, act.RequestID, act.Decision, ""); err != nil {
		log.Printf("permission reply failed for %s: %v", act.RequestID, err)
		a.send(chatID, fmt.Sprintf("❌ Could not reply to the permission request: %v", err))
		return
	}
	a.delAction(token)
	a.delPermissionActions(act.RequestID)

	label := "Rejected"
	switch act.Decision {
	case "once":
		label = "Allowed once"
	case "always":
		label = "Always allowed"
	}
	a.mu.Lock()
	msgs := a.permMsgs[act.RequestID]
	delete(a.permMsgs, act.RequestID)
	a.mu.Unlock()
	for _, m := range msgs {
		a.editMarkup(m.ChatID, m.MessageID, "🔐 Permission request: "+label, nil)
	}
}

func (a *App) handlePermissionReplied(requestID string) {
	a.mu.Lock()
	msgs := a.permMsgs[requestID]
	delete(a.permMsgs, requestID)
	a.mu.Unlock()
	a.delPermissionActions(requestID)
	for _, m := range msgs {
		a.editMarkup(m.ChatID, m.MessageID, "🔐 Permission request resolved in OpenCode.", nil)
	}
}

func (a *App) delPermissionActions(requestID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, act := range a.actions {
		if act.Kind == "perm" && act.RequestID == requestID {
			delete(a.actions, token)
		}
	}
}

func (a *App) pendingPermissionCount(ctx context.Context) (int, error) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, act := range a.actions {
		if act.Kind == "perm" && now.Before(act.Expires) {
			n++
		}
	}
	return n, nil
}

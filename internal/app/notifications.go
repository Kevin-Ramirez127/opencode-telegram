package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/occlient"
	"opencode-telegram/internal/settings"
)

// notifySession reports a finished, failed, interrupted, or idle session.
// kind is one of succeeded/failed/interrupted/idle.
func (a *App) notifySession(sessionID, kind, detail string) {
	if sessionID == "" {
		return
	}

	st := a.settings.Get()
	n := st.Notifications

	typeKey := "session_done"
	switch kind {
	case "failed":
		typeKey = "session_failed"
	case "interrupted":
		typeKey = "session_interrupted"
	}

	state := a.sessionSnapshot(sessionID)

	// idle is the fallback signal. If the execution outcome already produced a
	// notification for this run, there is nothing more to say.
	if kind == "idle" {
		if state.NotifiedRun {
			return
		}
		if !state.Busy && state.Outcome == "" {
			return
		}
	}

	if !n.Enabled || !enabledFor(n, typeKey) {
		a.finishRun(sessionID, kind, false)
		return
	}
	// Do not claim a session is finished while it is actually waiting for a
	// human answer or permission decision.
	if a.hasPendingHumanInput(sessionID) {
		a.finishRun(sessionID, kind, false)
		return
	}
	// Guard against duplicate frames for the same run/kind.
	if !a.markOnce("session:"+sessionID+":"+kind, 10*time.Second) {
		a.finishRun(sessionID, kind, false)
		return
	}

	sess, err := a.fetchSession(sessionID)
	if err != nil {
		log.Printf("notification: could not read session %s: %v", sessionID, err)
		sess = occlient.Session{ID: sessionID, Title: state.Title, Location: occlient.Location{Directory: state.Directory}}
	}
	a.rememberSession(sess)
	if !n.IncludeChildSessions && sess.ParentID != "" {
		a.finishRun(sessionID, kind, false)
		return
	}

	icon, heading := "✅", "OpenCode session finished"
	switch kind {
	case "failed":
		icon, heading = "❌", "OpenCode session failed"
	case "interrupted":
		icon, heading = "⏹", "OpenCode session interrupted"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", icon, heading)
	fmt.Fprintf(&b, "Title: %s\n", sess.DisplayTitle())
	if n.IncludeDirectory && sess.Location.Directory != "" {
		fmt.Fprintf(&b, "Directory: %s\n", sess.Location.Directory)
	}
	if kind == "idle" {
		b.WriteString("Outcome: idle\n")
	} else if kind == "succeeded" {
		b.WriteString("Outcome: succeeded\n")
	} else if kind == "failed" {
		b.WriteString("Outcome: failed\n")
	} else if kind == "interrupted" {
		fmt.Fprintf(&b, "Outcome: interrupted")
		if detail != "" {
			fmt.Fprintf(&b, " (%s)", detail)
		}
		b.WriteString("\n")
	}
	if d := sessionDuration(sess); d != "" {
		fmt.Fprintf(&b, "Duration: %s\n", d)
	}
	if kind == "failed" && detail != "" {
		fmt.Fprintf(&b, "\nError: %s\n", truncateText(detail, 1200))
	}

	if n.IncludeLastMessage && kind != "failed" {
		if text := a.lastAssistantText(sessionID); text != "" {
			fmt.Fprintf(&b, "\nLast message:\n%s\n", truncateText(text, 1200))
		}
	}

	var markup tgbotapi.InlineKeyboardMarkup
	if a.settings.Get().Security.AllowNewMessages {
		token := a.addAction(action{Kind: "bind_reply", SessionID: sessionID})
		markup = tgbotapi.NewInlineKeyboardMarkup([]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Reply to this session", callbackData(token)),
		})
	}

	a.broadcast(b.String(), markup)
	a.finishRun(sessionID, kind, true)
}

func (a *App) finishRun(sessionID, kind string, notified bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.sessions[sessionID]
	if st == nil {
		st = &sessionState{ID: sessionID}
		a.sessions[sessionID] = st
	}
	st.Busy = false
	st.NotifiedRun = true
	st.Outcome = kind
	if notified {
		st.LastSeen = time.Now()
	}
}

func (a *App) hasPendingHumanInput(sessionID string) bool {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, flow := range a.forms {
		if flow.SessionID == sessionID {
			return true
		}
	}
	for _, act := range a.actions {
		if act.Kind == "perm" && act.SessionID == sessionID && now.Before(act.Expires) {
			return true
		}
	}
	return false
}

func enabledFor(n settings.Notifications, key string) bool {
	switch key {
	case "session_done":
		return n.SessionDone
	case "session_failed":
		return n.SessionFailed
	case "session_interrupted":
		return n.SessionInterrupted
	case "question_asked":
		return n.QuestionAsked
	case "permission_asked":
		return n.PermissionAsked
	case "retry":
		return n.Retry
	}
	return false
}

func (a *App) fetchSession(sessionID string) (occlient.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return a.oc.GetSession(ctx, sessionID)
}

func (a *App) lastAssistantText(sessionID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	text, err := a.oc.LastAssistantText(ctx, sessionID)
	if err != nil {
		log.Printf("notification: could not read messages for %s: %v", sessionID, err)
		return ""
	}
	return text
}

func sessionDuration(s occlient.Session) string {
	if s.Time.Created <= 0 {
		return ""
	}
	end := s.Time.Updated
	if end <= s.Time.Created {
		end = time.Now().UnixMilli()
	}
	d := time.Duration(end-s.Time.Created) * time.Millisecond
	if d < time.Second {
		return ""
	}
	return formatDuration(d)
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func (a *App) notifyRetry(sessionID string, attempt int, message string) {
	n := a.settings.Get().Notifications
	if !n.Enabled || !n.Retry {
		return
	}
	if !a.markOnce(fmt.Sprintf("retry:%s:%d", sessionID, attempt), 10*time.Minute) {
		return
	}
	title := a.sessionLabel(sessionID)
	text := fmt.Sprintf("⚠️ OpenCode retry #%d\nTitle: %s", attempt, title)
	if message != "" {
		text += "\n\n" + truncateText(message, 1200)
	}
	a.broadcast(text, tgbotapi.InlineKeyboardMarkup{})
}

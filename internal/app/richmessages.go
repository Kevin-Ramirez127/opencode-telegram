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

// Long assistant messages are delivered either block by block or as a short
// preview with an on-demand summary ("resume"). These prompts are used for the
// summary actions and never mutate the OpenCode session history.
const (
	messageSummaryPrompt = "Summarize your most recent assistant response for a chat notification. " +
		"Keep every important fact, decision, command, and next step. Be concise, use short bullets, " +
		"do not call tools, and do not modify files. Output only the summary."
	thinkingSummaryPrompt = "Summarize the reasoning/thinking from your most recent assistant message. " +
		"Keep the key decisions, assumptions, and open questions. Be concise, do not call tools, " +
		"and do not modify files. Output only the summary."
	thinkingResumePrompt = "Continue from your previous reasoning. Resume the task without repeating work " +
		"you already completed. If you were interrupted, pick up exactly where you left off."
)

// outgoingMessage is one Telegram message and its optional inline keyboard.
type outgoingMessage struct {
	Text   string
	Markup tgbotapi.InlineKeyboardMarkup
}

// notificationMarkup builds the buttons attached to a session notification.
// hasThinking controls the thinking buttons, and offerSummary adds the
// on-demand "Ask for resume" button for oversized messages.
func (a *App) notificationMarkup(sessionID string, hasThinking, offerSummary bool) tgbotapi.InlineKeyboardMarkup {
	st := a.settings.Get()
	var rows [][]tgbotapi.InlineKeyboardButton

	if st.Security.AllowNewMessages {
		token := a.addAction(action{Kind: "bind_reply", SessionID: sessionID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Reply to this session", callbackData(token)),
		})
	}
	if hasThinking && st.Notifications.ShowRawThinking {
		token := a.addAction(action{Kind: "show_thinking", SessionID: sessionID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🧠 Show raw thinking", callbackData(token)),
		})
	}
	if hasThinking && st.Security.AllowResumeThinking {
		token := a.addAction(action{Kind: "resume_thinking", SessionID: sessionID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("↩️ Resume thinking", callbackData(token)),
		})
	}
	if offerSummary {
		token := a.addAction(action{Kind: "resume_message", SessionID: sessionID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📄 Ask for resume", callbackData(token)),
		})
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// broadcastParts sends the same sequence of message parts to every notify
// chat. Each part is already sized for Telegram.
func (a *App) broadcastParts(parts []outgoingMessage) []sentMessage {
	if len(parts) == 0 {
		return nil
	}
	chats := a.notifyChats()
	out := make([]sentMessage, 0, len(chats)*len(parts))
	for _, chatID := range chats {
		for _, part := range parts {
			msg, err := a.sendMarkup(chatID, part.Text, part.Markup)
			if err != nil {
				continue
			}
			out = append(out, sentMessage{ChatID: chatID, MessageID: msg.MessageID})
		}
	}
	return out
}

// sendPartsToChat sends a sequence of already-split parts to one chat.
func (a *App) sendPartsToChat(chatID int64, parts []string) {
	for _, part := range parts {
		a.send(chatID, part)
	}
}

// contentMessages turns a header plus content blocks into Telegram-sized
// messages. Every text block gets its own message; only a single block that is
// itself too big is split further.
func contentMessages(header string, blocks []string) []string {
	var out []string
	if strings.TrimSpace(header) != "" {
		out = append(out, splitTelegramText(header, maxTelegramText)...)
	}
	for _, block := range blocks {
		out = append(out, splitTelegramText(block, maxTelegramText)...)
	}
	return out
}

// splitTelegramText splits text into messages of at most limit bytes, preferring
// paragraph and line boundaries and never cutting a UTF-8 rune.
func splitTelegramText(s string, limit int) []string {
	s = sanitizeUTF8(s)
	if limit <= 0 {
		limit = maxTelegramText
	}
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if len(s) <= limit {
		return []string{s}
	}

	var out []string
	for len(s) > limit {
		window := s[:limit]
		cut := -1
		for _, sep := range []string{"\n\n", "\n", " "} {
			if idx := strings.LastIndex(window, sep); idx >= limit/2 {
				cut = idx + len(sep)
				break
			}
		}
		if cut <= 0 {
			cut = len(truncateUTF8Bytes(s, limit))
		}
		if cut <= 0 {
			break
		}
		if chunk := strings.TrimRight(s[:cut], "\n"); strings.TrimSpace(chunk) != "" {
			out = append(out, chunk)
		}
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if strings.TrimSpace(s) != "" {
		out = append(out, s)
	}
	return out
}

func (a *App) lastAssistantMessage(sessionID string) (occlient.Message, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	msg, ok, err := a.oc.LastAssistantMessage(ctx, sessionID)
	if err != nil {
		log.Printf("notification: could not read messages for %s: %v", sessionID, err)
		return occlient.Message{}, false
	}
	return msg, ok
}

func (a *App) lastAssistantWithReasoning(sessionID string) (occlient.Message, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	msg, ok, err := a.oc.LastAssistantWithReasoning(ctx, sessionID)
	if err != nil {
		log.Printf("thinking: could not read messages for %s: %v", sessionID, err)
		return occlient.Message{}, false
	}
	return msg, ok
}

func (a *App) generateText(sessionID, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return a.oc.Generate(ctx, sessionID, prompt)
}

// showRawThinking sends the raw reasoning blocks of the newest assistant
// message that has any.
func (a *App) showRawThinking(chatID int64, act action) {
	if !a.settings.Get().Notifications.ShowRawThinking {
		a.send(chatID, "Showing raw thinking is disabled in the OpenCode Telegram plugin.")
		return
	}
	msg, ok := a.lastAssistantWithReasoning(act.SessionID)
	if !ok {
		a.send(chatID, "No thinking found for the latest message of this session.")
		return
	}
	header := fmt.Sprintf("🧠 Raw thinking\nSession: %s", a.sessionLabel(act.SessionID))
	a.sendPartsToChat(chatID, contentMessages(header, msg.ReasoningParts()))
}

// resumeThinking asks OpenCode either to continue the run or to summarize the
// reasoning, depending on the plugin setting.
func (a *App) resumeThinking(chatID int64, act action) {
	st := a.settings.Get()
	if !st.Security.AllowResumeThinking {
		a.send(chatID, "Resuming thinking is disabled. Enable it in the OpenCode Telegram plugin first.")
		return
	}
	if _, ok := a.lastAssistantWithReasoning(act.SessionID); !ok {
		a.send(chatID, "No thinking found for the latest message of this session.")
		return
	}

	if st.Notifications.ResumeThinkingMode == "summary" {
		text, err := a.generateText(act.SessionID, thinkingSummaryPrompt)
		if err != nil {
			a.send(chatID, fmt.Sprintf("❌ Could not summarize the thinking: %v", err))
			return
		}
		if text == "" {
			a.send(chatID, "OpenCode returned an empty summary.")
			return
		}
		header := fmt.Sprintf("🧠 Thinking resume\nSession: %s", a.sessionLabel(act.SessionID))
		a.sendPartsToChat(chatID, contentMessages(header, []string{text}))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.oc.PromptResume(ctx, act.SessionID, thinkingResumePrompt); err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not resume thinking: %v", err))
		return
	}
	a.send(chatID, fmt.Sprintf("▶️ Asked OpenCode to resume thinking for %s.", a.sessionLabel(act.SessionID)))
}

// resumeMessage sends a generated summary of the last assistant message. It is
// used when "long_message_mode" is "summary" and the user taps the button on an
// oversized notification.
func (a *App) resumeMessage(chatID int64, act action) {
	msg, ok := a.lastAssistantMessage(act.SessionID)
	if !ok {
		a.send(chatID, "No assistant message found for this session.")
		return
	}
	text, err := a.generateText(act.SessionID, messageSummaryPrompt)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not generate a resume: %v", err))
		return
	}
	if text == "" {
		text = truncateText(msg.FinalText(), maxTelegramText)
	}
	if strings.TrimSpace(text) == "" {
		a.send(chatID, "OpenCode returned an empty resume.")
		return
	}
	header := fmt.Sprintf("📄 Resume of the last message\nSession: %s", a.sessionLabel(act.SessionID))
	a.sendPartsToChat(chatID, contentMessages(header, []string{text}))
}

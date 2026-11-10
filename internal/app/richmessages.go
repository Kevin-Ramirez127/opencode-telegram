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

// Summary actions use the stateless generation endpoint with the exact text
// that was shown, so they never summarize a newer message by mistake.
const (
	messageSummaryPrompt = "Summarize the following assistant output for a chat notification. " +
		"Keep every important fact, decision, command, and next step. Be concise, use short bullets, " +
		"do not call tools, and do not modify files. Output only the summary."
	thinkingSummaryPrompt = "Summarize the following reasoning/thinking for a chat notification. " +
		"Keep the key decisions, assumptions, and open questions. Be concise, do not call tools, " +
		"and do not modify files. Output only the summary."
	maxSummaryInput = 32000
)

// outgoingMessage is one Telegram message and its optional inline keyboard.
type outgoingMessage struct {
	Text   string
	Markup tgbotapi.InlineKeyboardMarkup
}

// notificationMarkup builds the buttons attached to a session notification.
// The buttons target the exact assistant message that produced the
// notification.
func (a *App) notificationMarkup(sessionID, messageID string, includeOutput, hasThinking bool) tgbotapi.InlineKeyboardMarkup {
	st := a.settings.Get()
	var rows [][]tgbotapi.InlineKeyboardButton

	if includeOutput {
		token := a.addAction(action{Kind: "summarize_output", SessionID: sessionID, MessageID: messageID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📄 Summarize output", callbackData(token)),
		})
	}
	if hasThinking {
		token := a.addAction(action{Kind: "show_thinking", SessionID: sessionID, MessageID: messageID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🧠 Show thinking", callbackData(token)),
		})
	}
	if st.Security.AllowNewMessages {
		token := a.addAction(action{Kind: "bind_reply", SessionID: sessionID})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Reply to this session", callbackData(token)),
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
	a.sendPartsToChatWithMarkup(chatID, parts, tgbotapi.InlineKeyboardMarkup{})
}

// sendPartsToChatWithMarkup sends parts to one chat and attaches markup to the
// final part only.
func (a *App) sendPartsToChatWithMarkup(chatID int64, parts []string, markup tgbotapi.InlineKeyboardMarkup) {
	for i, part := range parts {
		if i == len(parts)-1 {
			a.sendMarkup(chatID, part, markup)
			continue
		}
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

// messageForAction resolves the exact message a button was created for. It
// only falls back to the latest message for very old action tokens.
func (a *App) messageForAction(act action) (occlient.Message, error) {
	if act.MessageID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return a.oc.GetMessage(ctx, act.SessionID, act.MessageID)
	}
	msg, ok := a.lastAssistantMessage(act.SessionID)
	if !ok {
		return occlient.Message{}, fmt.Errorf("no assistant message found for session %s", act.SessionID)
	}
	return msg, nil
}

func (a *App) generateSummary(sessionID, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return a.oc.Generate(ctx, sessionID, prompt)
}

// showThinking sends the raw reasoning blocks of the message the button was
// created for.
func (a *App) showThinking(chatID int64, act action) {
	done := a.startLoading(chatID, "⏳ Loading thinking…")
	msg, err := a.messageForAction(act)
	if err != nil {
		done()
		a.send(chatID, fmt.Sprintf("❌ Could not read the thinking: %v", err))
		return
	}
	reasoning := msg.ReasoningParts()
	if len(reasoning) == 0 {
		done()
		a.send(chatID, "No thinking found for this message.")
		return
	}

	header := fmt.Sprintf("🧠 Raw thinking\nSession: %s", a.sessionLabel(act.SessionID))
	markup := tgbotapi.InlineKeyboardMarkup{}
	if a.settings.Get().Notifications.ShowSummarizeThinking {
		token := a.addAction(action{
			Kind:      "summarize_thinking",
			SessionID: act.SessionID,
			MessageID: msg.ID,
		})
		markup = tgbotapi.NewInlineKeyboardMarkup([]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📝 Summarize thinking", callbackData(token)),
		})
	}
	done()
	a.sendPartsToChatWithMarkup(chatID, contentMessages(header, reasoning), markup)
}

// summarizeThinking summarizes the reasoning of the message the button was
// created for.
func (a *App) summarizeThinking(chatID int64, act action) {
	done := a.startLoading(chatID, "⏳ Summarizing thinking…")
	msg, err := a.messageForAction(act)
	if err != nil {
		done()
		a.send(chatID, fmt.Sprintf("❌ Could not read the thinking: %v", err))
		return
	}
	reasoning := msg.ReasoningText()
	if reasoning == "" {
		done()
		a.send(chatID, "No thinking found for this message.")
		return
	}

	text, err := a.generateSummary(act.SessionID, thinkingSummaryPrompt+"\n\n---\n\n"+truncateText(reasoning, maxSummaryInput))
	if err != nil {
		done()
		a.send(chatID, fmt.Sprintf("❌ Could not summarize the thinking: %v", err))
		return
	}
	if text == "" {
		done()
		a.send(chatID, "OpenCode returned an empty summary.")
		return
	}

	header := fmt.Sprintf("🧠 Thinking summary\nSession: %s", a.sessionLabel(act.SessionID))
	done()
	a.sendPartsToChat(chatID, contentMessages(header, []string{text}))
}

// summarizeOutput summarizes the assistant output of the message the button was
// created for.
func (a *App) summarizeOutput(chatID int64, act action) {
	done := a.startLoading(chatID, "⏳ Summarizing output…")
	msg, err := a.messageForAction(act)
	if err != nil {
		done()
		a.send(chatID, fmt.Sprintf("❌ Could not read the output: %v", err))
		return
	}
	output := msg.FinalText()
	if output == "" {
		done()
		a.send(chatID, "No assistant output found for this message.")
		return
	}

	text, err := a.generateSummary(act.SessionID, messageSummaryPrompt+"\n\n---\n\n"+truncateText(output, maxSummaryInput))
	if err != nil {
		done()
		a.send(chatID, fmt.Sprintf("❌ Could not summarize the output: %v", err))
		return
	}
	if text == "" {
		done()
		a.send(chatID, "OpenCode returned an empty summary.")
		return
	}

	header := fmt.Sprintf("📄 Output summary\nSession: %s", a.sessionLabel(act.SessionID))
	done()
	a.sendPartsToChat(chatID, contentMessages(header, []string{text}))
}

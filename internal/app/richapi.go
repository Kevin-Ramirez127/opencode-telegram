package app

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// richMessageContent is the markdown variant of Telegram's InputRichMessage.
// The daemon gets its content from OpenCode, which already speaks Markdown.
type richMessageContent struct {
	Markdown string `json:"markdown,omitempty"`
}

func (a *App) sendRichMarkup(chatID int64, text string, markup tgbotapi.InlineKeyboardMarkup) (tgbotapi.Message, error) {
	params := tgbotapi.Params{
		"chat_id": strconv.FormatInt(chatID, 10),
	}
	if err := params.AddInterface("rich_message", richMessageContent{Markdown: text}); err != nil {
		return tgbotapi.Message{}, fmt.Errorf("encode rich message: %w", err)
	}
	if len(markup.InlineKeyboard) > 0 {
		if err := params.AddInterface("reply_markup", markup); err != nil {
			return tgbotapi.Message{}, fmt.Errorf("encode reply markup: %w", err)
		}
	}

	resp, err := a.bot.MakeRequest("sendRichMessage", params)
	if err != nil {
		return tgbotapi.Message{}, err
	}
	var sent tgbotapi.Message
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &sent); err != nil {
			return tgbotapi.Message{}, fmt.Errorf("decode rich message response: %w", err)
		}
	}
	return sent, nil
}

func (a *App) editRichMarkup(chatID int64, messageID int, text string, markup *tgbotapi.InlineKeyboardMarkup) error {
	params := tgbotapi.Params{
		"chat_id":    strconv.FormatInt(chatID, 10),
		"message_id": strconv.Itoa(messageID),
	}
	if err := params.AddInterface("rich_message", richMessageContent{Markdown: text}); err != nil {
		return fmt.Errorf("encode rich message: %w", err)
	}

	// Rich messages need an explicit empty keyboard to remove old buttons.
	empty := emptyInlineKeyboard()
	if markup != nil {
		if err := params.AddInterface("reply_markup", *markup); err != nil {
			return fmt.Errorf("encode reply markup: %w", err)
		}
	} else if err := params.AddInterface("reply_markup", empty); err != nil {
		return fmt.Errorf("encode empty reply markup: %w", err)
	}

	_, err := a.bot.MakeRequest("editMessageText", params)
	return err
}

// logRichFallback is best-effort: older Telegram servers or local Bot API
// servers may not know sendRichMessage yet. The plain-text path keeps the bot
// usable there.
func logRichFallback(action string, chatID int64, err error) {
	log.Printf("rich %s failed for chat %d, falling back to plain text: %v", action, chatID, err)
}

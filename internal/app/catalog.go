package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/occlient"
)

func (a *App) showAgents(chatID int64) {
	sessionID := a.binding(chatID)
	if sessionID == "" {
		a.send(chatID, "No session bound. Send /sessions to pick one.")
		return
	}
	stopLoading := a.startDelayedLoading(chatID, "⏳ Loading agents…", 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := a.oc.GetSession(ctx, sessionID)
	if err != nil {
		stopLoading()
		a.send(chatID, fmt.Sprintf("❌ Could not read session %s: %v", sessionID, err))
		return
	}
	a.rememberSession(sess)

	agents, err := a.oc.ListAgents(ctx, sess.Location.Directory)
	if err != nil {
		stopLoading()
		a.send(chatID, fmt.Sprintf("❌ Could not list agents: %v", err))
		return
	}

	switchable := make([]occlient.AgentInfo, 0, len(agents))
	for _, agent := range agents {
		if agent.Hidden || agent.ID == "" {
			continue
		}
		if agent.Mode != "primary" && agent.Mode != "all" {
			continue
		}
		switchable = append(switchable, agent)
	}
	if len(switchable) == 0 {
		// Be permissive with older servers that do not report modes.
		for _, agent := range agents {
			if !agent.Hidden && agent.ID != "" {
				switchable = append(switchable, agent)
			}
		}
	}
	if len(switchable) == 0 {
		stopLoading()
		a.send(chatID, "No switchable agents found.")
		return
	}

	current := sess.Agent
	if current == "" {
		current = "default"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🧩 Agents for %s\n", sess.DisplayTitle())
	fmt.Fprintf(&b, "Current: `%s`\n\n", current)
	b.WriteString("Tap an agent to use it for subsequent turns.")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(switchable))
	for _, agent := range switchable {
		label := agentLabel(agent)
		if agent.ID == sess.Agent {
			label = "✓ " + label
		}
		token := a.addAction(action{
			Kind:      "agent",
			SessionID: sessionID,
			Value:     agent.ID,
			ChatID:    chatID,
		})
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(truncateOneLine(label, 60), callbackData(token)),
		})
	}
	stopLoading()
	a.sendMarkup(chatID, b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

func agentLabel(agent occlient.AgentInfo) string {
	if strings.TrimSpace(agent.Name) != "" {
		return fmt.Sprintf("%s (%s)", agent.Name, agent.ID)
	}
	return agent.ID
}

func (a *App) switchAgent(chatID int64, messageID int, sessionID, agentID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.oc.SwitchAgent(ctx, sessionID, agentID); err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not switch agent: %v", err))
		return
	}

	if sess, err := a.oc.GetSession(ctx, sessionID); err == nil {
		a.rememberSession(sess)
	}

	text := fmt.Sprintf("✅ Agent switched to `%s`\nSession: %s", agentID, a.sessionLabel(sessionID))
	if messageID > 0 {
		a.editMarkup(chatID, messageID, text, nil)
		return
	}
	a.send(chatID, text)
}

func (a *App) switchAgentFromCommand(chatID int64, sessionID, agentID string) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		a.send(chatID, "Usage: /agents <agent-id>")
		return
	}
	a.switchAgent(chatID, 0, sessionID, agentID)
}

const modelSwitchingDisabled = "⚠️ Your configuration does not allow choosing models from Telegram.\n\n" +
	"Enable \"Security: model switching\" in the OpenCode Telegram plugin settings."

func (a *App) showModels(chatID int64) {
	if !a.settings.Get().Security.AllowModelSwitching {
		a.send(chatID, modelSwitchingDisabled)
		return
	}
	sessionID := a.binding(chatID)
	if sessionID == "" {
		a.send(chatID, "No session bound. Send /sessions to pick one.")
		return
	}
	stopLoading := a.startDelayedLoading(chatID, "⏳ Loading models…", 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sess, err := a.oc.GetSession(ctx, sessionID)
	if err != nil {
		stopLoading()
		a.send(chatID, fmt.Sprintf("❌ Could not read session %s: %v", sessionID, err))
		return
	}
	a.rememberSession(sess)

	models, err := a.oc.ListModels(ctx, sess.Location.Directory)
	if err != nil {
		stopLoading()
		a.send(chatID, fmt.Sprintf("❌ Could not list models: %v", err))
		return
	}

	enabled := make([]occlient.ModelInfo, 0, len(models))
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		if model.Type != "" && model.Type != "chat" {
			continue
		}
		enabled = append(enabled, model)
	}
	if len(enabled) == 0 {
		stopLoading()
		a.send(chatID, "No chat models found.")
		return
	}

	current := sess.Model.Key()
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 Available models (%d)\n", len(enabled))
	fmt.Fprintf(&b, "Session: %s\n", sess.DisplayTitle())
	if current != "" {
		fmt.Fprintf(&b, "Current: `%s`\n", current)
	}
	b.WriteString("\nTap a model to use it for subsequent turns.")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, (len(enabled)+1)/2)
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	for _, model := range enabled {
		label := model.Name
		if strings.TrimSpace(label) == "" {
			label = model.ModelID
		}
		if model.ProviderID != "" {
			label = fmt.Sprintf("%s (%s)", label, model.ProviderID)
		}
		if current == model.ProviderID+"/"+model.ModelID {
			label = "✓ " + label
		}
		token := a.addAction(action{
			Kind:      "model",
			SessionID: sessionID,
			Value:     model.ModelID,
			Decision:  model.ProviderID,
			ChatID:    chatID,
		})
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(truncateOneLine(label, 60), callbackData(token)))
		if len(row) == 2 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}

	stopLoading()
	a.sendMarkup(chatID, b.String(), tgbotapi.NewInlineKeyboardMarkup(rows...))
}

func (a *App) switchModel(chatID int64, messageID int, sessionID, providerID, modelID string) {
	if !a.settings.Get().Security.AllowModelSwitching {
		a.send(chatID, modelSwitchingDisabled)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	model := occlient.ModelRef{ID: modelID, ProviderID: providerID}
	if err := a.oc.SwitchModel(ctx, sessionID, model); err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not switch model: %v", err))
		return
	}

	if sess, err := a.oc.GetSession(ctx, sessionID); err == nil {
		a.rememberSession(sess)
	}

	text := fmt.Sprintf("✅ Model switched to `%s`\nSession: %s", model.Key(), a.sessionLabel(sessionID))
	if messageID > 0 {
		a.editMarkup(chatID, messageID, text, nil)
		return
	}
	a.send(chatID, text)
}

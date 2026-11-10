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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := a.oc.GetSession(ctx, sessionID)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not read session %s: %v", sessionID, err))
		return
	}
	a.rememberSession(sess)

	agents, err := a.oc.ListAgents(ctx, sess.Location.Directory)
	if err != nil {
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

func (a *App) showModels(chatID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sess occlient.Session
	sessionID := a.binding(chatID)
	if sessionID != "" {
		if got, err := a.oc.GetSession(ctx, sessionID); err == nil {
			sess = got
			a.rememberSession(got)
		}
	}

	models, err := a.oc.ListModels(ctx, sess.Location.Directory)
	if err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not list models: %v", err))
		return
	}

	type providerGroup struct {
		name   string
		models []occlient.ModelInfo
	}
	var groups []providerGroup
	index := make(map[string]int)
	total := 0
	for _, model := range models {
		if !model.Enabled {
			continue
		}
		if model.Type != "" && model.Type != "chat" {
			continue
		}
		total++
		idx, ok := index[model.ProviderID]
		if !ok {
			idx = len(groups)
			index[model.ProviderID] = idx
			groups = append(groups, providerGroup{name: model.ProviderID})
		}
		groups[idx].models = append(groups[idx].models, model)
	}
	if total == 0 {
		a.send(chatID, "No chat models found.")
		return
	}

	current := sess.Model.Key()
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 Available models (%d)\n", total)
	if sessionID != "" {
		fmt.Fprintf(&b, "Session: %s\n", a.sessionLabel(sessionID))
		if current != "" {
			fmt.Fprintf(&b, "Current: `%s`\n", current)
		}
	}
	for _, group := range groups {
		fmt.Fprintf(&b, "\n**%s**\n", group.name)
		for _, model := range group.models {
			marker := "•"
			if current == model.ProviderID+"/"+model.ModelID {
				marker = "✅"
			}
			status := ""
			if model.Status != "" && model.Status != "active" {
				status = " (" + model.Status + ")"
			}
			name := model.Name
			if strings.TrimSpace(name) == "" {
				name = model.ModelID
			}
			fmt.Fprintf(&b, "%s %s — `%s`%s\n", marker, name, model.ModelID, status)
		}
	}

	a.sendPartsToChat(chatID, splitTelegramText(b.String(), maxTelegramText))
}

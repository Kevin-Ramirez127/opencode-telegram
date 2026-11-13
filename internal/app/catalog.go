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
	fmt.Fprintf(&b, "**🧠 Available models (%d)**\n\n", len(enabled))
	fmt.Fprintf(&b, "Session: %s\n", sess.DisplayTitle())
	if current != "" {
		fmt.Fprintf(&b, "Current model: **%s** (`%s`)\n", modelNameFor(models, current), current)
		if sess.Model.Variant != "" {
			fmt.Fprintf(&b, "Current effort: `%s`\n", sess.Model.Variant)
		}
	} else {
		b.WriteString("Current model: **OpenCode default**\n")
	}
	b.WriteString("\nTap a model to use it for subsequent turns.")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, (len(enabled)+1)/2)
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	for _, model := range enabled {
		label := modelLabel(model)
		if current == modelKey(model) {
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

func modelKey(model occlient.ModelInfo) string {
	id := model.ModelID
	if strings.TrimSpace(id) == "" {
		id = model.ID
	}
	if strings.TrimSpace(model.ProviderID) == "" || strings.TrimSpace(id) == "" {
		return ""
	}
	return model.ProviderID + "/" + id
}

func modelLabel(model occlient.ModelInfo) string {
	label := strings.TrimSpace(model.Name)
	if label == "" {
		label = model.ModelID
	}
	if label == "" {
		label = model.ID
	}
	if model.ProviderID != "" {
		label = fmt.Sprintf("%s (%s)", label, model.ProviderID)
	}
	return label
}

func modelNameFor(models []occlient.ModelInfo, key string) string {
	for _, model := range models {
		if modelKey(model) == key {
			return modelLabel(model)
		}
	}
	return key
}

func findModel(models []occlient.ModelInfo, key string) (occlient.ModelInfo, bool) {
	for _, model := range models {
		if modelKey(model) == key {
			return model, true
		}
	}
	return occlient.ModelInfo{}, false
}

func (a *App) showEffort(chatID int64) {
	if !a.settings.Get().Security.AllowModelSwitching {
		a.send(chatID, modelSwitchingDisabled)
		return
	}
	sessionID := a.binding(chatID)
	if sessionID == "" {
		a.send(chatID, "No session bound. Send /sessions to pick one.")
		return
	}
	stopLoading := a.startDelayedLoading(chatID, "⏳ Loading effort levels…", 250*time.Millisecond)

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

	model, ok := findModel(models, sess.Model.Key())
	if !ok && sess.Model.Key() == "" {
		if def, err := a.oc.DefaultModel(ctx, sess.Location.Directory); err == nil && def != nil {
			model, ok = *def, true
		}
	}
	if !ok {
		stopLoading()
		a.send(chatID, "❌ Could not determine the session's current model. Use `/models` to pick one.")
		return
	}
	if len(model.Variants) == 0 {
		stopLoading()
		a.send(chatID, fmt.Sprintf("No effort levels are available for **%s**.", modelLabel(model)))
		return
	}

	current := sess.Model.Variant
	var b strings.Builder
	b.WriteString("**🎚 Effort level**\n\n")
	fmt.Fprintf(&b, "- **Session:** %s\n", sess.DisplayTitle())
	fmt.Fprintf(&b, "- **Model:** **%s** (`%s`)\n", modelLabel(model), modelKey(model))
	if current == "" {
		b.WriteString("- **Current effort:** model default")
	} else {
		fmt.Fprintf(&b, "- **Current effort:** `%s`", current)
	}
	b.WriteString("\n\nTap an effort level to use it for subsequent turns.")

	rows := make([][]tgbotapi.InlineKeyboardButton, 0, (len(model.Variants)+1)/2)
	row := make([]tgbotapi.InlineKeyboardButton, 0, 2)
	modelID := model.ModelID
	if strings.TrimSpace(modelID) == "" {
		modelID = model.ID
	}
	for _, variant := range model.Variants {
		label := variant.ID
		if label == "" {
			continue
		}
		if label == current {
			label = "✅ " + label
		}
		token := a.addAction(action{
			Kind:      "effort",
			SessionID: sessionID,
			ModelID:   modelID,
			Value:     variant.ID,
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

func (a *App) switchEffort(chatID int64, messageID int, act action) {
	if !a.settings.Get().Security.AllowModelSwitching {
		a.send(chatID, modelSwitchingDisabled)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	model := occlient.ModelRef{
		ID:         act.ModelID,
		ProviderID: act.Decision,
		Variant:    act.Value,
	}
	if err := a.oc.SwitchModel(ctx, act.SessionID, model); err != nil {
		a.send(chatID, fmt.Sprintf("❌ Could not switch effort: %v", err))
		return
	}
	if sess, err := a.oc.GetSession(ctx, act.SessionID); err == nil {
		a.rememberSession(sess)
	}

	text := fmt.Sprintf("✅ Effort set to `%s`\nModel: `%s`\nSession: %s",
		act.Value, model.Key(), a.sessionLabel(act.SessionID))
	if messageID > 0 {
		a.editMarkup(chatID, messageID, text, nil)
		return
	}
	a.send(chatID, text)
}

// lastUsedModel returns the model from the most recent session, preferring a
// session in the same directory. It lets new sessions and sessions without an
// explicit model keep using the model the user last worked with.
func (a *App) lastUsedModel(ctx context.Context, directory string) (occlient.ModelRef, bool) {
	sessions, err := a.oc.ListSessions(ctx, 20)
	if err != nil {
		return occlient.ModelRef{}, false
	}
	directory = strings.TrimSpace(directory)
	var fallback occlient.ModelRef
	for _, sess := range sessions {
		if sess.Model.Key() == "" {
			continue
		}
		if fallback.Key() == "" {
			fallback = sess.Model
		}
		if directory != "" && strings.TrimSpace(sess.Location.Directory) == directory {
			return sess.Model, true
		}
	}
	if fallback.Key() != "" {
		return fallback, true
	}
	return occlient.ModelRef{}, false
}

// ensureSessionModel applies the last used model when a session has no
// explicit selection. An existing session model is never overridden.
func (a *App) ensureSessionModel(ctx context.Context, sess occlient.Session) occlient.Session {
	if sess.Model.Key() != "" {
		return sess
	}
	model, ok := a.lastUsedModel(ctx, sess.Location.Directory)
	if !ok {
		return sess
	}
	if err := a.oc.SwitchModel(ctx, sess.ID, model); err != nil {
		return sess
	}
	sess.Model = model
	return sess
}

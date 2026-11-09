package app

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"opencode-telegram/internal/occlient"
)

type formFlow struct {
	Form       occlient.Form
	SessionID  string
	Answers    map[string]any
	Defaults   map[string]any
	Skipped    map[string]bool
	Multi      map[string]map[string]bool
	MsgIDs     map[int64]int
	Pos        int
	CurrentKey string
	Created    time.Time
}

type pendingInput struct {
	Kind     string
	FormID   string
	FieldKey string
	Created  time.Time
}

// handleFormCreated is called for live form.created events and during
// reconnect reconciliation.
func (a *App) handleFormCreated(f occlient.Form) {
	st := a.settings.Get()
	if !st.Notifications.Enabled || !st.Notifications.QuestionAsked {
		return
	}
	if f.ID == "" {
		return
	}
	if !a.markOnce("form:"+f.ID, 24*time.Hour) {
		return
	}
	a.formMu.Lock()
	defer a.formMu.Unlock()
	a.mu.Lock()
	_, exists := a.forms[f.ID]
	a.mu.Unlock()
	if exists {
		return
	}
	a.startFormFlow(f)
}

func (a *App) startFormFlow(f occlient.Form) {
	flow := &formFlow{
		Form:      f,
		SessionID: f.SessionID,
		Answers:   make(map[string]any),
		Defaults:  make(map[string]any),
		Skipped:   make(map[string]bool),
		Multi:     make(map[string]map[string]bool),
		MsgIDs:    make(map[int64]int),
		Created:   time.Now(),
	}
	for _, field := range f.Fields {
		if len(field.Default) > 0 {
			var v any
			if err := parseJSON(field.Default, &v); err == nil {
				flow.Defaults[field.Key] = v
				if field.Hidden {
					flow.Answers[field.Key] = v
				}
			}
		}
		if field.Hidden {
			flow.Skipped[field.Key] = true
		}
	}

	a.mu.Lock()
	a.forms[f.ID] = flow
	a.mu.Unlock()

	header := fmt.Sprintf("❓ OpenCode is asking a question\nTitle: %s\nSession: %s",
		blank(f.Title, "(untitled form)"), a.sessionLabel(f.SessionID))
	if dir := a.sessionSnapshot(f.SessionID).Directory; dir != "" {
		header += "\nDirectory: " + dir
	}
	a.broadcast(header, tgbotapi.InlineKeyboardMarkup{})
	a.sendNextField(flow)
}

func (a *App) sendNextField(flow *formFlow) {
	a.clearFieldMessages(flow)
	for flow.Pos < len(flow.Form.Fields) {
		field := flow.Form.Fields[flow.Pos]
		if field.Hidden || flow.Skipped[field.Key] {
			flow.Pos++
			continue
		}
		if _, answered := flow.Answers[field.Key]; answered {
			flow.Pos++
			continue
		}
		if !a.whenOK(flow, field) {
			flow.Skipped[field.Key] = true
			flow.Pos++
			continue
		}
		flow.CurrentKey = field.Key
		a.promptField(flow, field)
		return
	}
	a.submitForm(flow)
}

func (a *App) promptField(flow *formFlow, field occlient.FormField) {
	text := a.fieldPromptText(flow, field)
	markup, answerable := a.fieldMarkup(flow, field)
	if !answerable {
		text += "\n\nThis field needs an answer in OpenCode. Answering here is disabled."
	}

	flow.MsgIDs = make(map[int64]int)
	if len(markup.InlineKeyboard) == 0 {
		for _, chatID := range a.notifyChats() {
			msg, err := a.sendMarkup(chatID, text, tgbotapi.InlineKeyboardMarkup{})
			if err == nil {
				flow.MsgIDs[chatID] = msg.MessageID
			}
		}
		return
	}
	for _, chatID := range a.notifyChats() {
		msg, err := a.sendMarkup(chatID, text, markup)
		if err == nil {
			flow.MsgIDs[chatID] = msg.MessageID
		}
	}
}

func (a *App) fieldPromptText(flow *formFlow, field occlient.FormField) string {
	var b strings.Builder
	fmt.Fprintf(&b, "❓ %s\n", blank(flow.Form.Title, "OpenCode question"))
	if flow.SessionID != "" {
		fmt.Fprintf(&b, "Session: %s\n", a.sessionLabel(flow.SessionID))
	}
	fmt.Fprintf(&b, "\n%s\n", field.TitleOrKey())
	if field.Description != "" {
		fmt.Fprintf(&b, "%s\n", field.Description)
	}
	if def, ok := flow.Defaults[field.Key]; ok {
		fmt.Fprintf(&b, "Default: %s\n", formatAnswerValue(def))
	}
	if field.Required {
		b.WriteString("Required.\n")
	}
	return strings.TrimSpace(b.String())
}

func (a *App) fieldMarkup(flow *formFlow, field occlient.FormField) (tgbotapi.InlineKeyboardMarkup, bool) {
	var rows [][]tgbotapi.InlineKeyboardButton
	add := func(label string, act action) {
		act.FormID = flow.Form.ID
		act.FieldKey = field.Key
		token := a.addAction(act)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(truncateOneLine(label, 60), callbackData(token)),
		})
	}

	allowCustom := a.settings.Get().Security.AllowCustomFormAnswers
	answerable := false

	switch field.Type {
	case "boolean":
		add("Yes", action{Kind: "form_opt", Value: "true"})
		add("No", action{Kind: "form_opt", Value: "false"})
		answerable = true

	case "multiselect":
		selected := flow.Multi[field.Key]
		if selected == nil {
			selected = make(map[string]bool)
			flow.Multi[field.Key] = selected
		}
		var row []tgbotapi.InlineKeyboardButton
		for _, opt := range field.Options {
			mark := "☐"
			if selected[opt.Value] {
				mark = "☑"
			}
			act := action{Kind: "form_toggle", Value: opt.Value}
			act.FormID = flow.Form.ID
			act.FieldKey = field.Key
			token := a.addAction(act)
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(
				truncateOneLine(mark+" "+opt.Label, 30), callbackData(token)))
			if len(row) == 2 {
				rows = append(rows, row)
				row = nil
			}
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
		add("Done", action{Kind: "form_done"})
		answerable = len(field.Options) > 0

	case "string":
		if len(field.Options) > 0 {
			var row []tgbotapi.InlineKeyboardButton
			for _, opt := range field.Options {
				act := action{Kind: "form_opt", Value: opt.Value}
				act.FormID = flow.Form.ID
				act.FieldKey = field.Key
				token := a.addAction(act)
				row = append(row, tgbotapi.NewInlineKeyboardButtonData(
					truncateOneLine(opt.Label, 30), callbackData(token)))
				if len(row) == 2 {
					rows = append(rows, row)
					row = nil
				}
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
			answerable = true
		}
		if allowCustom && (field.Custom || len(field.Options) == 0) {
			add("✍️ Type your own response", action{Kind: "form_custom"})
			answerable = true
		}

	case "number", "integer":
		if allowCustom {
			add("✍️ Type a number", action{Kind: "form_custom"})
			answerable = true
		}

	case "external":
		if field.Placeholder != "" {
			// The placeholder is not guaranteed to be a URL; the canonical URL
			// field is not exposed in the typed schema, so just point the user
			// at OpenCode.
			answerable = false
		}
	default:
		if allowCustom {
			add("✍️ Type your answer", action{Kind: "form_custom"})
			answerable = true
		}
	}

	if !field.Required {
		add("Skip", action{Kind: "form_skip"})
		answerable = true
	}

	return tgbotapi.NewInlineKeyboardMarkup(rows...), answerable
}

func (a *App) formChoose(chatID int64, act action, _ bool) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	flow := a.formFlow(act.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	field, ok := formField(flow, act.FieldKey)
	if !ok {
		a.send(chatID, "⚠️ Unknown field.")
		return
	}

	var value any = act.Value
	switch field.Type {
	case "boolean":
		value = act.Value == "true"
	case "number":
		if n, err := strconv.ParseFloat(act.Value, 64); err == nil {
			value = n
		}
	case "integer":
		if n, err := strconv.ParseInt(act.Value, 10, 64); err == nil {
			value = n
		}
	}
	a.setFieldAnswer(flow, field, value)
	a.sendNextField(flow)
}

func (a *App) formToggle(chatID int64, act action) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	flow := a.formFlow(act.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	field, ok := formField(flow, act.FieldKey)
	if !ok || field.Type != "multiselect" {
		a.send(chatID, "⚠️ Unknown option.")
		return
	}
	selected := flow.Multi[field.Key]
	if selected == nil {
		selected = make(map[string]bool)
		flow.Multi[field.Key] = selected
	}
	selected[act.Value] = !selected[act.Value]

	// Re-render this chat's field message so the checkbox reflects the choice.
	text := a.fieldPromptText(flow, field)
	markup, _ := a.fieldMarkup(flow, field)
	if id := flow.MsgIDs[chatID]; id != 0 {
		a.editMarkup(chatID, id, text, &markup)
	} else {
		msg, err := a.sendMarkup(chatID, text, markup)
		if err == nil {
			flow.MsgIDs[chatID] = msg.MessageID
		}
	}
}

func (a *App) formDone(chatID int64, act action) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	flow := a.formFlow(act.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	field, ok := formField(flow, act.FieldKey)
	if !ok {
		a.send(chatID, "⚠️ Unknown field.")
		return
	}
	selected := flow.Multi[field.Key]
	values := make([]string, 0, len(selected))
	for value, on := range selected {
		if on {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	a.setFieldAnswer(flow, field, values)
	a.sendNextField(flow)
}

func (a *App) formSkip(chatID int64, act action) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	flow := a.formFlow(act.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	field, ok := formField(flow, act.FieldKey)
	if !ok {
		a.send(chatID, "⚠️ Unknown field.")
		return
	}
	if field.Required {
		a.send(chatID, "This field is required.")
		return
	}
	if def, ok := flow.Defaults[field.Key]; ok {
		flow.Answers[field.Key] = def
	} else {
		flow.Skipped[field.Key] = true
	}
	a.sendNextField(flow)
}

func (a *App) formPromptCustom(chatID int64, act action) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	if !a.settings.Get().Security.AllowCustomFormAnswers {
		a.send(chatID, "Typing your own answer is disabled. Pick one of the provided options or answer in OpenCode.")
		return
	}
	flow := a.formFlow(act.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	field, ok := formField(flow, act.FieldKey)
	if !ok {
		a.send(chatID, "⚠️ Unknown field.")
		return
	}
	a.mu.Lock()
	a.pendingInput[chatID] = &pendingInput{
		Kind:     "form",
		FormID:   act.FormID,
		FieldKey: act.FieldKey,
		Created:  time.Now(),
	}
	a.mu.Unlock()

	cancel := a.addAction(action{Kind: "form_cancel_input"})
	markup := tgbotapi.NewInlineKeyboardMarkup([]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("Cancel", callbackData(cancel)),
	})
	a.sendMarkup(chatID, fmt.Sprintf("Type your answer for: %s", field.TitleOrKey()), markup)
}

func (a *App) hasPendingFormInput(chatID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.pendingInput[chatID]
	return ok
}

func (a *App) clearPendingInput(chatID int64) {
	a.mu.Lock()
	delete(a.pendingInput, chatID)
	a.mu.Unlock()
}

func (a *App) handleFormText(chatID int64, text string) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	a.mu.Lock()
	pi := a.pendingInput[chatID]
	delete(a.pendingInput, chatID)
	a.mu.Unlock()
	if pi == nil {
		return
	}
	if !a.settings.Get().Security.AllowCustomFormAnswers {
		a.send(chatID, "Typing your own answer is disabled.")
		return
	}
	flow := a.formFlow(pi.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	field, ok := formField(flow, pi.FieldKey)
	if !ok {
		a.send(chatID, "⚠️ Unknown field.")
		return
	}
	value, err := parseFieldValue(field, text)
	if err != nil {
		// Keep the prompt active so the user can correct the value.
		a.mu.Lock()
		a.pendingInput[chatID] = pi
		a.mu.Unlock()
		a.send(chatID, fmt.Sprintf("❌ %v\nTry again, or press Cancel.", err))
		return
	}
	a.setFieldAnswer(flow, field, value)
	a.sendNextField(flow)
}

func (a *App) setFieldAnswer(flow *formFlow, field occlient.FormField, value any) {
	flow.Answers[field.Key] = value
	delete(flow.Skipped, field.Key)
	// Replace the field message with a compact confirmation when possible.
	for chatID, msgID := range flow.MsgIDs {
		if msgID == 0 {
			continue
		}
		a.editMarkup(chatID, msgID,
			fmt.Sprintf("✓ %s: %s", field.TitleOrKey(), formatAnswerValue(value)), nil)
	}
}

func (a *App) clearFieldMessages(flow *formFlow) {
	for chatID, msgID := range flow.MsgIDs {
		if msgID == 0 {
			continue
		}
		a.editReplyMarkup(chatID, msgID)
	}
	flow.MsgIDs = make(map[int64]int)
}

func (a *App) formFlow(formID string) *formFlow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.forms[formID]
}

func (a *App) formRetrySubmit(chatID int64, act action) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	flow := a.formFlow(act.FormID)
	if flow == nil {
		a.send(chatID, "⚠️ This question is no longer pending.")
		return
	}
	a.submitForm(flow)
}

func formField(flow *formFlow, key string) (occlient.FormField, bool) {
	for _, f := range flow.Form.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return occlient.FormField{}, false
}

func (a *App) whenOK(flow *formFlow, field occlient.FormField) bool {
	for _, cond := range field.When {
		actual, ok := flow.Answers[cond.Key]
		if !ok {
			return false
		}
		equal := valuesEqual(actual, cond.Value)
		switch cond.Op {
		case "eq":
			if !equal {
				return false
			}
		case "neq":
			if equal {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func valuesEqual(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func parseFieldValue(field occlient.FormField, text string) (any, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("answer must not be empty")
	}
	switch field.Type {
	case "string":
		if field.MinLength != nil && len(text) < *field.MinLength {
			return nil, fmt.Errorf("answer must be at least %d characters", *field.MinLength)
		}
		if field.MaxLength != nil && len(text) > *field.MaxLength {
			return nil, fmt.Errorf("answer must be at most %d characters", *field.MaxLength)
		}
		if field.Pattern != "" {
			re, err := regexp.Compile(field.Pattern)
			if err == nil && !re.MatchString(text) {
				return nil, fmt.Errorf("answer does not match the required pattern")
			}
		}
		if len(field.Options) > 0 {
			for _, opt := range field.Options {
				if opt.Value == text || strings.EqualFold(opt.Label, text) {
					return opt.Value, nil
				}
			}
			if !field.Custom {
				return nil, fmt.Errorf("choose one of the provided options")
			}
		}
		return text, nil

	case "number":
		n, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("answer must be a number")
		}
		if !withinRange(n, field.Minimum, field.Maximum) {
			return nil, fmt.Errorf("answer is outside the allowed range")
		}
		return n, nil

	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("answer must be an integer")
		}
		if !withinRange(float64(n), field.Minimum, field.Maximum) {
			return nil, fmt.Errorf("answer is outside the allowed range")
		}
		return n, nil

	case "boolean":
		switch strings.ToLower(text) {
		case "yes", "y", "true", "1", "on":
			return true, nil
		case "no", "n", "false", "0", "off":
			return false, nil
		}
		return nil, fmt.Errorf("answer must be yes or no")

	case "multiselect":
		parts := strings.Split(text, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			matched := false
			for _, opt := range field.Options {
				if opt.Value == part || strings.EqualFold(opt.Label, part) {
					out = append(out, opt.Value)
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("unknown option %q", part)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("choose at least one option")
		}
		return out, nil
	}
	return text, nil
}

func withinRange(v float64, min, max any) bool {
	if min != nil {
		if n, ok := toFloat(min); ok && v < n {
			return false
		}
	}
	if max != nil {
		if n, ok := toFloat(max); ok && v > n {
			return false
		}
	}
	return true
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		n, err := strconv.ParseFloat(x, 64)
		return n, err == nil
	}
	return 0, false
}

func (a *App) submitForm(flow *formFlow) {
	answer := make(map[string]any)
	for _, field := range flow.Form.Fields {
		if field.Hidden {
			if v, ok := flow.Answers[field.Key]; ok {
				answer[field.Key] = v
			}
			continue
		}
		if flow.Skipped[field.Key] {
			if v, ok := flow.Defaults[field.Key]; ok {
				answer[field.Key] = v
			}
			continue
		}
		if v, ok := flow.Answers[field.Key]; ok {
			answer[field.Key] = v
			continue
		}
		if field.Required {
			a.broadcast(fmt.Sprintf("⚠️ Cannot submit %q: %s is still unanswered.",
				flow.Form.Title, field.TitleOrKey()), tgbotapi.InlineKeyboardMarkup{})
			return
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.oc.ReplyForm(ctx, flow.SessionID, flow.Form.ID, answer); err != nil {
		log.Printf("form reply failed for %s: %v", flow.Form.ID, err)
		token := a.addAction(action{Kind: "form_submit", FormID: flow.Form.ID})
		markup := tgbotapi.NewInlineKeyboardMarkup([]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("Retry", callbackData(token)),
		})
		a.broadcast(fmt.Sprintf("❌ Could not submit the answer: %v", err), markup)
		return
	}
	a.broadcast(fmt.Sprintf("✅ Answer sent\n%s\n\n%s",
		blank(flow.Form.Title, "OpenCode question"), summarizeAnswer(answer)), tgbotapi.InlineKeyboardMarkup{})
	a.finishForm(flow.Form.ID)
}

func (a *App) finishForm(formID string) {
	a.mu.Lock()
	delete(a.forms, formID)
	for chatID, pi := range a.pendingInput {
		if pi.FormID == formID {
			delete(a.pendingInput, chatID)
		}
	}
	a.mu.Unlock()
	a.delFormActions(formID)
}

func (a *App) handleFormSettled(formID, status string) {
	a.formMu.Lock()
	defer a.formMu.Unlock()
	a.mu.Lock()
	_, existed := a.forms[formID]
	if existed {
		delete(a.forms, formID)
	}
	a.mu.Unlock()
	if !existed {
		return
	}
	a.delFormActions(formID)
	if status == "answered" {
		a.broadcast("✅ The question was answered in OpenCode.", tgbotapi.InlineKeyboardMarkup{})
	} else {
		a.broadcast("Question cancelled in OpenCode.", tgbotapi.InlineKeyboardMarkup{})
	}
}

func summarizeAnswer(answer map[string]any) string {
	if len(answer) == 0 {
		return "(no fields)"
	}
	keys := make([]string, 0, len(answer))
	for k := range answer {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, formatAnswerValue(answer[k]))
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatAnswerValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "yes"
		}
		return "no"
	case []string:
		return strings.Join(x, ", ")
	default:
		return fmt.Sprintf("%v", x)
	}
}

func blank(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

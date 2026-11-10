// Package occlient is a small typed client for the OpenCode v2 HTTP API.
//
// It covers only the endpoints the daemon needs: event streaming, sessions,
// messages, forms (agent questions), permissions, prompts, and interrupt.
package occlient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxErrorBody = 4096

// APIError is a non-2xx OpenCode response.
type APIError struct {
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("opencode API %d: %s", e.Status, msg)
}

// Client talks to one OpenCode server. The endpoint can be replaced at runtime
// when the background service restarts on a new port.
type Client struct {
	mu       sync.RWMutex
	baseURL  string
	password string
	http     *http.Client
	stream   *http.Client
}

func New(baseURL, password string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		password: password,
		http:     &http.Client{Timeout: timeout},
		// SSE must not have a global timeout: the stream is long-lived and is
		// cancelled through the request context instead.
		stream: &http.Client{},
	}
}

// SetEndpoint replaces the server URL and password.
func (c *Client) SetEndpoint(baseURL, password string) {
	c.mu.Lock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.password = password
	c.mu.Unlock()
}

// Endpoint returns the current URL and password.
func (c *Client) Endpoint() (string, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL, c.password
}

func (c *Client) configured() bool {
	base, _ := c.Endpoint()
	return base != ""
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	base, password := c.Endpoint()
	if base == "" {
		return fmt.Errorf("opencode endpoint is not configured")
	}

	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if password != "" {
		req.SetBasicAuth("opencode", password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &APIError{Status: resp.StatusCode, Body: string(data), Message: decodeErrorMessage(data)}
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func decodeErrorMessage(data []byte) string {
	var e struct {
		Message string `json:"message"`
		Tag     string `json:"_tag"`
	}
	if json.Unmarshal(data, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Tag != "" {
			return e.Tag
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type Location struct {
	Directory string `json:"directory"`
}

type SessionTime struct {
	Created  int64 `json:"created"`
	Updated  int64 `json:"updated"`
	Busy     int64 `json:"busy,omitempty"`
	Idle     int64 `json:"idle,omitempty"`
	Viewed   int64 `json:"viewed,omitempty"`
	Archived int64 `json:"archived,omitempty"`
}

type Session struct {
	ID        string      `json:"id"`
	ParentID  string      `json:"parentID,omitempty"`
	ProjectID string      `json:"projectID,omitempty"`
	Agent     string      `json:"agent,omitempty"`
	Model     ModelRef    `json:"model,omitempty"`
	Title     string      `json:"title,omitempty"`
	Outcome   string      `json:"outcome,omitempty"`
	Cost      float64     `json:"cost,omitempty"`
	Time      SessionTime `json:"time"`
	Location  Location    `json:"location"`
}

type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

func (m ModelRef) Key() string {
	if m.ID == "" || m.ProviderID == "" {
		return ""
	}
	return m.ProviderID + "/" + m.ID
}

type AgentInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Mode        string    `json:"mode"`
	Hidden      bool      `json:"hidden"`
	Model       *ModelRef `json:"model,omitempty"`
}

type ModelInfo struct {
	ID         string `json:"id"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Name       string `json:"name"`
	Type       string `json:"type,omitempty"`
	Family     string `json:"family,omitempty"`
	Status     string `json:"status"`
	Enabled    bool   `json:"enabled"`
}

func (s Session) DisplayTitle() string {
	if strings.TrimSpace(s.Title) != "" {
		return s.Title
	}
	return s.ID
}

type StructuredError struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Status       int    `json:"status,omitempty"`
	RetryAfterMs int64  `json:"retryAfterMs,omitempty"`
}

type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
}

type Message struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Text    string           `json:"text,omitempty"`
	Content []ContentPart    `json:"content,omitempty"`
	Error   *StructuredError `json:"error,omitempty"`
	Time    struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time"`
}

// TextParts returns every non-empty assistant text block in order.
func (m Message) TextParts() []string {
	return contentParts(m, "text")
}

// ReasoningParts returns every non-empty reasoning/thinking block in order.
func (m Message) ReasoningParts() []string {
	return contentParts(m, "reasoning")
}

// FinalText returns the last text part of an assistant message.
func (m Message) FinalText() string {
	return strings.TrimSpace(strings.Join(m.TextParts(), "\n\n"))
}

// ReasoningText returns the full raw reasoning of an assistant message.
func (m Message) ReasoningText() string {
	return strings.TrimSpace(strings.Join(m.ReasoningParts(), "\n\n"))
}

// HasReasoning reports whether the message carries reasoning content.
func (m Message) HasReasoning() bool {
	return len(m.ReasoningParts()) > 0
}

func contentParts(m Message, kind string) []string {
	parts := make([]string, 0, len(m.Content))
	for _, p := range m.Content {
		if p.Type == kind && strings.TrimSpace(p.Text) != "" {
			parts = append(parts, p.Text)
		}
	}
	return parts
}

type FormOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type FormWhen struct {
	Key   string `json:"key"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

type FormField struct {
	Key         string          `json:"key"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Type        string          `json:"type"`
	Required    bool            `json:"required,omitempty"`
	Hidden      bool            `json:"hidden,omitempty"`
	When        []FormWhen      `json:"when,omitempty"`
	Options     []FormOption    `json:"options,omitempty"`
	Custom      bool            `json:"custom,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Placeholder string          `json:"placeholder,omitempty"`
	Format      string          `json:"format,omitempty"`
	Pattern     string          `json:"pattern,omitempty"`
	MinLength   *int            `json:"minLength,omitempty"`
	MaxLength   *int            `json:"maxLength,omitempty"`
	Minimum     any             `json:"minimum,omitempty"`
	Maximum     any             `json:"maximum,omitempty"`
	MinItems    *int            `json:"minItems,omitempty"`
	MaxItems    *int            `json:"maxItems,omitempty"`
}

func (f FormField) TitleOrKey() string {
	if strings.TrimSpace(f.Title) != "" {
		return f.Title
	}
	return f.Key
}

type Form struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Title     string         `json:"title"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Fields    []FormField    `json:"fields"`
}

type Permission struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Action    string         `json:"action"`
	Resources []string       `json:"resources"`
	Save      []string       `json:"save,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Source    any            `json:"source,omitempty"`
	Message   string         `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// Operations
// ---------------------------------------------------------------------------

func (c *Client) Info(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	err := c.do(ctx, http.MethodGet, "/api/info", nil, &out)
	return out, err
}

func (c *Client) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("order", "desc")
	var out struct {
		Data []Session `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c *Client) ActiveSessions(ctx context.Context) (map[string]string, error) {
	var out struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/session/active", nil, &out); err != nil {
		return nil, err
	}
	active := make(map[string]string, len(out.Data))
	for id, v := range out.Data {
		active[id] = v.Type
	}
	return active, nil
}

func (c *Client) GetSession(ctx context.Context, sessionID string) (Session, error) {
	var out struct {
		Data Session `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID), nil, &out)
	return out.Data, err
}

// ListAgents returns the agents registered for a location (usually the
// session's working directory).
func (c *Client) ListAgents(ctx context.Context, directory string) ([]AgentInfo, error) {
	var out struct {
		Data []AgentInfo `json:"data"`
	}
	path := "/api/agent"
	if q := locationQuery(directory); len(q) > 0 {
		path += "?" + q
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ListModels returns the chat models available for a location.
func (c *Client) ListModels(ctx context.Context, directory string) ([]ModelInfo, error) {
	var out struct {
		Data []ModelInfo `json:"data"`
	}
	q := url.Values{}
	q.Set("types", "chat")
	if directory != "" {
		q.Set("location[directory]", directory)
	}
	if err := c.do(ctx, http.MethodGet, "/api/model?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c *Client) SwitchAgent(ctx context.Context, sessionID, agentID string) error {
	path := "/api/session/" + url.PathEscape(sessionID) + "/agent"
	return c.do(ctx, http.MethodPost, path, map[string]any{"agent": agentID}, nil)
}

func (c *Client) SwitchModel(ctx context.Context, sessionID string, model ModelRef) error {
	path := "/api/session/" + url.PathEscape(sessionID) + "/model"
	return c.do(ctx, http.MethodPost, path, map[string]any{"model": model}, nil)
}

type UsageMeter struct {
	Percent  float64 `json:"percent"`
	Limited  bool    `json:"limited"`
	ResetsAt int64   `json:"resetsAt"`
}

type UsageGo struct {
	Rolling UsageMeter `json:"rolling"`
	Weekly  UsageMeter `json:"weekly"`
	Monthly UsageMeter `json:"monthly"`
}

type UsageWorkspace struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	UsageURL string `json:"usageURL"`
}

type UsageBudgetModel struct {
	ProviderID string  `json:"providerID"`
	ModelID    string  `json:"modelID"`
	Name       string  `json:"name"`
	Spent      float64 `json:"spent,omitempty"`
}

type UsageBudget struct {
	ID       string             `json:"id"`
	Limit    float64            `json:"limit"`
	Spent    float64            `json:"spent"`
	Scope    string             `json:"scope"`
	Per      string             `json:"per"`
	Coverage string             `json:"coverage"`
	Models   []UsageBudgetModel `json:"models"`
	Vendors  []string           `json:"vendors"`
	Picked   bool               `json:"picked"`
	Shared   bool               `json:"shared"`
	ResetsAt int64              `json:"resetsAt"`
}

type UsageLimits struct {
	Workspace UsageWorkspace `json:"workspace"`
	Go        *UsageGo       `json:"go,omitempty"`
	Budgets   []UsageBudget  `json:"budgets"`
}

// UsageLimits returns the OpenCode Go allowance and Console budgets for a
// location. A nil result means the location is not signed in.
func (c *Client) UsageLimits(ctx context.Context, directory string) (*UsageLimits, error) {
	var out struct {
		Data *UsageLimits `json:"data"`
	}
	path := "/api/experimental/usage/limits"
	if q := locationQuery(directory); len(q) > 0 {
		path += "?" + q
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func locationQuery(directory string) string {
	q := url.Values{}
	if directory != "" {
		q.Set("location[directory]", directory)
	}
	return q.Encode()
}

func (c *Client) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	var out struct {
		Data []Message `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/message", nil, &out)
	return out.Data, err
}

// GetMessage returns one projected message by ID.
func (c *Client) GetMessage(ctx context.Context, sessionID, messageID string) (Message, error) {
	var out struct {
		Data Message `json:"data"`
	}
	path := "/api/session/" + url.PathEscape(sessionID) + "/message/" + url.PathEscape(messageID)
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Data, err
}

// AssistantMessages returns the newest assistant messages, first one first.
// Asking the server for a descending, assistant-only page avoids downloading
// the whole transcript just for a notification preview.
func (c *Client) AssistantMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprintf("%d", limit))
	q.Set("order", "desc")
	q.Set("type", "assistant")
	var out struct {
		Data []Message `json:"data"`
	}
	path := "/api/session/" + url.PathEscape(sessionID) + "/message?" + q.Encode()
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// LastAssistantMessage returns the newest assistant message that has content.
func (c *Client) LastAssistantMessage(ctx context.Context, sessionID string) (Message, bool, error) {
	messages, err := c.AssistantMessages(ctx, sessionID, 20)
	if err != nil {
		return Message{}, false, err
	}
	for _, m := range messages {
		if len(m.Content) > 0 {
			return m, true, nil
		}
	}
	return Message{}, false, nil
}

// LastAssistantText returns the newest assistant text in a session.
func (c *Client) LastAssistantText(ctx context.Context, sessionID string) (string, error) {
	messages, err := c.AssistantMessages(ctx, sessionID, 20)
	if err != nil {
		return "", err
	}
	for _, m := range messages {
		if text := m.FinalText(); text != "" {
			return text, nil
		}
	}
	return "", nil
}

func (c *Client) ListForms(ctx context.Context, sessionID string) ([]Form, error) {
	var out struct {
		Data []Form `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/form", nil, &out)
	return out.Data, err
}

func (c *Client) ListPermissions(ctx context.Context, sessionID string) ([]Permission, error) {
	var out struct {
		Data []Permission `json:"data"`
	}
	err := c.do(ctx, http.MethodGet, "/api/session/"+url.PathEscape(sessionID)+"/permission", nil, &out)
	return out.Data, err
}

func (c *Client) ReplyForm(ctx context.Context, sessionID, formID string, answer map[string]any) error {
	path := "/api/session/" + url.PathEscape(sessionID) + "/form/" + url.PathEscape(formID) + "/reply"
	return c.do(ctx, http.MethodPost, path, map[string]any{"answer": answer}, nil)
}

func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID, decision, message string) error {
	path := "/api/session/" + url.PathEscape(sessionID) + "/permission/" + url.PathEscape(requestID) + "/reply"
	body := map[string]any{"decision": decision}
	if message != "" {
		body["message"] = message
	}
	return c.do(ctx, http.MethodPost, path, body, nil)
}

func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	path := "/api/session/" + url.PathEscape(sessionID) + "/prompt"
	return c.do(ctx, http.MethodPost, path, map[string]any{
		"text":     text,
		"delivery": "queue",
	}, nil)
}

// Generate asks OpenCode for transient text from the session context. It uses
// the session's configured model and does not add a message to the history.
func (c *Client) Generate(ctx context.Context, sessionID, prompt string) (string, error) {
	path := "/api/session/" + url.PathEscape(sessionID) + "/generate"
	var out struct {
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"prompt": prompt}, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Data.Text), nil
}

func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	path := "/api/session/" + url.PathEscape(sessionID) + "/interrupt"
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

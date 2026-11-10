// Package settings stores notification and security preferences.
//
// Notifications are disabled by default. The file is watched/reloaded by the
// daemon and can be edited by the OpenCode plugin through the local HTTP API.
// Security settings are intentionally separate: they can only be changed from
// the local plugin API or by editing this file, never from Telegram itself.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"opencode-telegram/internal/safeio"
)

// Notifications controls which OpenCode events are delivered to Telegram.
type Notifications struct {
	Enabled              bool    `json:"enabled"`
	SessionDone          bool    `json:"session_done"`
	SessionFailed        bool    `json:"session_failed"`
	SessionInterrupted   bool    `json:"session_interrupted"`
	QuestionAsked        bool    `json:"question_asked"`
	PermissionAsked      bool    `json:"permission_asked"`
	Retry                bool    `json:"retry"`
	IncludeChildSessions bool    `json:"include_child_sessions"`
	IncludeLastMessage   bool    `json:"include_last_message"`
	IncludeDirectory     bool    `json:"include_directory"`
	NotifyChats          []int64 `json:"notify_chats"`

	// LongMessageMode decides what happens when a notification would exceed
	// Telegram's 4096-byte limit. "split" sends one message per assistant text
	// block; "summary" sends a short preview plus an on-demand summary button.
	LongMessageMode string `json:"long_message_mode"`
	// ShowRawThinking adds a "Show raw thinking" button to notifications that
	// carry reasoning, which sends the raw reasoning text.
	ShowRawThinking bool `json:"show_raw_thinking"`
	// ResumeThinkingMode controls the "Resume thinking" button. "summary"
	// (default) asks OpenCode for a summary of the reasoning; "continue" asks
	// OpenCode to pick the run back up instead.
	ResumeThinkingMode string `json:"resume_thinking_mode"`
}

// Security controls actions that can inject arbitrary content into OpenCode.
// Both default to false so a compromised Telegram account cannot type new
// prompts or free-form answers unless the operator explicitly enables it.
type Security struct {
	AllowCustomFormAnswers bool `json:"allow_custom_form_answers"`
	AllowNewMessages       bool `json:"allow_new_messages"`
	// AllowResumeThinking lets Telegram ask OpenCode to resume a run or
	// summarize its thinking. It stays off until the operator opts in.
	AllowResumeThinking bool `json:"allow_resume_thinking"`
}

type Settings struct {
	Notifications Notifications `json:"notifications"`
	Security      Security      `json:"security"`
	UpdatedAt     int64         `json:"updated_at,omitempty"`
}

// Defaults returns the fail-closed defaults: no notifications delivered, and
// no Telegram-originated prompts or free-form answers.
func Defaults() Settings {
	return Settings{
		Notifications: Notifications{
			Enabled:              false,
			SessionDone:          true,
			SessionFailed:        true,
			SessionInterrupted:   true,
			QuestionAsked:        true,
			PermissionAsked:      true,
			Retry:                false,
			IncludeChildSessions: false,
			IncludeLastMessage:   true,
			IncludeDirectory:     true,
			NotifyChats:          []int64{},
			LongMessageMode:      "split",
			ShowRawThinking:      false,
			ResumeThinkingMode:   "summary",
		},
		Security: Security{
			AllowCustomFormAnswers: false,
			AllowNewMessages:       false,
			AllowResumeThinking:    false,
		},
	}
}

// Patch is a partial settings update. Omitted fields are left unchanged.
type Patch struct {
	Notifications *NotificationsPatch `json:"notifications,omitempty"`
	Security      *SecurityPatch      `json:"security,omitempty"`
}

type NotificationsPatch struct {
	Enabled              *bool    `json:"enabled,omitempty"`
	SessionDone          *bool    `json:"session_done,omitempty"`
	SessionFailed        *bool    `json:"session_failed,omitempty"`
	SessionInterrupted   *bool    `json:"session_interrupted,omitempty"`
	QuestionAsked        *bool    `json:"question_asked,omitempty"`
	PermissionAsked      *bool    `json:"permission_asked,omitempty"`
	Retry                *bool    `json:"retry,omitempty"`
	IncludeChildSessions *bool    `json:"include_child_sessions,omitempty"`
	IncludeLastMessage   *bool    `json:"include_last_message,omitempty"`
	IncludeDirectory     *bool    `json:"include_directory,omitempty"`
	NotifyChats          *[]int64 `json:"notify_chats,omitempty"`
	LongMessageMode      *string  `json:"long_message_mode,omitempty"`
	ShowRawThinking      *bool    `json:"show_raw_thinking,omitempty"`
	ResumeThinkingMode   *string  `json:"resume_thinking_mode,omitempty"`
}

type SecurityPatch struct {
	AllowCustomFormAnswers *bool `json:"allow_custom_form_answers,omitempty"`
	AllowNewMessages       *bool `json:"allow_new_messages,omitempty"`
	AllowResumeThinking    *bool `json:"allow_resume_thinking,omitempty"`
}

// Store is a concurrency-safe settings file.
type Store struct {
	mu   sync.RWMutex
	path string
	cur  Settings
}

// Open loads the settings file, creating it with defaults when missing.
func Open(path string) (*Store, error) {
	s := &Store{path: path, cur: Defaults()}
	if err := s.Load(); err != nil {
		if !os.IsNotExist(err) {
			return s, err
		}
		if err := s.saveLocked(); err != nil {
			return s, fmt.Errorf("create settings: %w", err)
		}
	}
	return s, nil
}

// Load re-reads the file. On any error the previous settings are kept.
func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var next Settings
	if err := json.Unmarshal(data, &next); err != nil {
		return fmt.Errorf("settings %s is corrupt: %w", s.path, err)
	}
	normalize(&next)
	if err := validate(next); err != nil {
		return fmt.Errorf("settings %s invalid: %w", s.path, err)
	}

	s.mu.Lock()
	s.cur = next
	s.mu.Unlock()
	return nil
}

// Get returns a copy of the current settings.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cur)
}

// Patch applies a partial update, validates it, and persists it.
func (s *Store) Patch(p Patch) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := clone(s.cur)
	if p.Notifications != nil {
		applyNotifications(&next.Notifications, p.Notifications)
	}
	if p.Security != nil {
		applySecurity(&next.Security, p.Security)
	}
	next.UpdatedAt = time.Now().UnixMilli()
	if err := validate(next); err != nil {
		return clone(s.cur), err
	}
	if err := write(s.path, next); err != nil {
		return clone(s.cur), err
	}
	s.cur = next
	return clone(next), nil
}

// Replace writes a complete settings value. Used by tests and future CLIs.
func (s *Store) Replace(next Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalize(&next)
	next.UpdatedAt = time.Now().UnixMilli()
	if err := validate(next); err != nil {
		return clone(s.cur), err
	}
	if err := write(s.path, next); err != nil {
		return clone(s.cur), err
	}
	s.cur = next
	return clone(next), nil
}

func (s *Store) saveLocked() error {
	return write(s.path, s.cur)
}

func applyNotifications(dst *Notifications, p *NotificationsPatch) {
	if p.Enabled != nil {
		dst.Enabled = *p.Enabled
	}
	if p.SessionDone != nil {
		dst.SessionDone = *p.SessionDone
	}
	if p.SessionFailed != nil {
		dst.SessionFailed = *p.SessionFailed
	}
	if p.SessionInterrupted != nil {
		dst.SessionInterrupted = *p.SessionInterrupted
	}
	if p.QuestionAsked != nil {
		dst.QuestionAsked = *p.QuestionAsked
	}
	if p.PermissionAsked != nil {
		dst.PermissionAsked = *p.PermissionAsked
	}
	if p.Retry != nil {
		dst.Retry = *p.Retry
	}
	if p.IncludeChildSessions != nil {
		dst.IncludeChildSessions = *p.IncludeChildSessions
	}
	if p.IncludeLastMessage != nil {
		dst.IncludeLastMessage = *p.IncludeLastMessage
	}
	if p.IncludeDirectory != nil {
		dst.IncludeDirectory = *p.IncludeDirectory
	}
	if p.NotifyChats != nil {
		dst.NotifyChats = append([]int64(nil), (*p.NotifyChats)...)
	}
	if p.LongMessageMode != nil {
		dst.LongMessageMode = *p.LongMessageMode
	}
	if p.ShowRawThinking != nil {
		dst.ShowRawThinking = *p.ShowRawThinking
	}
	if p.ResumeThinkingMode != nil {
		dst.ResumeThinkingMode = *p.ResumeThinkingMode
	}
}

func applySecurity(dst *Security, p *SecurityPatch) {
	if p.AllowCustomFormAnswers != nil {
		dst.AllowCustomFormAnswers = *p.AllowCustomFormAnswers
	}
	if p.AllowNewMessages != nil {
		dst.AllowNewMessages = *p.AllowNewMessages
	}
	if p.AllowResumeThinking != nil {
		dst.AllowResumeThinking = *p.AllowResumeThinking
	}
}

func normalize(s *Settings) {
	if s.Notifications.NotifyChats == nil {
		s.Notifications.NotifyChats = []int64{}
	}
	s.Notifications.LongMessageMode = strings.ToLower(strings.TrimSpace(s.Notifications.LongMessageMode))
	if s.Notifications.LongMessageMode == "" {
		s.Notifications.LongMessageMode = "split"
	}
	s.Notifications.ResumeThinkingMode = strings.ToLower(strings.TrimSpace(s.Notifications.ResumeThinkingMode))
	if s.Notifications.ResumeThinkingMode == "" {
		s.Notifications.ResumeThinkingMode = "summary"
	}
}

func validate(s Settings) error {
	switch s.Notifications.LongMessageMode {
	case "split", "summary":
	default:
		return fmt.Errorf("long_message_mode must be \"split\" or \"summary\"")
	}
	switch s.Notifications.ResumeThinkingMode {
	case "continue", "summary":
	default:
		return fmt.Errorf("resume_thinking_mode must be \"continue\" or \"summary\"")
	}
	if len(s.Notifications.NotifyChats) > 50 {
		return fmt.Errorf("notify_chats may contain at most 50 chats")
	}
	seen := make(map[int64]struct{}, len(s.Notifications.NotifyChats))
	for _, id := range s.Notifications.NotifyChats {
		if id == 0 {
			return fmt.Errorf("notify_chats contains chat id 0")
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("notify_chats contains duplicate chat id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func write(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := safeio.WriteFileAtomic(path, data, safeio.DefaultPerm); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

func clone(s Settings) Settings {
	s.Notifications.NotifyChats = append([]int64(nil), s.Notifications.NotifyChats...)
	return s
}

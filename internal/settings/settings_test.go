package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAreOff(t *testing.T) {
	d := Defaults()
	if d.Notifications.Enabled {
		t.Fatal("notifications must be off by default")
	}
	if d.Security.AllowCustomFormAnswers || d.Security.AllowNewMessages || d.Security.AllowResumeThinking {
		t.Fatal("security overrides must be off by default")
	}
	if !d.Notifications.SessionDone || !d.Notifications.QuestionAsked {
		t.Fatal("individual notification types should default to on")
	}
	if d.Notifications.LongMessageMode != "split" {
		t.Fatalf("long_message_mode = %q", d.Notifications.LongMessageMode)
	}
	if d.Notifications.ResumeThinkingMode != "continue" {
		t.Fatalf("resume_thinking_mode = %q", d.Notifications.ResumeThinkingMode)
	}
}

func TestPatchLongMessageAndThinking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	mode := "summary"
	thinkMode := "summary"
	show := true
	allow := true
	next, err := s.Patch(Patch{
		Notifications: &NotificationsPatch{
			LongMessageMode:    &mode,
			ResumeThinkingMode: &thinkMode,
			ShowRawThinking:    &show,
		},
		Security: &SecurityPatch{AllowResumeThinking: &allow},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Notifications.LongMessageMode != "summary" ||
		next.Notifications.ResumeThinkingMode != "summary" ||
		!next.Notifications.ShowRawThinking || !next.Security.AllowResumeThinking {
		t.Fatalf("patch not applied: %+v", next)
	}
}

func TestRejectUnknownLongMessageMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	bad := "sometimes"
	if _, err := s.Patch(Patch{Notifications: &NotificationsPatch{LongMessageMode: &bad}}); err == nil {
		t.Fatal("unknown long message mode must be rejected")
	}
	if _, err := s.Patch(Patch{Notifications: &NotificationsPatch{ResumeThinkingMode: &bad}}); err == nil {
		t.Fatal("unknown resume thinking mode must be rejected")
	}
}

func TestOpenCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().Notifications.Enabled {
		t.Fatal("created file must start with notifications off")
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %o, want 600", info.Mode().Perm())
	}
}

func TestPatchPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	off := false
	next, err := s.Patch(Patch{
		Notifications: &NotificationsPatch{
			Enabled:     &on,
			Retry:       &on,
			NotifyChats: &[]int64{123},
		},
		Security: &SecurityPatch{AllowNewMessages: &on, AllowCustomFormAnswers: &off},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !next.Notifications.Enabled || !next.Notifications.Retry || !next.Security.AllowNewMessages {
		t.Fatal("patch was not applied")
	}
	if next.Security.AllowCustomFormAnswers {
		t.Fatal("unset security field must stay false")
	}

	reopened, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Get().Notifications.Enabled || len(reopened.Get().Notifications.NotifyChats) != 1 {
		t.Fatal("settings did not persist")
	}
}

func TestRejectZeroChatID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	chats := []int64{0}
	if _, err := s.Patch(Patch{Notifications: &NotificationsPatch{NotifyChats: &chats}}); err == nil {
		t.Fatal("chat id 0 must be rejected")
	}
}

func TestCorruptReloadKeepsPrevious(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := s.Patch(Patch{Notifications: &NotificationsPatch{Enabled: &on}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err == nil {
		t.Fatal("corrupt reload must fail")
	}
	if !s.Get().Notifications.Enabled {
		t.Fatal("previous settings must stay in effect")
	}
}

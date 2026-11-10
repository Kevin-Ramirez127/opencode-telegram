package occlient

import (
	"testing"
)

func TestParseSSEFrame(t *testing.T) {
	ev, err := ParseSSEFrame([]byte(`{"id":"evt_1","created":123,"type":"session.idle","location":{"directory":"/tmp"},"data":{"sessionID":"ses_1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "session.idle" || ev.Location.Directory != "/tmp" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	var data struct {
		SessionID string `json:"sessionID"`
	}
	if err := ev.DecodeData(&data); err != nil {
		t.Fatal(err)
	}
	if data.SessionID != "ses_1" {
		t.Fatalf("sessionID = %q", data.SessionID)
	}
}

func TestFinalTextPicksTextParts(t *testing.T) {
	m := Message{Content: []ContentPart{
		{Type: "reasoning", Text: "thinking"},
		{Type: "text", Text: "hello"},
		{Type: "tool", Name: "bash"},
		{Type: "text", Text: "world"},
	}}
	if got := m.FinalText(); got != "hello\n\nworld" {
		t.Fatalf("FinalText = %q", got)
	}
}

func TestSessionDisplayTitle(t *testing.T) {
	if got := (Session{ID: "ses_1"}).DisplayTitle(); got != "ses_1" {
		t.Fatalf("fallback title = %q", got)
	}
	if got := (Session{ID: "ses_1", Title: "Build"}).DisplayTitle(); got != "Build" {
		t.Fatalf("title = %q", got)
	}
}

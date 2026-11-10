package occlient

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLive is an opt-in smoke test against the local OpenCode service.
//
//	OPENCODE_LIVE_TEST=1 go test ./internal/occlient -run TestLive -v
func TestLive(t *testing.T) {
	if os.Getenv("OPENCODE_LIVE_TEST") == "" {
		t.Skip("set OPENCODE_LIVE_TEST=1 to run against the local service")
	}
	svc, ok := DiscoverService()
	if !ok {
		t.Skip("no local OpenCode service registration found")
	}
	c := New(svc.URL, svc.Password, 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	t.Logf("OpenCode %v at %s", info["version"], svc.URL)

	sessions, err := c.ListSessions(ctx, 3)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	t.Logf("listed %d session(s)", len(sessions))
	if len(sessions) > 0 {
		text, err := c.LastAssistantText(ctx, sessions[0].ID)
		if err != nil {
			t.Fatalf("last assistant text: %v", err)
		}
		t.Logf("last assistant text: %.80q", text)
	}

	streamCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	events := make(chan Event, 8)
	go func() { _ = c.StreamEvents(streamCtx, EventTypes, events) }()
	select {
	case ev := <-events:
		if ev.Type == "" {
			t.Fatal("empty event type")
		}
		t.Logf("first event: %s", ev.Type)
	case <-streamCtx.Done():
		t.Fatal("no event received before timeout")
	}
}

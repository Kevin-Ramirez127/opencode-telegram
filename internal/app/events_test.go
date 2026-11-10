package app

import (
	"testing"

	"opencode-telegram/internal/occlient"
)

func TestEventSessionState(t *testing.T) {
	a, srv := testApp(t)
	defer srv.Close()

	a.handleEvent(occlient.Event{
		Type: "session.execution.started",
		Data: []byte(`{"sessionID":"ses_x"}`),
	})
	if !a.sessionSnapshot("ses_x").Busy {
		t.Fatal("execution.started must mark the session busy")
	}

	a.handleEvent(occlient.Event{
		Type: "session.execution.succeeded",
		Data: []byte(`{"sessionID":"ses_x"}`),
	})
	st := a.sessionSnapshot("ses_x")
	if st.Busy {
		t.Fatal("execution.succeeded must clear busy")
	}
	if st.Outcome != "succeeded" {
		t.Fatalf("outcome = %q", st.Outcome)
	}
}

func TestDisabledNotificationsDoNotCreateFlows(t *testing.T) {
	a, srv := testApp(t)
	defer srv.Close()

	a.handleEvent(occlient.Event{
		Type: "form.created",
		Data: []byte(`{"form":{"id":"frm_1","sessionID":"ses_x","title":"Pick","fields":[]}}`),
	})
	if a.formFlow("frm_1") != nil {
		t.Fatal("forms must not be tracked while notifications are disabled")
	}

	a.handleEvent(occlient.Event{
		Type: "permission.asked",
		Data: []byte(`{"id":"per_1","sessionID":"ses_x","action":"bash","resources":["ls"]}`),
	})
	if n, _ := a.pendingPermissionCount(t.Context()); n != 0 {
		t.Fatalf("pending permissions = %d", n)
	}
}

func TestSessionDeletedClearsBinding(t *testing.T) {
	a, srv := testApp(t)
	defer srv.Close()
	a.setBinding(123, "ses_x")
	a.handleEvent(occlient.Event{
		Type: "session.deleted",
		Data: []byte(`{"sessionID":"ses_x"}`),
	})
	if a.binding(123) != "" {
		t.Fatal("binding must be cleared when its session is deleted")
	}
}

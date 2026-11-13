package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"opencode-telegram/internal/occlient"
)

// eventLoop keeps the SSE subscription alive. The stream is live-only and
// deliberately fails on slow consumers, so reconnecting is part of normal
// operation.
func (a *App) eventLoop(ctx context.Context) {
	go a.eventWorker(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		if !a.ensureEndpoint(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = time.Second

		log.Printf("subscribing to OpenCode events at %s", a.endpointLabel())
		err := a.oc.StreamEvents(ctx, occlient.EventTypes, a.updates)
		if ctx.Err() != nil {
			return
		}
		a.setOCStatus(false, "", err)
		if err == nil {
			err = fmt.Errorf("event stream ended")
		}
		log.Printf("OpenCode event stream ended: %v (reconnecting in %s)", err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = nextBackoff(backoff)
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// ensureEndpoint discovers the background service when needed and checks it
// with /api/info. It returns false when OpenCode is not reachable yet.
func (a *App) ensureEndpoint(ctx context.Context) bool {
	base, password := a.oc.Endpoint()
	if !a.explicitURL || !a.explicitPassword {
		if svc, ok := occlient.DiscoverService(); ok {
			nextBase, nextPassword := base, password
			if !a.explicitURL && svc.URL != "" {
				nextBase = svc.URL
			}
			if !a.explicitPassword && svc.Password != "" {
				nextPassword = svc.Password
			}
			if nextBase != base || nextPassword != password {
				a.oc.SetEndpoint(nextBase, nextPassword)
				base, password = a.oc.Endpoint()
				log.Printf("discovered OpenCode service at %s (version %s)", base, svc.Version)
			}
		}
	}
	if base == "" {
		a.setOCStatus(false, "", fmt.Errorf("OpenCode service not found (start OpenCode or set OPENCODE_URL)"))
		return false
	}

	ictx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	info, err := a.oc.Info(ictx)
	if err != nil {
		a.setOCStatus(false, "", err)
		return false
	}
	version, _ := info["version"].(string)
	a.setOCStatus(true, version, nil)
	log.Printf("connected to OpenCode %s at %s", version, base)
	return true
}

func (a *App) eventWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-a.updates:
			a.noteEvent()
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("panic while handling event %s: %v", ev.Type, r)
					}
				}()
				a.handleEvent(ev)
			}()
		}
	}
}

func (a *App) handleEvent(ev occlient.Event) {
	switch ev.Type {
	case "server.connected":
		a.setOCStatus(true, "", nil)
		go a.syncPending(context.Background())

	case "session.created", "session.renamed", "session.moved":
		var data struct {
			SessionID string            `json:"sessionID"`
			Title     string            `json:"title"`
			Location  occlient.Location `json:"location"`
		}
		if ev.DecodeData(&data) == nil && data.SessionID != "" {
			a.mu.Lock()
			st := a.sessions[data.SessionID]
			if st == nil {
				st = &sessionState{ID: data.SessionID}
				a.sessions[data.SessionID] = st
			}
			if data.Title != "" {
				st.Title = data.Title
			}
			if data.Location.Directory != "" {
				st.Directory = data.Location.Directory
			}
			st.LastSeen = time.Now()
			a.mu.Unlock()
		}

	case "session.deleted":
		var data struct {
			SessionID string `json:"sessionID"`
			ID        string `json:"id"`
		}
		if ev.DecodeData(&data) == nil {
			id := data.SessionID
			if id == "" {
				id = data.ID
			}
			a.stopLiveForSession(id)
			a.forgetSession(id)
		}

	case "session.text.started", "session.text.delta", "session.text.ended",
		"session.tool.input.started", "session.tool.success", "session.tool.failed":
		a.handleLiveEvent(ev)

	case "session.execution.started":
		id := sessionIDFrom(ev)
		if id != "" {
			a.setBusy(id, true)
		}

	case "session.execution.succeeded":
		id := sessionIDFrom(ev)
		if id != "" {
			a.stopLiveForSession(id)
			a.notifySession(id, "succeeded", "")
		}

	case "session.execution.failed":
		var data struct {
			SessionID string                   `json:"sessionID"`
			Error     occlient.StructuredError `json:"error"`
		}
		_ = ev.DecodeData(&data)
		if data.SessionID != "" {
			a.stopLiveForSession(data.SessionID)
			msg := data.Error.Message
			if msg == "" {
				msg = data.Error.Type
			}
			a.notifySession(data.SessionID, "failed", msg)
		}

	case "session.execution.interrupted":
		var data struct {
			SessionID string `json:"sessionID"`
			Reason    string `json:"reason"`
		}
		_ = ev.DecodeData(&data)
		if data.SessionID != "" {
			a.stopLiveForSession(data.SessionID)
			a.notifySession(data.SessionID, "interrupted", data.Reason)
		}

	case "session.status":
		var data struct {
			SessionID string `json:"sessionID"`
			Status    struct {
				Type    string `json:"type"`
				Attempt int    `json:"attempt"`
				Message string `json:"message"`
			} `json:"status"`
		}
		if ev.DecodeData(&data) != nil || data.SessionID == "" {
			return
		}
		switch data.Status.Type {
		case "busy":
			a.setBusy(data.SessionID, true)
		case "idle":
			a.notifySession(data.SessionID, "idle", "")
		case "retry":
			a.notifyRetry(data.SessionID, data.Status.Attempt, data.Status.Message)
		}

	case "session.idle":
		if id := sessionIDFrom(ev); id != "" {
			// A question or permission request also leaves the session idle;
			// keep streaming because the run resumes after the answer.
			if !a.hasPendingHumanInput(id) {
				a.stopLiveForSession(id)
			}
			a.notifySession(id, "idle", "")
		}

	case "form.created":
		var data struct {
			Form occlient.Form `json:"form"`
		}
		if ev.DecodeData(&data) == nil && data.Form.ID != "" {
			a.handleFormCreated(data.Form)
		}

	case "form.replied":
		var data struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		if ev.DecodeData(&data) == nil && data.ID != "" {
			a.handleFormSettled(data.ID, "answered")
		}

	case "form.cancelled":
		var data struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		if ev.DecodeData(&data) == nil && data.ID != "" {
			a.handleFormSettled(data.ID, "cancelled")
		}

	case "permission.asked":
		var p occlient.Permission
		if ev.DecodeData(&p) == nil && p.ID != "" {
			a.handlePermissionAsked(p)
		}

	case "permission.replied":
		var data struct {
			SessionID string `json:"sessionID"`
			RequestID string `json:"requestID"`
			Reply     string `json:"reply"`
		}
		if ev.DecodeData(&data) == nil && data.RequestID != "" {
			a.handlePermissionReplied(data.RequestID)
		}
	}
}

func sessionIDFrom(ev occlient.Event) string {
	var data struct {
		SessionID string `json:"sessionID"`
	}
	if err := ev.DecodeData(&data); err != nil {
		return ""
	}
	return data.SessionID
}

func (a *App) forgetSession(id string) {
	if id == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, id)
	for chatID, bound := range a.bindings {
		if bound == id {
			delete(a.bindings, chatID)
		}
	}
	a.mu.Unlock()
	a.clearNotified("session:" + id)
}

// syncPending reconciles live state after a reconnect. It is deliberately
// bounded: the 50 newest sessions, and only pending forms/permissions.
func (a *App) syncPending(ctx context.Context) {
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	sessions, err := a.recentSessions(ctx, 50)
	if err != nil {
		log.Printf("sync: could not list sessions: %v", err)
		return
	}
	active, err := a.oc.ActiveSessions(ctx)
	if err != nil {
		log.Printf("sync: could not list active sessions: %v", err)
	} else {
		for id := range active {
			a.setBusy(id, true)
		}
	}

	seenForms := 0
	seenPerms := 0
	for _, s := range sessions {
		forms, err := a.oc.ListForms(ctx, s.ID)
		if err == nil {
			for _, f := range forms {
				seenForms++
				a.handleFormCreated(f)
			}
		}
		perms, err := a.oc.ListPermissions(ctx, s.ID)
		if err == nil {
			for _, p := range perms {
				seenPerms++
				a.handlePermissionAsked(p)
			}
		}
	}
	if seenForms > 0 || seenPerms > 0 {
		log.Printf("sync: recovered %d pending question(s), %d pending permission(s)", seenForms, seenPerms)
	}
}

// parseJSON is a tiny helper used by forms for default values.
func parseJSON(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return fmt.Errorf("empty value")
	}
	return json.Unmarshal(raw, dst)
}

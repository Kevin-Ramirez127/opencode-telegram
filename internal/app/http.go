package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"opencode-telegram/internal/settings"
)

const maxJSONBody = 64 * 1024

// serveHTTP runs the loopback settings API used by the OpenCode plugin. It is
// not the same thing as OpenCode's own HTTP API.
func (a *App) serveHTTP(ctx context.Context) {
	mux := a.routes()

	srv := &http.Server{
		Addr:              a.cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("plugin settings API listening on http://%s", a.cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("plugin settings API stopped: %v", err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.withAuth(a.handleHealth))
	mux.HandleFunc("/api/plugin/status", a.withAuth(a.handleHealth))
	mux.HandleFunc("/api/plugin/settings", a.withAuth(a.handleSettings))
	mux.HandleFunc("/api/plugin/test-notification", a.withAuth(a.handleTestNotification))
	mux.HandleFunc("/api/plugin/sessions", a.withAuth(a.handleSessions))
	return mux
}

func (a *App) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authOK(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="opencode-telegram"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next(w, r)
	}
}

func (a *App) authOK(r *http.Request) bool {
	if a.cfg.PluginToken == "" {
		return true
	}
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	return header == "Bearer "+a.cfg.PluginToken
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, a.snapshot())
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"settings": a.settings.Get(),
			"status":   a.snapshot(),
		})
	case http.MethodPatch, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read body"})
			return
		}
		var patch settings.Patch
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid settings patch: " + err.Error()})
			return
		}
		if err := a.validatePatch(patch); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		next, err := a.settings.Patch(patch)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		log.Printf("plugin updated settings: notifications=%v custom_answers=%v new_messages=%v model_switching=%v",
			next.Notifications.Enabled, next.Security.AllowCustomFormAnswers, next.Security.AllowNewMessages,
			next.Security.AllowModelSwitching)
		writeJSON(w, http.StatusOK, map[string]any{
			"settings": next,
			"status":   a.snapshot(),
		})
	default:
		w.Header().Set("Allow", "GET, PATCH, POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (a *App) validatePatch(p settings.Patch) error {
	if p.Notifications != nil && p.Notifications.NotifyChats != nil {
		for _, id := range *p.Notifications.NotifyChats {
			if !a.whitelist.ChatAllowed(id) {
				return fmt.Errorf("chat %d is not in the allowlist", id)
			}
		}
	}
	return nil
}

func (a *App) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, http.MethodPost) {
		return
	}
	chats := a.notifyChats()
	a.sendTestNotification(chats)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sent": len(chats)})
}

func (a *App) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sessions, err := a.recentSessions(ctx, 20)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	type row struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Updated   int64  `json:"updated"`
		Outcome   string `json:"outcome,omitempty"`
		Running   bool   `json:"running"`
	}
	out := make([]row, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, row{
			ID:        s.ID,
			Title:     s.DisplayTitle(),
			Directory: s.Location.Directory,
			Updated:   s.Time.Updated,
			Outcome:   s.Outcome,
			Running:   a.sessionSnapshot(s.ID).Busy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func methodAllowed(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/browser"
)

// BrowserLiveHandler provides HTTP endpoints for browser live view (screencast + input relay).
type BrowserLiveHandler struct {
	sessions store.ScreencastSessionStore
	manager  atomic.Pointer[browser.Manager]
	logger   *slog.Logger
	upgrader websocket.Upgrader
}

// NewBrowserLiveHandler creates a BrowserLiveHandler.
func NewBrowserLiveHandler(ss store.ScreencastSessionStore, mgr *browser.Manager, l *slog.Logger) *BrowserLiveHandler {
	if l == nil {
		l = slog.Default()
	}
	h := &BrowserLiveHandler{
		sessions: ss,
		logger:   l,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 65536,
			CheckOrigin:     checkSameOrigin,
		},
	}
	h.manager.Store(mgr)
	return h
}

// SetManager replaces the underlying browser Manager (used for config hot-reload).
func (h *BrowserLiveHandler) SetManager(mgr *browser.Manager) { h.manager.Store(mgr) }

// getManager returns the current browser Manager (safe for concurrent access).
func (h *BrowserLiveHandler) getManager() *browser.Manager { return h.manager.Load() }

// RegisterRoutes registers the live view HTTP routes.
func (h *BrowserLiveHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /browser/status", requireAuth("", h.handleStatus))
	mux.HandleFunc("GET /browser/tabs", requireAuth("", h.handleTabs))
	mux.HandleFunc("POST /browser/start", requireAuth("", h.handleStartBrowser))
	mux.HandleFunc("POST /browser/stop", requireAuth("", h.handleStopBrowser))
	mux.HandleFunc("POST /browser/close-tab", requireAuth("", h.handleCloseTab))
	// Authenticated screencast — direct WS for chat panel.
	// Auth via Sec-WebSocket-Protocol header (WebSocket API cannot send custom headers).
	mux.HandleFunc("GET /browser/screencast/{targetId}", h.handleScreencastWS)
	// Token-based endpoints — for sharing with unauthenticated viewers
	mux.HandleFunc("POST /browser/live", requireAuth("", h.handleCreate))
	mux.HandleFunc("GET /browser/live/{token}", h.handleView)
	mux.HandleFunc("GET /browser/live/{token}/info", h.handleInfo)
	mux.HandleFunc("GET /browser/live/{token}/ws", h.handleWS)
}

// handleStatus returns the current browser engine status.
func (h *BrowserLiveHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.getManager().Status())
}

// handleTabs returns a list of open browser tabs, optionally filtered by query params.
func (h *BrowserLiveHandler) handleTabs(w http.ResponseWriter, r *http.Request) {
	tabs, err := h.getManager().ListTabs(browserCtx(r))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tabs": []any{}, "error": err.Error()})
		return
	}
	// Filter by session key (strict) or agent key.
	sessionKey := r.URL.Query().Get("sessionKey")
	agentKey := r.URL.Query().Get("agentKey")
	if sessionKey != "" {
		var bySession []browser.TabInfo
		for _, t := range tabs {
			if t.SessionKey == sessionKey {
				bySession = append(bySession, t)
			}
		}
		tabs = bySession
	} else if agentKey != "" {
		filtered := make([]browser.TabInfo, 0, len(tabs))
		for _, t := range tabs {
			if t.AgentKey == agentKey {
				filtered = append(filtered, t)
			}
		}
		tabs = filtered
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tabs": tabs})
}

// handleStartBrowser starts the browser engine.
func (h *BrowserLiveHandler) handleStartBrowser(w http.ResponseWriter, r *http.Request) {
	if err := h.getManager().Start(browserCtx(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleStopBrowser stops the browser engine.
func (h *BrowserLiveHandler) handleStopBrowser(w http.ResponseWriter, r *http.Request) {
	if err := h.getManager().Stop(browserCtx(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleCloseTab closes a specific browser tab by targetId.
func (h *BrowserLiveHandler) handleCloseTab(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetID string `json:"targetId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TargetID == "" {
		http.Error(w, "targetId is required", http.StatusBadRequest)
		return
	}
	if err := h.getManager().CloseTab(browserCtx(r), req.TargetID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleScreencastWS handles authenticated screencast WS for the chat panel.
// Auth via Sec-WebSocket-Protocol header: client sends bearer token as a subprotocol,
// server echoes it back to complete the handshake. Safe (not in URL, not logged).
func (h *BrowserLiveHandler) handleScreencastWS(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("targetId")
	if targetID == "" {
		http.Error(w, "targetId required", http.StatusBadRequest)
		return
	}

	// Extract bearer token from Sec-WebSocket-Protocol header (set by client as subprotocol).
	bearer := ""
	for _, p := range websocket.Subprotocols(r) {
		bearer = p
		break
	}

	// Validate auth before upgrading — reject with 401 if invalid.
	if !tokenMatch(bearer, pkgGatewayToken) {
		if _, role := ResolveAPIKey(r.Context(), bearer); role == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Upgrade with the token echoed back as selected subprotocol (required by WS spec).
	//
	// Security note: echoing the bearer token in Sec-WebSocket-Protocol is architecturally
	// necessary. Browser WebSocket APIs cannot send custom headers (e.g. Authorization),
	// so the token is passed as a subprotocol by the client. The server must echo the chosen
	// subprotocol back or the browser rejects the handshake. The token is already known to
	// the client (they sent it), so echoing it does not introduce new exposure. Reverse
	// proxies may log this header — this is acceptable because the token appears in access
	// logs only for this specific upgrade request, and the client itself supplied it.
	// Changing this mechanism would break the existing auth flow for the chat panel.
	upgrader := h.upgrader
	upgrader.Subprotocols = []string{bearer}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("screencast ws upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	page := h.getManager().PageByTargetID(targetID)
	if page == nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"target page not found"}`))
		return
	}

	h.logger.Info("screencast connected", "target", targetID)
	h.runScreencastLoop(conn, page, "takeover", targetID)
	h.logger.Info("screencast disconnected", "target", targetID)
}

// handleWS handles the token-based WebSocket connection for streaming frames and input.
func (h *BrowserLiveHandler) handleWS(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	sess, err := h.sessions.GetByToken(r.Context(), token)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		http.Error(w, "session expired", http.StatusGone)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Warn("websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	page := h.getManager().PageByTargetID(sess.TargetID)
	if page == nil {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"target page not found"}`))
		return
	}

	h.logger.Info("live view connected", "token", token[:8], "mode", sess.Mode, "target", sess.TargetID)
	h.runScreencastLoop(conn, page, sess.Mode, sess.TargetID)
	h.logger.Info("live view disconnected", "token", token[:8])
}

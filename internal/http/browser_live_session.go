package http

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/browser"
)

// browserCtx bridges the store tenant ID into the browser package's own context key.
func browserCtx(r *http.Request) context.Context {
	ctx := r.Context()
	if tid := store.TenantIDFromContext(ctx); tid.String() != "00000000-0000-0000-0000-000000000000" {
		ctx = browser.WithTenantID(ctx, tid.String())
	}
	return ctx
}

// handleCreate creates a new screencast session token for a live view share link.
func (h *BrowserLiveHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetID string `json:"targetId"`
		Mode     string `json:"mode"` // "view" or "takeover"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TargetID == "" {
		http.Error(w, "targetId is required", http.StatusBadRequest)
		return
	}
	if req.Mode == "" {
		req.Mode = "view"
	}

	// Verify target page exists before creating a session token.
	// After browser restart, old targetIDs are gone — fail fast with a clear error.
	if h.getManager().PageByTargetID(req.TargetID) == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "target page not found"})
		return
	}

	// Generate crypto-random token (20 bytes = 40 hex chars).
	// Kept under 64 hex chars to avoid the tool output scrubber's long-hex-string rule.
	tokenBytes := make([]byte, 20)
	if _, err := rand.Read(tokenBytes); err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(tokenBytes)

	tenantID := store.TenantIDFromContext(r.Context()).String()
	sess := &store.ScreencastSession{
		TenantID:  tenantID,
		Token:     token,
		TargetID:  req.TargetID,
		Mode:      req.Mode,
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}

	if err := h.sessions.Create(r.Context(), sess); err != nil {
		h.logger.Warn("failed to create screencast session", "error", err)
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token": token,
		"url":   "/browser/live/" + token,
	})
}

// handleView serves the self-contained HTML viewer for a live session.
func (h *BrowserLiveHandler) handleView(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	sess, err := h.sessions.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found or expired", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		http.Error(w, "session expired", http.StatusGone)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, liveViewHTML, token, sess.Mode)
}

// handleInfo returns session metadata as JSON (public, token-based auth).
func (h *BrowserLiveHandler) handleInfo(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		http.Error(w, "token required", http.StatusBadRequest)
		return
	}

	sess, err := h.sessions.GetByToken(r.Context(), token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found or expired", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if time.Now().After(sess.ExpiresAt) {
		http.Error(w, "session expired", http.StatusGone)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"mode":      sess.Mode,
		"targetId":  sess.TargetID,
		"expiresAt": sess.ExpiresAt.Format(time.RFC3339),
	})
}

package pancake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
)

const (
	webhookPath  = "/channels/pancake/webhook"
	maxBodyBytes = 1 << 20 // 1 MB — prevent abuse
)

// verifyHMAC verifies a Pancake HMAC-SHA256 signature.
// Expected header format: "sha256=<hex-digest>"
func verifyHMAC(body []byte, secret, signature string) bool {
	const prefix = "sha256="
	if len(signature) <= len(prefix) {
		return false
	}
	got, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(got, expected)
}

// --- Global webhook router for multi-page support ---

// webhookRouter routes incoming Pancake webhook events to the correct channel instance by page_id.
// A single HTTP handler is shared across all pancake channel instances.
type webhookRouter struct {
	mu           sync.RWMutex
	instances    map[string]*Channel // pageID → channel
	routeHandled bool                // true after first webhookRoute() call
}

var globalRouter = &webhookRouter{
	instances: make(map[string]*Channel),
}

func (r *webhookRouter) register(ch *Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instances[ch.pageID] = ch
}

func (r *webhookRouter) unregister(pageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.instances, pageID)
}

// webhookRoute returns the path+handler on first call; ("", nil) for subsequent calls.
// The HTTP mux retains the route once registered — routeHandled prevents duplicate mounts.
func (r *webhookRouter) webhookRoute() (string, http.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.routeHandled {
		r.routeHandled = true
		return webhookPath, r
	}
	return "", nil
}

// ServeHTTP is the shared handler for all Pancake page webhooks.
// Always returns HTTP 200 — Pancake suspends webhooks if >80% errors in a 30-min window.
func (r *webhookRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}

	lr := io.LimitReader(req.Body, maxBodyBytes+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		slog.Warn("pancake: router read body error", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	var event WebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		slog.Warn("pancake: router parse event error", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	if event.Event != "messaging" {
		slog.Debug("pancake: router skipping non-messaging event", "event", event.Event)
		w.WriteHeader(http.StatusOK)
		return
	}

	var data MessagingData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		slog.Warn("pancake: router parse messaging data error", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	r.mu.RLock()
	target := r.instances[data.PageID]
	r.mu.RUnlock()

	if target == nil {
		slog.Warn("pancake: no channel instance for page_id", "page_id", data.PageID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// HMAC signature verification — skip if webhook_secret not configured.
	if target.webhookSecret != "" {
		sig := req.Header.Get("X-Pancake-Signature")
		if !verifyHMAC(body, target.webhookSecret, sig) {
			slog.Warn("security.pancake_webhook_signature_mismatch",
				"page_id", data.PageID,
				"remote_addr", req.RemoteAddr)
			// Still return 200 to avoid Pancake webhook suspension.
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	target.handleMessagingEvent(data)
	w.WriteHeader(http.StatusOK)
}

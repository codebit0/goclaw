package whatsapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

const pairingDebounceTime = 60 * time.Second

// Channel connects to a WhatsApp bridge via WebSocket.
// The bridge (e.g. whatsapp-web.js based) handles the actual WhatsApp
// protocol; this channel just sends/receives JSON messages over WS.
type Channel struct {
	*channels.BaseChannel
	conn            *websocket.Conn
	config          config.WhatsAppConfig
	mu              sync.Mutex
	connected       bool
	ctx             context.Context
	cancel          context.CancelFunc
	pairingService  store.PairingStore
	pairingDebounce sync.Map // senderID → time.Time
	approvedGroups  sync.Map // chatID → true (in-memory cache for paired groups)

	// QR caching: last QR PNG from the bridge (base64) for wizard delivery.
	lastQRMu        sync.RWMutex
	lastQRB64       string // base64-encoded PNG, empty when bridge is already authenticated
	waAuthenticated bool   // true once bridge reports WhatsApp account is connected
	myJID           string // bot's phone JID (e.g. "1234567890@s.whatsapp.net"), for mention detection
	myLID           string // bot's Link ID (e.g. "9876543210@lid"), WhatsApp's newer identifier

	// typingCancel tracks active typing-refresh loops per chatID.
	// WhatsApp clears "composing" after ~10s, so we refresh every 8s until the reply is sent.
	typingCancel sync.Map // chatID → context.CancelFunc
}

// GetLastQRB64 returns the most recent QR PNG (base64) received from the bridge.
// Returns "" when the bridge is already authenticated or no QR has been received yet.
func (c *Channel) GetLastQRB64() string {
	c.lastQRMu.RLock()
	defer c.lastQRMu.RUnlock()
	return c.lastQRB64
}

// IsAuthenticated reports whether the WhatsApp account is currently authenticated via the bridge.
func (c *Channel) IsAuthenticated() bool {
	c.lastQRMu.RLock()
	defer c.lastQRMu.RUnlock()
	return c.waAuthenticated
}

// New creates a new WhatsApp channel from config.
func New(cfg config.WhatsAppConfig, msgBus *bus.MessageBus, pairingSvc store.PairingStore) (*Channel, error) {
	if cfg.BridgeURL == "" {
		return nil, fmt.Errorf("whatsapp bridge_url is required")
	}

	base := channels.NewBaseChannel(channels.TypeWhatsApp, msgBus, cfg.AllowFrom)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	return &Channel{
		BaseChannel:    base,
		config:         cfg,
		pairingService: pairingSvc,
	}, nil
}

// Start connects to the WhatsApp bridge WebSocket and begins listening.
func (c *Channel) Start(ctx context.Context) error {
	slog.Info("starting whatsapp channel", "bridge_url", c.config.BridgeURL)
	c.MarkStarting("Connecting to WhatsApp bridge")

	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.connect(); err != nil {
		// Don't fail hard — reconnect loop will keep trying.
		slog.Warn("initial whatsapp bridge connection failed, will retry", "error", err)
		c.MarkDegraded("Bridge unreachable", err.Error(), channels.ChannelFailureKindNetwork, true)
	}

	go c.listenLoop()

	c.SetRunning(true)
	return nil
}

// BlockReplyEnabled returns the per-channel block_reply override (nil = inherit gateway default).
func (c *Channel) BlockReplyEnabled() *bool { return c.config.BlockReply }

// Stop gracefully shuts down the WhatsApp channel.
func (c *Channel) Stop(_ context.Context) error {
	slog.Info("stopping whatsapp channel")

	if c.cancel != nil {
		c.cancel()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected = false
	c.SetRunning(false)

	// Cancel all active typing goroutines to prevent leaks.
	c.typingCancel.Range(func(key, value any) bool {
		value.(context.CancelFunc)()
		c.typingCancel.Delete(key)
		return true
	})

	c.MarkStopped("Stopped")

	return nil
}

// SendBridgeCommand sends a control command to the bridge (e.g. reauth, ping, pairing_code).
// Extra optional fields are merged into the command payload.
// Bridge protocol: { type: "command", action: "<action>", ...extra }
func (c *Channel) SendBridgeCommand(action string, extra ...map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("whatsapp bridge not connected")
	}
	payload := map[string]any{"type": "command", "action": action}
	for _, e := range extra {
		for k, v := range e {
			payload[k] = v
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// Send delivers an outbound message to the WhatsApp bridge.
// Supports text-only messages and messages with media attachments.
func (c *Channel) Send(_ context.Context, msg bus.OutboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("whatsapp bridge not connected")
	}

	// Send media attachments first (each as a separate bridge message).
	if len(msg.Media) > 0 {
		for i, m := range msg.Media {
			// Use message content as caption for the first media item only.
			caption := m.Caption
			if caption == "" && i == 0 && msg.Content != "" {
				caption = markdownToWhatsApp(msg.Content)
			}
			mediaPayload := map[string]any{
				"type":     "media",
				"to":       msg.ChatID,
				"path":     m.URL,
				"mimetype": m.ContentType,
				"caption":  caption,
			}
			data, err := json.Marshal(mediaPayload)
			if err != nil {
				return fmt.Errorf("marshal whatsapp media: %w", err)
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return fmt.Errorf("send whatsapp media: %w", err)
			}
		}
		// If caption was used on first media, don't send content again as text.
		if msg.Media[0].Caption == "" && msg.Content != "" {
			msg.Content = ""
		}
	}

	// Send text content (if any remains after media caption).
	if msg.Content != "" {
		payload := map[string]any{
			"type":    "message",
			"to":      msg.ChatID,
			"content": markdownToWhatsApp(msg.Content),
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal whatsapp message: %w", err)
		}
		if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return fmt.Errorf("send whatsapp message: %w", err)
		}
	}

	// Stop typing loop synchronously, then send "paused" after releasing the lock.
	chatID := msg.ChatID
	if cancel, ok := c.typingCancel.LoadAndDelete(chatID); ok {
		cancel.(context.CancelFunc)()
	}
	go c.sendPresence(chatID, "paused")

	return nil
}

// keepTyping sends "composing" presence repeatedly until ctx is cancelled.
// WhatsApp clears the typing indicator after ~10s so we refresh every 8s.
func (c *Channel) keepTyping(ctx context.Context, chatID string) {
	c.sendPresence(chatID, "composing")
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendPresence(chatID, "composing")
		}
	}
}

// sendPresence sends a WhatsApp presence update (composing / paused) to a chat.
func (c *Channel) sendPresence(to, state string) {
	if err := c.SendBridgeCommand("presence", map[string]any{"to": to, "state": state}); err != nil {
		slog.Debug("whatsapp: failed to send presence update", "state", state, "error", err)
	}
}

// connect establishes the WebSocket connection to the bridge.
func (c *Channel) connect() error {
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, _, err := dialer.Dial(c.config.BridgeURL, nil)
	if err != nil {
		return fmt.Errorf("dial whatsapp bridge %s: %w", c.config.BridgeURL, err)
	}

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.mu.Unlock()

	slog.Info("whatsapp bridge connected", "url", c.config.BridgeURL)
	return nil
}

// listenLoop reads messages from the bridge with automatic reconnection.
func (c *Channel) listenLoop() {
	backoff := time.Second

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			// Not connected — attempt reconnect with backoff
			slog.Info("attempting whatsapp bridge reconnect", "backoff", backoff)

			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
			}

			if err := c.connect(); err != nil {
				slog.Warn("whatsapp bridge reconnect failed", "error", err)
				c.MarkDegraded("Bridge unreachable", err.Error(), channels.ChannelFailureKindNetwork, true)
				backoff = min(backoff*2, 30*time.Second)
				continue
			}

			backoff = time.Second // reset on success
			continue
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			slog.Warn("whatsapp read error, will reconnect", "error", err)

			c.mu.Lock()
			if c.conn != nil {
				_ = c.conn.Close()
				c.conn = nil
			}
			c.connected = false
			c.mu.Unlock()

			continue
		}

		var msg map[string]any
		if err := json.Unmarshal(message, &msg); err != nil {
			slog.Warn("invalid whatsapp message JSON", "error", err)
			continue
		}

		msgType, _ := msg["type"].(string)
		switch msgType {
		case "message":
			c.handleIncomingMessage(msg)
		case "qr":
			c.handleBridgeQR(msg)
		case "status":
			c.handleBridgeStatus(msg)
		case "":
			// Bridge sent a message without a "type" field — common misconfiguration.
			// Expected format: {"type":"message","from":"...","chat":"...","content":"..."}
			// Check your bridge: fields must be "from"/"content", not "sender"/"body".
			slog.Warn("whatsapp bridge sent message without 'type' field — bridge format mismatch",
				"hint", "add type:\"message\", rename sender→from, body→content",
				"received_keys", mapKeys(msg),
			)
		default:
			slog.Debug("whatsapp bridge unknown event type, ignoring", "type", msgType)
		}
	}
}

// mapKeys returns the keys of a map for diagnostic logging.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// handleBridgeQR processes a QR code event from the bridge.
// It generates a PNG, caches it, and broadcasts a bus event for the QR wizard.
func (c *Channel) handleBridgeQR(msg map[string]any) {
	rawQR, _ := msg["data"].(string)
	if rawQR == "" {
		return
	}

	png, err := qrcode.Encode(rawQR, qrcode.Medium, 256)
	if err != nil {
		slog.Warn("whatsapp: failed to encode QR PNG", "error", err)
		return
	}
	pngB64 := base64.StdEncoding.EncodeToString(png)

	c.lastQRMu.Lock()
	c.lastQRB64 = pngB64
	c.lastQRMu.Unlock()

	slog.Info("whatsapp bridge QR received — scan to authenticate", "channel", c.Name())

	if mb := c.Bus(); mb != nil {
		mb.Broadcast(bus.Event{
			Name:     protocol.EventWhatsAppQRCode,
			TenantID: c.TenantID(),
			Payload: map[string]any{
				"channel_name": c.Name(),
				"png_b64":      pngB64,
			},
		})
	}
}

// handleBridgeStatus processes a status event from the bridge.
// On connect, it marks the channel healthy and broadcasts a QR-done event.
// On disconnect, it marks the channel degraded (reconnect loop will retry).
func (c *Channel) handleBridgeStatus(msg map[string]any) {
	connected, _ := msg["connected"].(bool)
	slog.Debug("whatsapp bridge status", "connected", connected, "channel", c.Name())

	c.lastQRMu.Lock()
	c.waAuthenticated = connected
	if connected {
		c.lastQRB64 = "" // clear QR — no longer needed
		// Capture bot's own JID + LID for group mention detection.
		// WhatsApp uses two identifier systems: phone JID and LID (Link ID).
		// Mentions in groups may use either format.
		if me, ok := msg["me"].(string); ok && me != "" {
			c.myJID = normalizeJID(me)
			slog.Info("whatsapp: bot JID set", "jid", c.myJID, "channel", c.Name())
		}
		if meLID, ok := msg["me_lid"].(string); ok && meLID != "" {
			c.myLID = normalizeJID(meLID)
			slog.Info("whatsapp: bot LID set", "lid", c.myLID, "channel", c.Name())
		}
	}
	c.lastQRMu.Unlock()

	if connected {
		c.MarkHealthy("WhatsApp authenticated and connected")
		if mb := c.Bus(); mb != nil {
			mb.Broadcast(bus.Event{
				Name:     protocol.EventWhatsAppQRDone,
				TenantID: c.TenantID(),
				Payload: map[string]any{
					"channel_name": c.Name(),
					"success":      true,
				},
			})
		}
	} else {
		c.MarkDegraded("WhatsApp disconnected", "Bridge reported disconnection — waiting for reconnect", channels.ChannelFailureKindNetwork, true)
	}
}

// handleIncomingMessage processes a message received from the bridge.
// Expected format: {"type":"message","from":"...","chat":"...","content":"...","id":"...","from_name":"...","media":[...]}
func (c *Channel) handleIncomingMessage(msg map[string]any) {
	ctx := context.Background()
	ctx = store.WithTenantID(ctx, c.TenantID())
	senderID, ok := msg["from"].(string)
	if !ok || senderID == "" {
		return
	}

	chatID, _ := msg["chat"].(string)
	if chatID == "" {
		chatID = senderID
	}

	// WhatsApp groups have chatID ending in "@g.us"
	peerKind := "direct"
	if strings.HasSuffix(chatID, "@g.us") {
		peerKind = "group"
	}

	slog.Debug("whatsapp incoming", "peer", peerKind, "sender", senderID, "chat", chatID, "policy", c.config.GroupPolicy)

	// DM/Group policy check
	if peerKind == "direct" {
		if !c.checkDMPolicy(ctx, senderID, chatID) {
			return
		}
	} else {
		if !c.checkGroupPolicy(ctx, senderID, chatID) {
			slog.Info("whatsapp group message rejected by policy", "sender_id", senderID, "chat_id", chatID, "policy", c.config.GroupPolicy)
			return
		}
	}

	// Allowlist check
	if !c.IsAllowed(senderID) {
		slog.Info("whatsapp message rejected by allowlist", "sender_id", senderID)
		return
	}

	content, _ := msg["content"].(string)

	// Parse media items from bridge: [{type, mimetype, filename, path}, ...]
	var mediaList []media.MediaInfo
	if mediaData, ok := msg["media"].([]any); ok {
		for _, item := range mediaData {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			filePath, _ := m["path"].(string)
			if filePath == "" {
				continue
			}
			mediaType, _ := m["type"].(string)
			mimeType, _ := m["mimetype"].(string)
			fileName, _ := m["filename"].(string)
			mediaList = append(mediaList, media.MediaInfo{
				Type:        mediaType,
				FilePath:    filePath,
				ContentType: mimeType,
				FileName:    fileName,
			})
		}
	}

	if content == "" && len(mediaList) == 0 {
		return // nothing to process
	}
	if content == "" {
		content = "[empty message]"
	}

	metadata := make(map[string]string)
	if messageID, ok := msg["id"].(string); ok {
		metadata["message_id"] = messageID
	}
	if userName, ok := msg["from_name"].(string); ok {
		metadata["user_name"] = userName
	}

	// require_mention: in groups, only process when the bot is @mentioned.
	// WhatsApp uses two identifier systems: phone JID (@s.whatsapp.net) and LID (@lid).
	// Mentions may use either format, so we check both.
	// Fails closed: if both JID and LID are unknown, treat as not-mentioned.
	if peerKind == "group" && c.config.RequireMention != nil && *c.config.RequireMention {
		c.lastQRMu.RLock()
		myJID := c.myJID
		myLID := c.myLID
		c.lastQRMu.RUnlock()
		mentioned := false
		if myJID != "" || myLID != "" {
			myJIDNorm := normalizeJID(myJID)
			myLIDNorm := normalizeJID(myLID)
			if jids, ok := msg["mentioned_jids"].([]any); ok {
				for _, j := range jids {
					jid, ok := j.(string)
					if !ok {
						continue
					}
					norm := normalizeJID(jid)
					if (myJIDNorm != "" && norm == myJIDNorm) || (myLIDNorm != "" && norm == myLIDNorm) {
						mentioned = true
						break
					}
				}
			}
		}
		if !mentioned {
			slog.Info("whatsapp group message skipped — bot not @mentioned", "sender_id", senderID, "my_jid", myJID, "my_lid", myLID)
			return
		}
	}

	slog.Debug("whatsapp message received",
		"sender_id", senderID,
		"chat_id", chatID,
		"preview", channels.Truncate(content, 50),
	)

	// Collect contact for processed messages.
	if cc := c.ContactCollector(); cc != nil {
		cc.EnsureContact(ctx, c.Type(), c.Name(), senderID, senderID, metadata["user_name"], "", peerKind, "user", "", "")
	}

	// Build media tags (e.g. <media:image>, <media:document name="...">)
	// and bus.MediaFile list for the agent pipeline.
	var mediaFiles []bus.MediaFile
	if len(mediaList) > 0 {
		mediaTags := media.BuildMediaTags(mediaList)
		if mediaTags != "" {
			if content != "[empty message]" {
				content = mediaTags + "\n\n" + content
			} else {
				content = mediaTags
			}
		}
		for _, m := range mediaList {
			if m.FilePath != "" {
				mediaFiles = append(mediaFiles, bus.MediaFile{
					Path:     m.FilePath,
					MimeType: m.ContentType,
				})
			}
		}
	}

	// Annotate with sender identity so the agent knows who is messaging.
	if senderName := metadata["user_name"]; senderName != "" {
		content = fmt.Sprintf("[From: %s]\n%s", senderName, content)
	}

	// Cancel any previous typing loop for this chat before starting a new one.
	// Without this, consecutive messages leak orphaned goroutines that send
	// "composing" forever (the old cancel gets overwritten in the sync.Map).
	if prevCancel, ok := c.typingCancel.LoadAndDelete(chatID); ok {
		prevCancel.(context.CancelFunc)()
	}

	// Show typing indicator for the full duration of agent processing.
	// WhatsApp clears "composing" after ~10s so we refresh every 8s.
	typingCtx, typingCancel := context.WithCancel(context.Background())
	c.typingCancel.Store(chatID, typingCancel)
	go c.keepTyping(typingCtx, chatID)

	// Derive userID from senderID.
	userID := senderID
	if idx := strings.IndexByte(senderID, '|'); idx > 0 {
		userID = senderID[:idx]
	}

	c.Bus().PublishInbound(bus.InboundMessage{
		Channel:  c.Name(),
		SenderID: senderID,
		ChatID:   chatID,
		Content:  content,
		Media:    mediaFiles,
		PeerKind: peerKind,
		UserID:   userID,
		AgentID:  c.AgentID(),
		TenantID: c.TenantID(),
		Metadata: metadata,
	})
}

// checkGroupPolicy evaluates the group policy for a sender, with pairing support.
func (c *Channel) checkGroupPolicy(ctx context.Context, senderID, chatID string) bool {
	groupPolicy := c.config.GroupPolicy
	if groupPolicy == "" {
		groupPolicy = "open"
	}

	switch groupPolicy {
	case "disabled":
		return false
	case "allowlist":
		return c.IsAllowed(senderID)
	case "pairing":
		if c.HasAllowList() && c.IsAllowed(senderID) {
			return true
		}
		if _, cached := c.approvedGroups.Load(chatID); cached {
			return true
		}
		groupSenderID := fmt.Sprintf("group:%s", chatID)
		if c.pairingService != nil {
			paired, err := c.pairingService.IsPaired(ctx, groupSenderID, c.Name())
			if err != nil {
				slog.Warn("security.pairing_check_failed, assuming paired (fail-open)",
					"group_sender", groupSenderID, "channel", c.Name(), "error", err)
				paired = true
			}
			if paired {
				c.approvedGroups.Store(chatID, true)
				return true
			}
		}
		c.sendPairingReply(ctx, groupSenderID, chatID)
		return false
	default: // "open"
		return true
	}
}

// checkDMPolicy evaluates the DM policy for a sender, handling pairing flow.
func (c *Channel) checkDMPolicy(ctx context.Context, senderID, chatID string) bool {
	dmPolicy := c.config.DMPolicy
	if dmPolicy == "" {
		dmPolicy = "pairing"
	}

	switch dmPolicy {
	case "disabled":
		slog.Debug("whatsapp DM rejected: disabled", "sender_id", senderID)
		return false
	case "open":
		return true
	case "allowlist":
		if !c.IsAllowed(senderID) {
			slog.Debug("whatsapp DM rejected by allowlist", "sender_id", senderID)
			return false
		}
		return true
	default: // "pairing"
		paired := false
		if c.pairingService != nil {
			p, err := c.pairingService.IsPaired(ctx, senderID, c.Name())
			if err != nil {
				slog.Warn("security.pairing_check_failed, assuming paired (fail-open)",
					"sender_id", senderID, "channel", c.Name(), "error", err)
				paired = true
			} else {
				paired = p
			}
		}
		inAllowList := c.HasAllowList() && c.IsAllowed(senderID)

		if paired || inAllowList {
			return true
		}

		c.sendPairingReply(ctx, senderID, chatID)
		return false
	}
}

// sendPairingReply sends a pairing code to the user via the WS bridge.
func (c *Channel) sendPairingReply(ctx context.Context, senderID, chatID string) {
	if c.pairingService == nil {
		slog.Warn("whatsapp pairing: no pairing service configured")
		return
	}

	// Debounce
	if lastSent, ok := c.pairingDebounce.Load(senderID); ok {
		if time.Since(lastSent.(time.Time)) < pairingDebounceTime {
			slog.Info("whatsapp pairing: debounced", "sender_id", senderID)
			return
		}
	}

	code, err := c.pairingService.RequestPairing(ctx, senderID, c.Name(), chatID, "default", nil)
	if err != nil {
		slog.Warn("whatsapp pairing request failed", "sender_id", senderID, "channel", c.Name(), "error", err)
		return
	}

	replyText := fmt.Sprintf(
		"GoClaw: access not configured.\n\nYour WhatsApp ID: %s\n\nPairing code: %s\n\nAsk the bot owner to approve with:\n  goclaw pairing approve %s",
		senderID, code, code,
	)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		slog.Warn("whatsapp bridge not connected, cannot send pairing reply")
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"type":    "message",
		"to":      chatID,
		"content": replyText,
	})

	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		slog.Warn("failed to send whatsapp pairing reply", "error", err)
	} else {
		c.pairingDebounce.Store(senderID, time.Now())
		slog.Info("whatsapp pairing reply sent", "sender_id", senderID, "code", code)
	}
}

// normalizeJID strips the Baileys device suffix from a WhatsApp JID.
// e.g. "12345678901:42@s.whatsapp.net" → "12345678901@s.whatsapp.net"
func normalizeJID(jid string) string {
	colonIdx := strings.Index(jid, ":")
	atIdx := strings.Index(jid, "@")
	if colonIdx > 0 && atIdx > colonIdx {
		return jid[:colonIdx] + jid[atIdx:]
	}
	return jid
}

package whatsapp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/channels/media"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	pairingDebounceTime = 60 * time.Second
	maxMessageLen       = 4096 // WhatsApp practical message length limit
)

func init() {
	// Set device name shown in WhatsApp's "Linked Devices" screen (once at package init).
	wastore.DeviceProps.Os = proto.String("GoClaw")
}

// Channel connects directly to WhatsApp via go.mau.fi/whatsmeow.
// Auth state is stored in PostgreSQL (standard) or SQLite (desktop).
type Channel struct {
	*channels.BaseChannel
	client          *whatsmeow.Client
	container       *sqlstore.Container
	config          config.WhatsAppConfig
	mu              sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	pairingService  store.PairingStore
	pairingDebounce sync.Map // senderID → time.Time
	approvedGroups  sync.Map // chatID → true (in-memory cache for paired groups)
	groupHistory    *channels.PendingHistory // tracks group messages for context

	// QR state
	lastQRMu        sync.RWMutex
	lastQRB64       string     // base64-encoded PNG, empty when authenticated
	waAuthenticated bool       // true once WhatsApp account is connected
	myJID           types.JID  // linked account's phone JID for mention detection
	myLID           types.JID  // linked account's LID — WhatsApp's newer identifier

	// typingCancel tracks active typing-refresh loops per chatID.
	typingCancel sync.Map // chatID string → context.CancelFunc
}

// GetLastQRB64 returns the most recent QR PNG (base64).
func (c *Channel) GetLastQRB64() string {
	c.lastQRMu.RLock()
	defer c.lastQRMu.RUnlock()
	return c.lastQRB64
}

// IsAuthenticated reports whether the WhatsApp account is currently authenticated.
func (c *Channel) IsAuthenticated() bool {
	c.lastQRMu.RLock()
	defer c.lastQRMu.RUnlock()
	return c.waAuthenticated
}

// cacheQR stores the latest QR PNG (base64) for late-joining wizard clients.
func (c *Channel) cacheQR(pngB64 string) {
	c.lastQRMu.Lock()
	c.lastQRB64 = pngB64
	c.lastQRMu.Unlock()
}

// detectDialect returns the sqlstore dialect string based on the DB driver.
func detectDialect(db *sql.DB) string {
	driverName := fmt.Sprintf("%T", db.Driver())
	if strings.Contains(driverName, "sqlite") {
		return "sqlite3"
	}
	return "pgx"
}

// New creates a new WhatsApp channel backed by whatsmeow.
func New(cfg config.WhatsAppConfig, msgBus *bus.MessageBus,
	pairingSvc store.PairingStore, db *sql.DB,
	pendingStore store.PendingMessageStore) (*Channel, error) {

	base := channels.NewBaseChannel(channels.TypeWhatsApp, msgBus, cfg.AllowFrom)
	base.ValidatePolicy(cfg.DMPolicy, cfg.GroupPolicy)

	dialect := detectDialect(db)
	container := sqlstore.NewWithDB(db, dialect, nil)
	if err := container.Upgrade(context.Background()); err != nil {
		return nil, fmt.Errorf("whatsapp sqlstore upgrade: %w", err)
	}

	return &Channel{
		BaseChannel:    base,
		config:         cfg,
		pairingService: pairingSvc,
		container:      container,
		groupHistory:   channels.MakeHistory("whatsapp", pendingStore, base.TenantID()),
	}, nil
}

// Start initializes the whatsmeow client and connects to WhatsApp.
func (c *Channel) Start(ctx context.Context) error {
	slog.Info("starting whatsapp channel (whatsmeow)")
	c.MarkStarting("Initializing WhatsApp connection")

	c.ctx, c.cancel = context.WithCancel(ctx)

	deviceStore, err := c.container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp get device: %w", err)
	}

	c.client = whatsmeow.NewClient(deviceStore, nil)
	c.client.AddEventHandler(c.handleEvent)

	if c.client.Store.ID == nil {
		// Not paired yet — QR flow will be triggered by qr_methods.go.
		slog.Info("whatsapp: not paired yet, waiting for QR scan", "channel", c.Name())
		c.MarkDegraded("Awaiting QR scan", "Scan QR code to authenticate",
			channels.ChannelFailureKindAuth, false)
	} else {
		if err := c.client.Connect(); err != nil {
			slog.Warn("whatsapp: initial connect failed", "error", err)
			c.MarkDegraded("Connection failed", err.Error(),
				channels.ChannelFailureKindNetwork, true)
		}
	}

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
	if c.client != nil {
		c.client.Disconnect()
	}

	// Cancel all active typing goroutines.
	c.typingCancel.Range(func(key, value any) bool {
		value.(context.CancelFunc)()
		c.typingCancel.Delete(key)
		return true
	})

	c.SetRunning(false)
	c.MarkStopped("Stopped")
	return nil
}

// handleEvent dispatches whatsmeow events.
func (c *Channel) handleEvent(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		c.handleIncomingMessage(v)
	case *events.Connected:
		c.handleConnected()
	case *events.Disconnected:
		c.handleDisconnected()
	case *events.LoggedOut:
		c.handleLoggedOut(v)
	case *events.PairSuccess:
		slog.Info("whatsapp: pair success", "channel", c.Name())
	}
}

// handleConnected processes the Connected event.
func (c *Channel) handleConnected() {
	c.lastQRMu.Lock()
	c.waAuthenticated = true
	c.lastQRB64 = ""
	if c.client.Store.ID != nil {
		c.myJID = *c.client.Store.ID
		c.myLID = c.client.Store.GetLID()
		slog.Info("whatsapp: connected", "jid", c.myJID.String(),
			"lid", c.myLID.String(), "channel", c.Name())
	}
	c.lastQRMu.Unlock()

	c.MarkHealthy("WhatsApp authenticated and connected")
}

// handleDisconnected processes the Disconnected event.
func (c *Channel) handleDisconnected() {
	c.lastQRMu.Lock()
	c.waAuthenticated = false
	c.lastQRMu.Unlock()

	c.MarkDegraded("WhatsApp disconnected", "Waiting for reconnect",
		channels.ChannelFailureKindNetwork, true)
	// whatsmeow auto-reconnects — no manual reconnect loop needed.
}

// handleLoggedOut processes the LoggedOut event.
func (c *Channel) handleLoggedOut(evt *events.LoggedOut) {
	slog.Warn("whatsapp: logged out", "reason", evt.Reason, "channel", c.Name())
	c.lastQRMu.Lock()
	c.waAuthenticated = false
	c.lastQRMu.Unlock()

	c.MarkDegraded("WhatsApp logged out", "Re-scan QR to reconnect",
		channels.ChannelFailureKindAuth, false)
}

// handleIncomingMessage processes an incoming WhatsApp message.
func (c *Channel) handleIncomingMessage(evt *events.Message) {
	ctx := context.Background()
	ctx = store.WithTenantID(ctx, c.TenantID())

	if evt.Info.IsFromMe {
		return
	}

	senderJID := evt.Info.Sender
	chatJID := evt.Info.Chat

	// WhatsApp uses dual identity: phone JID (@s.whatsapp.net) and LID (@lid).
	// Groups may use LID addressing. Normalize to phone JID for consistent
	// policy checks, pairing lookups, allowlists, and contact collection.
	if evt.Info.AddressingMode == types.AddressingModeLID && !evt.Info.SenderAlt.IsEmpty() {
		senderJID = evt.Info.SenderAlt
	}

	senderID := senderJID.String()
	chatID := chatJID.String()

	peerKind := "direct"
	if chatJID.Server == types.GroupServer {
		peerKind = "group"
	}

	slog.Debug("whatsapp incoming", "peer", peerKind, "sender", senderID, "chat", chatID,
		"addressing", evt.Info.AddressingMode, "policy", c.config.GroupPolicy)

	// DM/Group policy check.
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

	if !c.IsAllowed(senderID) {
		slog.Info("whatsapp message rejected by allowlist", "sender_id", senderID)
		return
	}

	content := extractTextContent(evt.Message)

	var mediaList []media.MediaInfo
	mediaList = c.downloadMedia(evt)

	if content == "" && len(mediaList) == 0 {
		return
	}
	if content == "" {
		content = "[empty message]"
	}

	// Group history + mention detection.
	historyLimit := c.config.HistoryLimit
	if historyLimit == 0 {
		historyLimit = channels.DefaultGroupHistoryLimit
	}
	if peerKind == "group" && c.config.RequireMention != nil && *c.config.RequireMention {
		if !c.isMentioned(evt) {
			// Not mentioned — record for context and skip.
			senderLabel := evt.Info.PushName
			if senderLabel == "" {
				senderLabel = senderID
			}
			c.groupHistory.Record(chatID, channels.HistoryEntry{
				Sender:    senderLabel,
				SenderID:  senderID,
				Body:      content,
				Timestamp: evt.Info.Timestamp,
				MessageID: string(evt.Info.ID),
			}, historyLimit)
			return
		}
		// Mentioned — prepend accumulated group context.
		content = c.groupHistory.BuildContext(chatID, content, historyLimit)
		c.groupHistory.Clear(chatID)
	}

	metadata := map[string]string{
		"message_id": string(evt.Info.ID),
	}
	if evt.Info.PushName != "" {
		metadata["user_name"] = evt.Info.PushName
	}

	// Build media tags and bus.MediaFile list.
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
					Path: m.FilePath, MimeType: m.ContentType,
				})
			}
		}
	}

	// Annotate with sender identity.
	if senderName := metadata["user_name"]; senderName != "" {
		content = fmt.Sprintf("[From: %s]\n%s", senderName, content)
	}

	// Collect contact.
	if cc := c.ContactCollector(); cc != nil {
		cc.EnsureContact(ctx, c.Type(), c.Name(), senderID, senderID,
			metadata["user_name"], "", peerKind, "user", "", "")
	}

	// Typing indicator.
	if prevCancel, ok := c.typingCancel.LoadAndDelete(chatID); ok {
		prevCancel.(context.CancelFunc)()
	}
	typingCtx, typingCancel := context.WithCancel(context.Background())
	c.typingCancel.Store(chatID, typingCancel)
	go c.keepTyping(typingCtx, chatJID)

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

// extractTextContent extracts text from any WhatsApp message variant.
// Includes quoted message context when present (reply-to messages).
func extractTextContent(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}

	var text string
	var quotedText string

	if msg.GetConversation() != "" {
		text = msg.GetConversation()
	} else if ext := msg.GetExtendedTextMessage(); ext != nil {
		text = ext.GetText()
		// Extract quoted (replied-to) message text.
		if ci := ext.GetContextInfo(); ci != nil {
			if qm := ci.GetQuotedMessage(); qm != nil {
				quotedText = extractQuotedText(qm)
			}
		}
	} else if img := msg.GetImageMessage(); img != nil {
		text = img.GetCaption()
	} else if vid := msg.GetVideoMessage(); vid != nil {
		text = vid.GetCaption()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		text = doc.GetCaption()
	}

	if quotedText != "" && text != "" {
		return fmt.Sprintf("[Replying to: %s]\n%s", quotedText, text)
	}
	if quotedText != "" {
		return fmt.Sprintf("[Replying to: %s]", quotedText)
	}
	return text
}

// extractQuotedText extracts plain text from a quoted message (no recursion).
func extractQuotedText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if msg.GetConversation() != "" {
		return msg.GetConversation()
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := msg.GetImageMessage(); img != nil && img.GetCaption() != "" {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil && vid.GetCaption() != "" {
		return vid.GetCaption()
	}
	return ""
}

// isMentioned checks if the linked account is @mentioned in a group message.
// WhatsApp uses dual identity: phone JID and LID. Mentions may use either format.
func (c *Channel) isMentioned(evt *events.Message) bool {
	c.lastQRMu.RLock()
	myJID := c.myJID
	myLID := c.myLID
	c.lastQRMu.RUnlock()

	if myJID.IsEmpty() && myLID.IsEmpty() {
		return false // fail closed: unknown identity = not mentioned
	}

	// Check mentioned JIDs from extended text.
	if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
		if ci := ext.GetContextInfo(); ci != nil {
			for _, jidStr := range ci.GetMentionedJID() {
				mentioned, _ := types.ParseJID(jidStr)
				if !myJID.IsEmpty() && mentioned.User == myJID.User {
					return true
				}
				if !myLID.IsEmpty() && mentioned.User == myLID.User {
					return true
				}
			}
		}
	}
	return false
}

// downloadMedia downloads media attachments from a WhatsApp message.
func (c *Channel) downloadMedia(evt *events.Message) []media.MediaInfo {
	msg := evt.Message
	if msg == nil {
		return nil
	}

	type mediaItem struct {
		mediaType string
		mimetype  string
		filename  string
		download  whatsmeow.DownloadableMessage
	}

	var items []mediaItem
	if img := msg.GetImageMessage(); img != nil {
		items = append(items, mediaItem{"image", img.GetMimetype(), "", img})
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		items = append(items, mediaItem{"video", vid.GetMimetype(), "", vid})
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		items = append(items, mediaItem{"audio", aud.GetMimetype(), "", aud})
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		items = append(items, mediaItem{"document", doc.GetMimetype(), doc.GetFileName(), doc})
	}
	if stk := msg.GetStickerMessage(); stk != nil {
		items = append(items, mediaItem{"sticker", stk.GetMimetype(), "", stk})
	}

	if len(items) == 0 {
		return nil
	}

	var result []media.MediaInfo
	for _, item := range items {
		data, err := c.client.Download(c.ctx, item.download)
		if err != nil {
			slog.Warn("whatsapp: media download failed", "type", item.mediaType, "error", err)
			continue
		}
		if len(data) > 20*1024*1024 { // 20MB limit
			slog.Warn("whatsapp: media too large, skipping", "type", item.mediaType,
				"size_mb", len(data)/(1024*1024))
			continue
		}

		ext := mimeToExt(item.mimetype)
		tmpFile, err := os.CreateTemp("", "goclaw_wa_*"+ext)
		if err != nil {
			slog.Warn("whatsapp: temp file creation failed", "error", err)
			continue
		}
		if _, err := tmpFile.Write(data); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			continue
		}
		tmpFile.Close()

		result = append(result, media.MediaInfo{
			Type:        item.mediaType,
			FilePath:    tmpFile.Name(),
			ContentType: item.mimetype,
			FileName:    item.filename,
		})
	}
	return result
}

// mimeToExt maps MIME types to file extensions.
func mimeToExt(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(mime, "image/png"):
		return ".png"
	case strings.HasPrefix(mime, "image/webp"):
		return ".webp"
	case strings.HasPrefix(mime, "video/mp4"):
		return ".mp4"
	case strings.HasPrefix(mime, "audio/ogg"):
		return ".ogg"
	case strings.HasPrefix(mime, "audio/mpeg"):
		return ".mp3"
	case strings.Contains(mime, "pdf"):
		return ".pdf"
	default:
		return ".bin"
	}
}

// Send delivers an outbound message to WhatsApp via whatsmeow.
func (c *Channel) Send(_ context.Context, msg bus.OutboundMessage) error {
	if c.client == nil || !c.client.IsConnected() {
		return fmt.Errorf("whatsapp not connected")
	}

	chatJID, err := types.ParseJID(msg.ChatID)
	if err != nil {
		return fmt.Errorf("invalid whatsapp JID %q: %w", msg.ChatID, err)
	}

	// Send media attachments first.
	if len(msg.Media) > 0 {
		for i, m := range msg.Media {
			caption := m.Caption
			if caption == "" && i == 0 && msg.Content != "" {
				caption = markdownToWhatsApp(msg.Content)
			}

			data, readErr := os.ReadFile(m.URL)
			if readErr != nil {
				return fmt.Errorf("read media file: %w", readErr)
			}

			waMsg, buildErr := c.buildMediaMessage(data, m.ContentType, caption)
			if buildErr != nil {
				return fmt.Errorf("build media message: %w", buildErr)
			}

			if _, sendErr := c.client.SendMessage(c.ctx, chatJID, waMsg); sendErr != nil {
				return fmt.Errorf("send whatsapp media: %w", sendErr)
			}
		}
		// Skip text if caption was used on first media.
		if msg.Media[0].Caption == "" && msg.Content != "" {
			msg.Content = ""
		}
	}

	// Send text (chunked if exceeding limit).
	if msg.Content != "" {
		formatted := markdownToWhatsApp(msg.Content)
		chunks := chunkText(formatted, maxMessageLen)
		for _, chunk := range chunks {
			waMsg := &waE2E.Message{
				Conversation: proto.String(chunk),
			}
			if _, err := c.client.SendMessage(c.ctx, chatJID, waMsg); err != nil {
				return fmt.Errorf("send whatsapp message: %w", err)
			}
		}
	}

	// Stop typing indicator.
	if cancel, ok := c.typingCancel.LoadAndDelete(msg.ChatID); ok {
		cancel.(context.CancelFunc)()
	}
	go c.sendPresence(chatJID, types.ChatPresencePaused)

	return nil
}

// buildMediaMessage uploads media to WhatsApp and returns the message proto.
func (c *Channel) buildMediaMessage(data []byte, mime, caption string) (*waE2E.Message, error) {
	switch {
	case strings.HasPrefix(mime, "image/"):
		uploaded, err := c.client.Upload(c.ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return nil, err
		}
		return &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mime),
				URL:           &uploaded.URL,
				DirectPath:    &uploaded.DirectPath,
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}, nil

	case strings.HasPrefix(mime, "video/"):
		uploaded, err := c.client.Upload(c.ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return nil, err
		}
		return &waE2E.Message{
			VideoMessage: &waE2E.VideoMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mime),
				URL:           &uploaded.URL,
				DirectPath:    &uploaded.DirectPath,
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}, nil

	case strings.HasPrefix(mime, "audio/"):
		uploaded, err := c.client.Upload(c.ctx, data, whatsmeow.MediaAudio)
		if err != nil {
			return nil, err
		}
		return &waE2E.Message{
			AudioMessage: &waE2E.AudioMessage{
				Mimetype:      proto.String(mime),
				URL:           &uploaded.URL,
				DirectPath:    &uploaded.DirectPath,
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}, nil

	default: // document
		uploaded, err := c.client.Upload(c.ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return nil, err
		}
		return &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				Caption:       proto.String(caption),
				Mimetype:      proto.String(mime),
				URL:           &uploaded.URL,
				DirectPath:    &uploaded.DirectPath,
				MediaKey:      uploaded.MediaKey,
				FileEncSHA256: uploaded.FileEncSHA256,
				FileSHA256:    uploaded.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(data))),
			},
		}, nil
	}
}

// keepTyping sends "composing" presence repeatedly until ctx is cancelled.
func (c *Channel) keepTyping(ctx context.Context, chatJID types.JID) {
	c.sendPresence(chatJID, types.ChatPresenceComposing)
	ticker := time.NewTicker(8 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendPresence(chatJID, types.ChatPresenceComposing)
		}
	}
}

// sendPresence sends a WhatsApp chat presence update.
func (c *Channel) sendPresence(to types.JID, state types.ChatPresence) {
	if c.client == nil || !c.client.IsConnected() {
		return
	}
	if err := c.client.SendChatPresence(c.ctx, to, state, ""); err != nil {
		slog.Debug("whatsapp: presence update failed", "state", state, "error", err)
	}
}

// StartQRFlow initiates the QR authentication flow.
// Returns a channel that emits QR code strings and auth events.
// Lazily initializes the whatsmeow client if Start() hasn't been called yet
// (handles timing race between async instance reload and wizard auto-start).
func (c *Channel) StartQRFlow(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	if c.client == nil {
		// Lazy init: wizard may request QR before Start() is called.
		c.mu.Lock()
		if c.client == nil {
			if c.ctx == nil {
				c.ctx, c.cancel = context.WithCancel(context.Background())
			}
			deviceStore, err := c.container.GetFirstDevice(ctx)
			if err != nil {
				c.mu.Unlock()
				return nil, fmt.Errorf("whatsapp get device: %w", err)
			}
			c.client = whatsmeow.NewClient(deviceStore, nil)
			c.client.AddEventHandler(c.handleEvent)
		}
		c.mu.Unlock()
	}

	if c.IsAuthenticated() {
		return nil, nil // caller checks this
	}

	qrChan, err := c.client.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsapp get QR channel: %w", err)
	}

	if !c.client.IsConnected() {
		if err := c.client.Connect(); err != nil {
			return nil, fmt.Errorf("whatsapp connect for QR: %w", err)
		}
	}

	return qrChan, nil
}

// Reauth clears the current session and prepares for a fresh QR scan.
func (c *Channel) Reauth() error {
	slog.Info("whatsapp: reauth requested", "channel", c.Name())

	c.lastQRMu.Lock()
	c.waAuthenticated = false
	c.lastQRB64 = ""
	c.lastQRMu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		c.client.Disconnect()
	}

	// Delete device from store to force fresh QR on next connect.
	if c.client != nil && c.client.Store.ID != nil {
		if err := c.client.Store.Delete(context.Background()); err != nil {
			slog.Warn("whatsapp: failed to delete device store", "error", err)
		}
	}

	// Reset context so the new client gets a fresh lifecycle.
	if c.cancel != nil {
		c.cancel()
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	// Re-create client with fresh device store.
	deviceStore, err := c.container.GetFirstDevice(context.Background())
	if err != nil {
		return fmt.Errorf("whatsapp: get fresh device: %w", err)
	}
	c.client = whatsmeow.NewClient(deviceStore, nil)
	c.client.AddEventHandler(c.handleEvent)

	return nil
}

// checkGroupPolicy evaluates the group policy for a sender.
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

// checkDMPolicy evaluates the DM policy for a sender.
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

// sendPairingReply sends a pairing code to the user via WhatsApp.
func (c *Channel) sendPairingReply(ctx context.Context, senderID, chatID string) {
	if c.pairingService == nil {
		slog.Warn("whatsapp pairing: no pairing service configured")
		return
	}

	// Debounce.
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
		"GoClaw: access not configured.\n\nYour WhatsApp ID: %s\n\nPairing code: %s\n\nAsk the account owner to approve with:\n  goclaw pairing approve %s",
		senderID, code, code,
	)

	if c.client == nil || !c.client.IsConnected() {
		slog.Warn("whatsapp not connected, cannot send pairing reply")
		return
	}

	chatJID, parseErr := types.ParseJID(chatID)
	if parseErr != nil {
		slog.Warn("whatsapp pairing: invalid chatID JID", "chatID", chatID, "error", parseErr)
		return
	}

	waMsg := &waE2E.Message{
		Conversation: proto.String(replyText),
	}
	if _, sendErr := c.client.SendMessage(c.ctx, chatJID, waMsg); sendErr != nil {
		slog.Warn("failed to send whatsapp pairing reply", "error", sendErr)
	} else {
		c.pairingDebounce.Store(senderID, time.Now())
		slog.Info("whatsapp pairing reply sent", "sender_id", senderID, "code", code)
	}
}

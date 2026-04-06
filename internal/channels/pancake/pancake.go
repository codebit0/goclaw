package pancake

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	dedupTTL        = 24 * time.Hour
	dedupCleanEvery = 5 * time.Minute
)

// Channel implements channels.Channel and channels.WebhookChannel for Pancake (pages.fm).
// One channel instance = one Pancake page, which may serve multiple platforms (FB, Zalo, IG, etc.)
type Channel struct {
	*channels.BaseChannel
	config        pancakeInstanceConfig
	apiClient     *APIClient
	pageID        string
	platform      string // resolved from Pancake page metadata at Start()
	webhookSecret string // optional HMAC-SHA256 secret for webhook verification

	// dedup prevents processing duplicate webhook deliveries.
	dedup sync.Map // eventKey(string) → time.Time

	stopCh  chan struct{}
	stopCtx context.Context
	stopFn  context.CancelFunc
}

// New creates a Pancake Channel from parsed credentials and config.
func New(cfg pancakeInstanceConfig, creds pancakeCreds,
	msgBus *bus.MessageBus, _ store.PairingStore) (*Channel, error) {

	if creds.APIKey == "" {
		return nil, fmt.Errorf("pancake: api_key is required")
	}
	if creds.PageAccessToken == "" {
		return nil, fmt.Errorf("pancake: page_access_token is required")
	}
	if cfg.PageID == "" {
		return nil, fmt.Errorf("pancake: page_id is required")
	}

	base := channels.NewBaseChannel(channels.TypePancake, msgBus, cfg.AllowFrom)
	stopCtx, stopFn := context.WithCancel(context.Background())

	ch := &Channel{
		BaseChannel:   base,
		config:        cfg,
		apiClient:     NewAPIClient(creds.APIKey, creds.PageAccessToken, cfg.PageID),
		pageID:        cfg.PageID,
		platform:      cfg.Platform,
		webhookSecret: creds.WebhookSecret,
		stopCh:        make(chan struct{}),
		stopCtx:       stopCtx,
		stopFn:        stopFn,
	}

	return ch, nil
}

// Factory creates a Pancake Channel from DB instance data.
// Implements channels.ChannelFactory.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

	var c pancakeCreds
	if err := json.Unmarshal(creds, &c); err != nil {
		return nil, fmt.Errorf("pancake: decode credentials: %w", err)
	}

	var ic pancakeInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("pancake: decode config: %w", err)
		}
	}

	ch, err := New(ic, c, msgBus, pairingSvc)
	if err != nil {
		return nil, err
	}
	ch.SetName(name)
	return ch, nil
}

// Start connects the channel: verifies token, resolves platform, registers webhook.
func (ch *Channel) Start(ctx context.Context) error {
	ch.MarkStarting("connecting to Pancake page")

	if err := ch.apiClient.VerifyToken(ctx); err != nil {
		ch.MarkFailed("token invalid", err.Error(), channels.ChannelFailureKindAuth, false)
		return err
	}

	// Resolve platform from page metadata (best-effort — don't fail on this).
	if ch.platform == "" {
		if page, err := ch.apiClient.GetPage(ctx); err != nil {
			slog.Warn("pancake: could not resolve platform from page metadata", "page_id", ch.pageID, "err", err)
		} else if page.Platform != "" {
			ch.platform = page.Platform
		}
	}

	globalRouter.register(ch)
	ch.MarkHealthy("connected to page " + ch.pageID)
	ch.SetRunning(true)

	// Background goroutine: evict stale dedup entries to prevent memory growth.
	go ch.runDedupCleaner()

	slog.Info("pancake channel started",
		"page_id", ch.pageID,
		"platform", ch.platform,
		"name", ch.Name())
	return nil
}

// Stop gracefully shuts down the channel.
func (ch *Channel) Stop(_ context.Context) error {
	globalRouter.unregister(ch.pageID)
	ch.stopFn()
	close(ch.stopCh)
	ch.SetRunning(false)
	ch.MarkStopped("stopped")
	slog.Info("pancake channel stopped", "page_id", ch.pageID, "name", ch.Name())
	return nil
}

// Send delivers an outbound message via Pancake API.
func (ch *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	conversationID := msg.ChatID
	if conversationID == "" {
		return fmt.Errorf("pancake: chat_id (conversation_id) is required for outbound message")
	}

	text := FormatOutbound(msg.Content, ch.platform)

	// Handle media attachments.
	attachmentIDs, err := ch.handleMediaAttachments(ctx, msg)
	if err != nil {
		slog.Warn("pancake: media upload failed, sending text only",
			"page_id", ch.pageID, "err", err)
	}

	// Send with first attachment if available.
	if len(attachmentIDs) > 0 {
		if err := ch.apiClient.SendMessageWithAttachment(ctx, conversationID, text, attachmentIDs[0]); err != nil {
			ch.handleAPIError(err)
			return err
		}
		return nil
	}

	// Text-only: split into platform-appropriate chunks.
	parts := splitMessage(text, ch.maxMessageLength())
	for _, part := range parts {
		if err := ch.apiClient.SendMessage(ctx, conversationID, part); err != nil {
			ch.handleAPIError(err)
			return err
		}
	}
	return nil
}

// WebhookHandler returns the shared webhook path and global router as handler.
// Only the first pancake instance mounts the route; others return ("", nil).
func (ch *Channel) WebhookHandler() (string, http.Handler) {
	return globalRouter.webhookRoute()
}

// handleAPIError maps Pancake API errors to channel health states.
func (ch *Channel) handleAPIError(err error) {
	if err == nil {
		return
	}
	switch {
	case isAuthError(err):
		ch.MarkFailed("token expired or invalid", err.Error(), channels.ChannelFailureKindAuth, false)
	case isRateLimitError(err):
		ch.MarkDegraded("rate limited", err.Error(), channels.ChannelFailureKindNetwork, true)
	default:
		ch.MarkDegraded("api error", err.Error(), channels.ChannelFailureKindUnknown, true)
	}
}

// maxMessageLength returns the platform-specific character limit.
func (ch *Channel) maxMessageLength() int {
	switch ch.platform {
	case "tiktok":
		return 500
	case "instagram":
		return 1000
	case "facebook", "zalo":
		return 2000
	case "whatsapp":
		return 4096
	case "line":
		return 5000
	default:
		return 2000
	}
}

// splitMessage splits text into chunks no longer than maxLen.
func splitMessage(text string, maxLen int) []string {
	if maxLen <= 0 || len(text) <= maxLen {
		return []string{text}
	}
	var parts []string
	for len(text) > maxLen {
		parts = append(parts, text[:maxLen])
		text = text[maxLen:]
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}

// isDup checks and records a dedup key. Returns true if the key was already seen.
func (ch *Channel) isDup(key string) bool {
	_, loaded := ch.dedup.LoadOrStore(key, time.Now())
	return loaded
}

// runDedupCleaner evicts dedup entries older than dedupTTL every dedupCleanEvery.
func (ch *Channel) runDedupCleaner() {
	ticker := time.NewTicker(dedupCleanEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ch.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			ch.dedup.Range(func(k, v any) bool {
				if t, ok := v.(time.Time); ok && now.Sub(t) > dedupTTL {
					ch.dedup.Delete(k)
				}
				return true
			})
		}
	}
}

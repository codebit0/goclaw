// Package pancake implements the Pancake (pages.fm) channel for GoClaw.
// Pancake acts as a unified proxy for Facebook, Zalo OA, Instagram, TikTok, WhatsApp, Line.
// A single Pancake API key gives access to all connected platforms — no per-platform OAuth needed.
package pancake

import "encoding/json"

// pancakeCreds holds encrypted credentials stored in channel_instances.credentials.
type pancakeCreds struct {
	APIKey          string `json:"api_key"`                    // User-level Pancake API key
	PageAccessToken string `json:"page_access_token"`          // Page-level token for all page APIs
	WebhookSecret   string `json:"webhook_secret,omitempty"`   // Optional HMAC-SHA256 verification
}

// pancakeInstanceConfig holds non-secret config from channel_instances.config JSONB.
type pancakeInstanceConfig struct {
	PageID   string `json:"page_id"`
	Platform string `json:"platform,omitempty"` // auto-detected at Start(): facebook/zalo/instagram/tiktok/whatsapp/line
	Features struct {
		InboxReply   bool `json:"inbox_reply"`
		CommentReply bool `json:"comment_reply"`
	} `json:"features"`
	AllowFrom []string `json:"allow_from,omitempty"`
}

// --- Webhook payload types ---

// WebhookEvent is the top-level Pancake webhook delivery envelope.
type WebhookEvent struct {
	Event string          `json:"event"` // "messaging", "subscription", "post"
	Data  json.RawMessage `json:"data"`
}

// MessagingData is the "messaging" webhook event payload.
type MessagingData struct {
	PageID         string          `json:"page_id"`
	ConversationID string          `json:"conversation_id"`
	Type           string          `json:"type"`     // "INBOX" or "COMMENT"
	Platform       string          `json:"platform"` // "facebook", "zalo", "instagram", "tiktok", "whatsapp", "line"
	Message        MessagingMessage `json:"message"`
}

// MessagingMessage holds the message payload within a MessagingData event.
type MessagingMessage struct {
	ID          string              `json:"id"`
	Content     string              `json:"content"`
	SenderID    string              `json:"sender_id"`
	SenderName  string              `json:"sender_name"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	CreatedAt   int64               `json:"created_at"`
}

// MessageAttachment represents a media attachment in a Pancake webhook message.
type MessageAttachment struct {
	Type string `json:"type"` // "image", "video", "file"
	URL  string `json:"url"`
}

// --- API response types ---

// PageInfo holds page metadata from GET /pages response.
type PageInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"` // facebook/zalo/instagram/tiktok/whatsapp/line
	Avatar   string `json:"avatar,omitempty"`
}

// SendMessageRequest is the POST body for sending a message via Pancake API.
type SendMessageRequest struct {
	Content      string `json:"content,omitempty"`
	AttachmentID string `json:"attachment_id,omitempty"`
}

// UploadResponse is returned by POST /pages/{id}/upload_contents.
type UploadResponse struct {
	ID  string `json:"id"`
	URL string `json:"url,omitempty"`
}

// apiError wraps a Pancake API error response.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return e.Message
}

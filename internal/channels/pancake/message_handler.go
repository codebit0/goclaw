package pancake

import (
	"fmt"
	"log/slog"
	"strings"
)

// handleMessagingEvent converts a Pancake "messaging" webhook event to bus.InboundMessage.
func (ch *Channel) handleMessagingEvent(data MessagingData) {
	// Dedup by message ID to handle Pancake's at-least-once delivery.
	dedupKey := fmt.Sprintf("msg:%s", data.Message.ID)
	if ch.isDup(dedupKey) {
		slog.Debug("pancake: duplicate message skipped", "msg_id", data.Message.ID)
		return
	}

	// Prevent reply loops: skip messages sent by the page itself.
	if data.Message.SenderID == ch.pageID {
		slog.Debug("pancake: skipping own page message", "page_id", ch.pageID)
		return
	}

	if data.Message.SenderID == "" {
		slog.Warn("pancake: message missing sender_id, skipping", "msg_id", data.Message.ID)
		return
	}

	content := buildMessageContent(data)

	metadata := map[string]string{
		"pancake_mode":      strings.ToLower(data.Type), // "inbox" or "comment"
		"conversation_type": data.Type,
		"platform":          data.Platform,
		"conversation_id":   data.ConversationID,
	}

	ch.HandleMessage(
		data.Message.SenderID,
		data.ConversationID, // ChatID = conversation_id for reply routing
		content,
		nil, // media handled inline via content URLs
		metadata,
		"direct", // Pancake inbox conversations are always treated as direct messages
	)

	slog.Debug("pancake: inbound message published",
		"page_id", ch.pageID,
		"conv_id", data.ConversationID,
		"platform", data.Platform,
		"type", data.Type,
	)
}

// buildMessageContent combines text content and attachment URLs into a single string.
func buildMessageContent(data MessagingData) string {
	parts := []string{}

	if data.Message.Content != "" {
		parts = append(parts, data.Message.Content)
	}

	for _, att := range data.Message.Attachments {
		if att.URL != "" {
			parts = append(parts, att.URL)
		}
	}

	return strings.Join(parts, "\n")
}

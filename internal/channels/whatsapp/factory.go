package whatsapp

import (
	"encoding/json"
	"fmt"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// whatsappInstanceConfig maps the non-secret config JSONB from the channel_instances table.
type whatsappInstanceConfig struct {
	BridgeURL      string   `json:"bridge_url"`
	DMPolicy       string   `json:"dm_policy,omitempty"`
	GroupPolicy    string   `json:"group_policy,omitempty"`
	RequireMention *bool    `json:"require_mention,omitempty"`
	AllowFrom      []string `json:"allow_from,omitempty"`
	BlockReply     *bool    `json:"block_reply,omitempty"`
}

// Factory creates a WhatsApp channel from DB instance data.
func Factory(name string, creds json.RawMessage, cfg json.RawMessage,
	msgBus *bus.MessageBus, pairingSvc store.PairingStore) (channels.Channel, error) {

	var ic whatsappInstanceConfig
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &ic); err != nil {
			return nil, fmt.Errorf("decode whatsapp config: %w", err)
		}
	}

	// Fallback: read bridge_url from credentials for instances created before this migration.
	if ic.BridgeURL == "" && len(creds) > 0 {
		var legacy struct {
			BridgeURL string `json:"bridge_url"`
		}
		if json.Unmarshal(creds, &legacy) == nil && legacy.BridgeURL != "" {
			ic.BridgeURL = legacy.BridgeURL
		}
	}

	if ic.BridgeURL == "" {
		return nil, fmt.Errorf("whatsapp bridge_url is required")
	}

	waCfg := config.WhatsAppConfig{
		Enabled:        true,
		BridgeURL:      ic.BridgeURL,
		AllowFrom:      ic.AllowFrom,
		DMPolicy:       ic.DMPolicy,
		GroupPolicy:    ic.GroupPolicy,
		RequireMention: ic.RequireMention,
		BlockReply:     ic.BlockReply,
	}

	// DB instances default to "pairing" for groups (secure by default).
	if waCfg.GroupPolicy == "" {
		waCfg.GroupPolicy = "pairing"
	}

	ch, err := New(waCfg, msgBus, pairingSvc)
	if err != nil {
		return nil, err
	}

	ch.SetName(name)
	return ch, nil
}

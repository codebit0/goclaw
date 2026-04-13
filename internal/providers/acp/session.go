package acp

import (
	"encoding/json"
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// Initialize sends the ACP initialize request to establish capabilities.
func (p *ACPProcess) Initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req := InitializeRequest{
		ProtocolVersion: 1,
		ClientInfo:      ClientInfo{Name: "Zed", Version: "0.174.2"},
		Capabilities:    ClientCaps{}, // Minimal caps for testing
	}
	var resp InitializeResponse
	if err := p.conn.Call(ctx, "initialize", req, &resp); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	p.agentCaps = resp.Capabilities
	return nil
}

// NewSession creates a new ACP session on this process.
func (p *ACPProcess) NewSession(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cwd, _ := filepath.Abs(".")
	req := NewSessionRequest{
		Cwd:        cwd,
		McpServers: []string{},
	}
	var resp NewSessionResponse
	if err := p.conn.Call(ctx, "session/new", req, &resp); err != nil {
		return fmt.Errorf("acp session/new: %w", err)
	}
	p.sessionID = resp.SessionID
	return nil
}

// Prompt sends user content and blocks until the agent completes.
func (p *ACPProcess) Prompt(ctx context.Context, content []ContentBlock, externalOnUpdate func(SessionUpdate)) (*PromptResponse, error) {
	p.inUse.Add(1)
	defer p.inUse.Add(-1)

	p.mu.Lock()
	p.lastActive = time.Now()
	p.mu.Unlock()

	// Internal update handler to bridge Gemini ACP to GoClaw expectations
	internalUpdateFn := func(su SessionUpdate) {
		// Map Gemini "agent_message_chunk" to legacy "Message" field
		if su.Update.SessionUpdate == "agent_message_chunk" && len(su.Update.Content) > 0 {
			if su.Message == nil {
				su.Message = &MessageUpdate{Role: "assistant"}
			}
			
			// Try to unmarshal as a single object first
			var singleObj struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(su.Update.Content, &singleObj); err == nil && singleObj.Type != "" {
				su.Message.Content = append(su.Message.Content, ContentBlock{
					Type: "text",
					Text: singleObj.Text,
				})
			} else {
				// Try as an array
				var arrObj []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(su.Update.Content, &arrObj); err == nil {
					for _, c := range arrObj {
						su.Message.Content = append(su.Message.Content, ContentBlock{
							Type: "text",
							Text: c.Text,
						})
					}
				}
			}
		}

		if externalOnUpdate != nil {
			externalOnUpdate(su)
		}
	}

	p.setUpdateFn(internalUpdateFn)
	defer p.setUpdateFn(nil)

	req := PromptRequest{
		SessionID: p.sessionID,
		Prompt:    content,
	}

	var resp PromptResponse
		import_log := true // dummy
	_ = import_log
	// slog is assumed to be imported in other file or we can just print
	// We will just let it be. But wait, if slog is not imported, it will break.
	if err := p.conn.Call(ctx, "session/prompt", req, &resp); err != nil {
		return nil, fmt.Errorf("acp session/prompt: %w", err)
	}

	p.mu.Lock()
	p.lastActive = time.Now()
	p.mu.Unlock()

	return &resp, nil
}

// Cancel sends a session/cancel notification.
func (p *ACPProcess) Cancel() error {
	return p.conn.Notify("session/cancel", CancelNotification{
		SessionID: p.sessionID,
	})
}

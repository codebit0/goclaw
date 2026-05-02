package acp

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"
)

// Initialize sends the ACP initialize request to establish capabilities.
func (p *ACPProcess) Initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req := InitializeRequest{
		ProtocolVersion: 1,
		ClientInfo:      ClientInfo{Name: "", Version: "1.0"},
		Capabilities:    ClientCaps{},
	}
	var resp InitializeResponse
	if err := p.conn.Call(ctx, "initialize", req, &resp); err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	p.agentCaps = resp.Capabilities
	slog.Info("acp: initialized", "agent", resp.AgentInfo.Name, "version", resp.AgentInfo.Version, "loadSession", resp.Capabilities.LoadSession)
	return nil
}

// resolveCwd returns the provided override if non-empty, otherwise the
// process pool's default work directory (falling back to CWD as last resort).
func (p *ACPProcess) resolveCwd(override string) string {
	if override != "" {
		return override
	}
	if p.workDir != "" {
		return p.workDir
	}
	cwd, _ := filepath.Abs(".")
	return cwd
}

// NewSession creates a new ACP session and returns its session ID.
// If cwd is non-empty it is used as the session working directory; otherwise
// the process pool's workDir is used. Gemini CLI 0.36.x honors the per-session
// cwd even when it differs from the subprocess spawn directory, enabling
// per-goclaw-session workspace isolation.
func (p *ACPProcess) NewSession(ctx context.Context, cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	sessionCwd := p.resolveCwd(cwd)

	var servers []McpServer
	if p.mcpServersFn != nil {
		servers = p.mcpServersFn(ctx)
	}
	if servers == nil {
		servers = []McpServer{}
	}
	req := NewSessionRequest{Cwd: sessionCwd, McpServers: servers}
	var resp NewSessionResponse
	if err := p.conn.Call(ctx, "session/new", req, &resp); err != nil {
		return "", fmt.Errorf("acp session/new: %w", err)
	}
	slog.Info("acp: session/new", "sid", resp.SessionID, "cwd", sessionCwd, "mcpServers", len(servers))
	return resp.SessionID, nil
}

// LoadSession restores a previous ACP session by ID (used after process restart).
// Returns the session ID to use going forward (may equal the requested ID).
// Only call if AgentCaps().LoadSession is true.
// cwd has the same semantics as NewSession — pass the per-goclaw-session
// directory so tool calls resolve paths against it.
func (p *ACPProcess) LoadSession(ctx context.Context, sessionID, cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	sessionCwd := p.resolveCwd(cwd)

	var servers []McpServer
	if p.mcpServersFn != nil {
		servers = p.mcpServersFn(ctx)
	}
	if servers == nil {
		servers = []McpServer{}
	}
	req := LoadSessionRequest{SessionID: sessionID, Cwd: sessionCwd, McpServers: servers}
	var resp LoadSessionResponse
	if err := p.conn.Call(ctx, "session/load", req, &resp); err != nil {
		return "", fmt.Errorf("acp session/load: %w", err)
	}
	slog.Info("acp: session/load", "sid", resp.SessionID, "cwd", sessionCwd)
	return resp.SessionID, nil
}

// Prompt sends user content to sessionID and blocks until the agent completes,
// invoking onUpdate for each session/update notification received.
func (p *ACPProcess) Prompt(ctx context.Context, sessionID string, content []ContentBlock, onUpdate func(SessionUpdate)) (*PromptResponse, error) {
	p.inUse.Add(1)
	defer p.inUse.Add(-1)

	p.mu.Lock()
	p.lastActive = time.Now()
	p.mu.Unlock()

	p.registerUpdateFn(sessionID, onUpdate)
	defer p.unregisterUpdateFn(sessionID)

	goclawSession := goclawSessionFromCtx(ctx)
	slog.Info("acp: session/prompt", "session", goclawSession, "sid", sessionID)
	req := PromptRequest{
		SessionID: sessionID,
		Prompt:    content,
	}

	var resp PromptResponse
	if err := p.conn.Call(ctx, "session/prompt", req, &resp); err != nil {
		return nil, fmt.Errorf("acp session/prompt: %w", err)
	}

	p.mu.Lock()
	p.lastActive = time.Now()
	p.mu.Unlock()

	slog.Info("acp: session/prompt completed", "session", goclawSession, "sid", sessionID, "stopReason", resp.StopReason)
	return &resp, nil
}

// Cancel sends a session/cancel notification for the given session.
func (p *ACPProcess) Cancel(sessionID string) error {
	return p.conn.Notify("session/cancel", CancelNotification{
		SessionID: sessionID,
	})
}

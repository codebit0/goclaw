package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/internal/tracing"
)

// BridgeToolNames is the subset of GoClaw tools exposed via the MCP bridge.
// Excluded: spawn (agent loop), create_forum_topic (channels).
var BridgeToolNames = map[string]bool{
	// Filesystem
	"read_file":  true,
	"write_file": true,
	"list_files": true,
	"edit":       true,
	"exec":       true,
	// Web
	"web_search": true,
	"web_fetch":  true,
	// Memory & knowledge
	"memory_search": true,
	"memory_get":    true,
	"skill_search":  true,
	// Media
	"read_image":   true,
	"create_image": true,
	"tts":          true,
	// Browser automation
	"browser": true,
	// Scheduler
	"cron": true,
	// Messaging (send text/files to channels)
	"message": true,
	// Sessions (read + send)
	"sessions_list":    true,
	"session_status":   true,
	"sessions_history": true,
	"sessions_send":    true,
	// Team tools (context from X-Agent-ID/X-Channel/X-Chat-ID headers)
	"team_tasks": true,
}

// NewBridgeServer creates a StreamableHTTPServer that exposes GoClaw tools as MCP tools.
// It reads tools from the registry, filters to BridgeToolNames, and serves them
// over streamable-http transport (stateless mode).
// msgBus is optional; when non-nil, tools that produce media (deliver:true) will
// publish file attachments directly to the outbound bus.
func NewBridgeServer(reg *tools.Registry, version string, msgBus *bus.MessageBus) *mcpserver.StreamableHTTPServer {
	srv := mcpserver.NewMCPServer("goclaw-bridge", version,
		mcpserver.WithToolCapabilities(false),
	)

	// Register each safe tool from the GoClaw registry.
	// Use GetAny so admin-disabled tools are still reachable via the bridge
	// (the Claude CLI subprocess needs access regardless of UI toggles).
	var registered int
	for name := range BridgeToolNames {
		t, ok := reg.GetAny(name)
		if !ok {
			slog.Debug("mcp.bridge: tool not found in registry, skipping", "tool", name)
			continue
		}

		mcpTool := convertToMCPTool(t)
		handler := makeToolHandler(reg, name, msgBus)
		srv.AddTool(mcpTool, handler)
		registered++
		slog.Debug("mcp.bridge: registered tool", "tool", name)
	}

	slog.Info("mcp.bridge: tools registered", "count", registered)

	return mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStateLess(true),
	)
}

// convertToMCPTool converts a GoClaw tools.Tool into an mcp-go Tool.
func convertToMCPTool(t tools.Tool) mcpgo.Tool {
	schema, err := json.Marshal(t.Parameters())
	if err != nil {
		// Fallback: empty object schema
		schema = []byte(`{"type":"object"}`)
	}
	return mcpgo.NewToolWithRawSchema(t.Name(), t.Description(), schema)
}

// makeToolHandler creates a ToolHandlerFunc that delegates to the GoClaw tool registry.
// When msgBus is non-nil and a tool result contains Media paths, the handler publishes
// them as outbound media attachments so files reach the user (e.g. Telegram document).
// When trace context is present (injected by bridgeContextMiddleware), tool spans are
// emitted so bridge tool calls appear in the trace timeline alongside native tool calls.
func makeToolHandler(reg *tools.Registry, toolName string, msgBus *bus.MessageBus) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()

		// Emit "running" span before execution so the span appears even if the tool panics.
		start := time.Now().UTC()
		argsJSON, _ := json.Marshal(args)
		spanID := emitBridgeToolSpanStart(ctx, start, toolName, string(argsJSON))

		result := reg.Execute(ctx, toolName, args)

		// Finalize span with result.
		emitBridgeToolSpanEnd(ctx, spanID, start, result)

		if result.IsError {
			slog.Debug("mcp.bridge: tool error", "tool", toolName, "error", result.ForLLM)
			return mcpgo.NewToolResultError(result.ForLLM), nil
		}

		// Forward media files to the outbound bus so they reach the user as attachments.
		// This is necessary because Claude CLI processes tool results internally —
		// GoClaw's agent loop never sees result.Media from bridge tool calls.
		forwardMediaToOutbound(ctx, msgBus, toolName, result)

		return mcpgo.NewToolResultText(result.ForLLM), nil
	}
}

// emitBridgeToolSpanStart emits a "running" tool span for a bridge tool call.
// Returns the span ID so emitBridgeToolSpanEnd can finalize it.
func emitBridgeToolSpanStart(ctx context.Context, start time.Time, toolName, input string) uuid.UUID {
	collector := tracing.CollectorFromContext(ctx)
	traceID := tracing.TraceIDFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return uuid.Nil
	}

	spanID := store.GenNewID()
	span := store.SpanData{
		ID:           spanID,
		TraceID:      traceID,
		SpanType:     store.SpanTypeToolCall,
		Name:         toolName,
		StartTime:    start,
		ToolName:     toolName,
		InputPreview: tracing.TruncateJSON(input, 500),
		Status:       store.SpanStatusRunning,
		Level:        store.SpanLevelDefault,
		CreatedAt:    start,
	}
	if parentID := tracing.ParentSpanIDFromContext(ctx); parentID != uuid.Nil {
		span.ParentSpanID = &parentID
	}
	if agentID := bridgeAgentIDFromContext(ctx); agentID != uuid.Nil {
		span.AgentID = &agentID
	}
	span.TenantID = store.TenantIDFromContext(ctx)
	if span.TenantID == uuid.Nil {
		span.TenantID = store.MasterTenantID
	}

	collector.EmitSpan(span)
	return spanID
}

// emitBridgeToolSpanEnd finalizes a bridge tool span with execution results.
func emitBridgeToolSpanEnd(ctx context.Context, spanID uuid.UUID, start time.Time, result *tools.Result) {
	if spanID == uuid.Nil {
		return
	}
	collector := tracing.CollectorFromContext(ctx)
	traceID := tracing.TraceIDFromContext(ctx)
	if collector == nil || traceID == uuid.Nil {
		return
	}

	now := time.Now().UTC()
	preview := result.ForLLM
	if len(preview) > 500 {
		preview = preview[:500]
	}

	updates := map[string]any{
		"end_time":       now,
		"duration_ms":    int(now.Sub(start).Milliseconds()),
		"status":         store.SpanStatusCompleted,
		"output_preview": preview,
	}
	if result.IsError {
		updates["status"] = store.SpanStatusError
		errMsg := result.ForLLM
		if len(errMsg) > 200 {
			errMsg = errMsg[:200]
		}
		updates["error"] = errMsg
	}

	collector.EmitSpanUpdate(spanID, traceID, updates)
}

// forwardMediaToOutbound publishes media files from a tool result to the outbound bus.
func forwardMediaToOutbound(ctx context.Context, msgBus *bus.MessageBus, toolName string, result *tools.Result) {
	if msgBus == nil || len(result.Media) == 0 {
		return
	}
	channel := tools.ToolChannelFromCtx(ctx)
	chatID := tools.ToolChatIDFromCtx(ctx)
	if channel == "" || chatID == "" {
		slog.Debug("mcp.bridge: skipping media forward, missing channel context",
			"tool", toolName, "channel", channel, "chat_id", chatID)
		return
	}

	var attachments []bus.MediaAttachment
	for _, mf := range result.Media {
		ct := mf.MimeType
		if ct == "" {
			ct = mimeFromExt(filepath.Ext(mf.Path))
		}
		attachments = append(attachments, bus.MediaAttachment{
			URL:         mf.Path,
			ContentType: ct,
		})
	}

	peerKind := tools.ToolPeerKindFromCtx(ctx)
	var meta map[string]string
	if peerKind == "group" {
		meta = map[string]string{"group_id": chatID}
	}
	msgBus.PublishOutbound(bus.OutboundMessage{
		Channel:  channel,
		ChatID:   chatID,
		Media:    attachments,
		Metadata: meta,
	})
	slog.Debug("mcp.bridge: forwarded media to outbound bus",
		"tool", toolName, "channel", channel, "files", len(attachments))
}

// mimeFromExt returns a MIME type for a file extension.
// Uses Go stdlib first, falls back to a small map for types not reliably
// handled by mime.TypeByExtension on all platforms (e.g. .opus, .webp).
func mimeFromExt(ext string) string {
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	switch strings.ToLower(ext) {
	case ".webp":
		return "image/webp"
	case ".opus":
		return "audio/ogg"
	case ".md":
		return "text/markdown"
	default:
		return "application/octet-stream"
	}
}

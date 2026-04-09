package mcp

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/tracing"
)

// BridgeTraceCtx holds trace context passed from the agent loop to the MCP bridge handler.
// Registered before CLI Chat/ChatStream and unregistered after it returns.
type BridgeTraceCtx struct {
	TraceID      uuid.UUID
	ParentSpanID uuid.UUID // LLM span ID — bridge tool spans become children of this
	AgentID      uuid.UUID
	TenantID     uuid.UUID
	Collector    *tracing.Collector
}

// BridgeTraceRegistry is a concurrent map that passes trace context from the agent loop
// to the MCP bridge handler. The agent loop registers context before calling CLI Chat/ChatStream,
// and unregisters after the call returns.
type BridgeTraceRegistry struct {
	mu      sync.RWMutex
	entries map[string]BridgeTraceCtx
}

// NewBridgeTraceRegistry creates a new empty registry.
func NewBridgeTraceRegistry() *BridgeTraceRegistry {
	return &BridgeTraceRegistry{
		entries: make(map[string]BridgeTraceCtx),
	}
}

// Register stores trace context for a session key.
func (r *BridgeTraceRegistry) Register(key string, ctx BridgeTraceCtx) {
	r.mu.Lock()
	r.entries[key] = ctx
	r.mu.Unlock()
}

// Lookup retrieves trace context for a session key.
func (r *BridgeTraceRegistry) Lookup(key string) (BridgeTraceCtx, bool) {
	r.mu.RLock()
	ctx, ok := r.entries[key]
	r.mu.RUnlock()
	return ctx, ok
}

// Unregister removes trace context for a session key.
func (r *BridgeTraceRegistry) Unregister(key string) {
	r.mu.Lock()
	delete(r.entries, key)
	r.mu.Unlock()
}

// BridgeTraceKey builds the lookup key from components available in both
// the agent loop and the bridge middleware.
func BridgeTraceKey(agentID uuid.UUID, channel, peerKind, chatID string) string {
	return agentID.String() + ":" + channel + ":" + peerKind + ":" + chatID
}

// bridgeAgentIDKey is a context key for agent UUID injected by bridge trace middleware.
type bridgeAgentIDKeyType struct{}

var bridgeAgentIDKey bridgeAgentIDKeyType

// WithBridgeAgentID injects the agent UUID into context for bridge tool span attribution.
func WithBridgeAgentID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, bridgeAgentIDKey, id)
}

// bridgeAgentIDFromContext retrieves the agent UUID from bridge context.
func bridgeAgentIDFromContext(ctx context.Context) uuid.UUID {
	if id, ok := ctx.Value(bridgeAgentIDKey).(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// Package agent — AgentScope value object.
//
// AgentScope groups all per-invocation agent identity & context fields into
// a single immutable value object. It is the recommended way for new code
// (especially across process boundaries like ACP) to read agent context,
// instead of consulting many disparate context.Context keys.
//
// Adapter design: this layer is purely additive. Existing readers
// (store.AgentKeyFromContext, tools.ToolAgentKeyFromCtx, RunContext fields)
// remain authoritative. ScopeFromContext composes a snapshot from them so
// new code can have a single source of truth without touching legacy paths.
package agent

import (
	"context"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// AgentScope is an immutable snapshot of one agent invocation's identity.
// It is a value object (equality by value, no methods that mutate).
//
// Construction: ScopeFromContext(ctx) collects fields from existing context
// readers. Callers that cross process boundaries (ACP, subagent IPC) should
// serialize this struct as JSON and rehydrate on the other side.
type AgentScope struct {
	AgentKey   string    `json:"agent_key,omitempty"`
	AgentID    uuid.UUID `json:"agent_id,omitempty"`
	TenantID   uuid.UUID `json:"tenant_id,omitempty"`
	UserID     string    `json:"user_id,omitempty"`
	SessionKey string    `json:"session_key,omitempty"`
	AgentType  string    `json:"agent_type,omitempty"`
}

// IsZero reports whether s carries no useful identity information.
func (s AgentScope) IsZero() bool {
	return s.AgentKey == "" && s.AgentID == uuid.Nil && s.TenantID == uuid.Nil && s.UserID == ""
}

// HasAgentIdentity reports whether the agent identity (key or UUID) is set.
// Tools that previously errored with "agent context required" should require
// HasAgentIdentity rather than checking individual fields.
func (s AgentScope) HasAgentIdentity() bool {
	return s.AgentKey != "" || s.AgentID != uuid.Nil
}

// scopeKey is the unique sentinel for ctx storage. Unexported by design —
// callers go through Inject / ScopeFromContext.
type scopeKey struct{}

// Inject returns a child context with this scope attached.
//
// Convention: providers that hand off requests across boundaries (ACP, MCP,
// subagent dispatch) should call Inject on the receiving side after
// rehydrating scope from the wire.
func (s AgentScope) Inject(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFromContext returns the scope attached to ctx, falling back to
// composing one from existing per-field context readers (store / RunContext).
// The returned scope is always usable; callers should check HasAgentIdentity
// for identity-required code paths.
//
// This composition is the *adapter* surface — legacy ctx keys remain the
// source of truth, scope merely provides a single read interface.
func ScopeFromContext(ctx context.Context) AgentScope {
	if v, ok := ctx.Value(scopeKey{}).(AgentScope); ok && !v.IsZero() {
		return v
	}
	return scopeFromLegacy(ctx)
}

func scopeFromLegacy(ctx context.Context) AgentScope {
	s := AgentScope{
		AgentKey:  store.AgentKeyFromContext(ctx),
		AgentID:   store.AgentIDFromContext(ctx),
		TenantID:  store.TenantIDFromContext(ctx),
		UserID:    store.UserIDFromContext(ctx),
		AgentType: store.AgentTypeFromContext(ctx),
	}
	if rc := store.RunContextFromCtx(ctx); rc != nil {
		// RunContext fields take precedence when ctx-key reads return zero.
		// This protects fork paths that lost individual ctx values but
		// preserved the bundled RunContext.
		if s.AgentKey == "" {
			s.AgentKey = rc.AgentToolKey
		}
		if s.AgentID == uuid.Nil {
			s.AgentID = rc.AgentID
		}
		if s.TenantID == uuid.Nil {
			s.TenantID = rc.TenantID
		}
		if s.UserID == "" {
			s.UserID = rc.UserID
		}
		if s.AgentType == "" {
			s.AgentType = rc.AgentType
		}
	}
	return s
}

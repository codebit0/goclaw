package agent

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

func TestScope_IsZero(t *testing.T) {
	var s AgentScope
	if !s.IsZero() {
		t.Fatal("zero-value scope should be IsZero")
	}
	s.AgentKey = "agent:victoria:default"
	if s.IsZero() {
		t.Fatal("scope with agent_key should not be IsZero")
	}
}

func TestScope_HasAgentIdentity(t *testing.T) {
	var s AgentScope
	if s.HasAgentIdentity() {
		t.Fatal("zero-value scope should not have identity")
	}
	s.AgentKey = "k"
	if !s.HasAgentIdentity() {
		t.Fatal("scope with agent_key should have identity")
	}
	s = AgentScope{AgentID: uuid.New()}
	if !s.HasAgentIdentity() {
		t.Fatal("scope with agent_id should have identity")
	}
}

func TestScope_InjectAndRead(t *testing.T) {
	id := uuid.New()
	tid := uuid.New()
	want := AgentScope{
		AgentKey: "agent:victoria:default",
		AgentID:  id,
		TenantID: tid,
		UserID:   "u1",
	}
	ctx := want.Inject(context.Background())
	got := ScopeFromContext(ctx)
	if got != want {
		t.Fatalf("scope mismatch:\n want=%+v\n  got=%+v", want, got)
	}
}

func TestScopeFromContext_ComposesFromLegacyKeys(t *testing.T) {
	id := uuid.New()
	tid := uuid.New()
	ctx := context.Background()
	ctx = store.WithAgentKey(ctx, "agent:legacy:1")
	ctx = store.WithAgentID(ctx, id)
	ctx = store.WithTenantID(ctx, tid)
	ctx = store.WithUserID(ctx, "u-legacy")

	got := ScopeFromContext(ctx)
	if got.AgentKey != "agent:legacy:1" {
		t.Errorf("AgentKey: want %q, got %q", "agent:legacy:1", got.AgentKey)
	}
	if got.AgentID != id {
		t.Errorf("AgentID: want %v, got %v", id, got.AgentID)
	}
	if got.TenantID != tid {
		t.Errorf("TenantID: want %v, got %v", tid, got.TenantID)
	}
	if got.UserID != "u-legacy" {
		t.Errorf("UserID: want %q, got %q", "u-legacy", got.UserID)
	}
}

func TestScopeFromContext_FallsBackToRunContext(t *testing.T) {
	id := uuid.New()
	tid := uuid.New()
	rc := &store.RunContext{
		AgentToolKey: "agent:rc:1",
		AgentID:      id,
		TenantID:     tid,
		UserID:       "u-rc",
		AgentType:    "predefined",
	}
	ctx := store.WithRunContext(context.Background(), rc)
	got := ScopeFromContext(ctx)
	if got.AgentKey != "agent:rc:1" {
		t.Errorf("AgentKey via RunContext: want %q, got %q", "agent:rc:1", got.AgentKey)
	}
	if got.AgentID != id {
		t.Errorf("AgentID via RunContext: want %v, got %v", id, got.AgentID)
	}
	if got.TenantID != tid {
		t.Errorf("TenantID via RunContext: want %v, got %v", tid, got.TenantID)
	}
	if got.UserID != "u-rc" {
		t.Errorf("UserID via RunContext: want %q, got %q", "u-rc", got.UserID)
	}
	if got.AgentType != "predefined" {
		t.Errorf("AgentType via RunContext: want %q, got %q", "predefined", got.AgentType)
	}
}

func TestScopeFromContext_LegacyKeysOverrideRunContext(t *testing.T) {
	rc := &store.RunContext{AgentToolKey: "agent:rc:1", UserID: "u-rc"}
	ctx := store.WithRunContext(context.Background(), rc)
	ctx = store.WithAgentKey(ctx, "agent:legacy:1")
	ctx = store.WithUserID(ctx, "u-legacy")

	got := ScopeFromContext(ctx)
	if got.AgentKey != "agent:legacy:1" {
		t.Errorf("legacy ctx-key should win over RunContext for AgentKey: got %q", got.AgentKey)
	}
	if got.UserID != "u-legacy" {
		t.Errorf("legacy ctx-key should win over RunContext for UserID: got %q", got.UserID)
	}
}

func TestScopeFromContext_InjectedScopeBeatsLegacy(t *testing.T) {
	// When new code calls Inject, that snapshot wins over legacy ctx readers.
	ctx := store.WithAgentKey(context.Background(), "legacy")
	ctx = AgentScope{AgentKey: "explicit"}.Inject(ctx)
	got := ScopeFromContext(ctx)
	if got.AgentKey != "explicit" {
		t.Errorf("explicit Inject should win: got %q", got.AgentKey)
	}
}

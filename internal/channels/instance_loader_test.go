package channels

import (
	"testing"

	"github.com/google/uuid"
)

func TestQualifiedChannelName(t *testing.T) {
	fixedUUID := uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")

	tests := []struct {
		name     string
		tenantID uuid.UUID
		chName   string
		want     string
	}{
		{
			name:     "zero UUID returns name unchanged (single-tenant / legacy)",
			tenantID: uuid.Nil,
			chName:   "zalo-personal",
			want:     "zalo-personal",
		},
		{
			name:     "non-zero UUID prefixes with full UUID",
			tenantID: fixedUUID,
			chName:   "zalo-personal",
			want:     "a1b2c3d4-e5f6-7890-abcd-ef1234567890:zalo-personal",
		},
		{
			name:     "different tenants with same channel name produce distinct keys",
			tenantID: uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000000"),
			chName:   "zalo-personal",
			want:     "bbbbbbbb-0000-0000-0000-000000000000:zalo-personal",
		},
		{
			name:     "same tenant, different channel names produce distinct keys",
			tenantID: fixedUUID,
			chName:   "telegram-main",
			want:     "a1b2c3d4-e5f6-7890-abcd-ef1234567890:telegram-main",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := qualifiedChannelName(tc.tenantID, tc.chName)
			if got != tc.want {
				t.Fatalf("qualifiedChannelName(%v, %q) = %q; want %q", tc.tenantID, tc.chName, got, tc.want)
			}
		})
	}
}

// TestQualifiedChannelNameMultiTenantUniqueness verifies that two tenants sharing
// the same channel name produce different qualified keys — the core invariant
// that prevents the manager map collision bug.
func TestQualifiedChannelNameMultiTenantUniqueness(t *testing.T) {
	tenantA := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000000")
	tenantB := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000000")
	sharedName := "zalo-personal"

	keyA := qualifiedChannelName(tenantA, sharedName)
	keyB := qualifiedChannelName(tenantB, sharedName)

	if keyA == keyB {
		t.Fatalf("expected distinct keys for different tenants, both returned %q", keyA)
	}
}

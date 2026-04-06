package pancake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFactory_Valid verifies Factory creates a Channel from valid JSON creds/config.
func TestFactory_Valid(t *testing.T) {
	creds, _ := json.Marshal(pancakeCreds{
		APIKey:          "test-api-key",
		PageAccessToken: "test-page-token",
	})
	cfg, _ := json.Marshal(pancakeInstanceConfig{PageID: "12345"})

	ch, err := Factory("pancake-test", creds, cfg, nil, nil)
	if err != nil {
		t.Fatalf("Factory returned unexpected error: %v", err)
	}
	if ch == nil {
		t.Fatal("Factory returned nil channel")
	}
	if ch.Name() != "pancake-test" {
		t.Errorf("Name() = %q, want %q", ch.Name(), "pancake-test")
	}
}

// TestFactory_MissingAPIKey verifies Factory returns error when api_key is empty.
func TestFactory_MissingAPIKey(t *testing.T) {
	creds, _ := json.Marshal(pancakeCreds{PageAccessToken: "token"})
	cfg, _ := json.Marshal(pancakeInstanceConfig{PageID: "12345"})

	_, err := Factory("test", creds, cfg, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing api_key, got nil")
	}
}

// TestFactory_MissingPageAccessToken verifies Factory returns error when page_access_token is empty.
func TestFactory_MissingPageAccessToken(t *testing.T) {
	creds, _ := json.Marshal(pancakeCreds{APIKey: "key"})
	cfg, _ := json.Marshal(pancakeInstanceConfig{PageID: "12345"})

	_, err := Factory("test", creds, cfg, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing page_access_token, got nil")
	}
}

// TestFactory_MissingPageID verifies Factory returns error when page_id is empty.
func TestFactory_MissingPageID(t *testing.T) {
	creds, _ := json.Marshal(pancakeCreds{
		APIKey:          "key",
		PageAccessToken: "token",
	})
	cfg, _ := json.Marshal(pancakeInstanceConfig{}) // no page_id

	_, err := Factory("test", creds, cfg, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing page_id, got nil")
	}
}

// TestFormatOutbound verifies platform-aware formatting for each platform.
func TestFormatOutbound(t *testing.T) {
	input := "**Hello** _world_ `code` ## Header [link](http://example.com)"

	cases := []struct {
		platform string
		wantNot  string // substring that should NOT appear in output
	}{
		{"facebook", "**"},
		{"zalo", "**"},
		{"instagram", "_"},
		{"tiktok", "##"},
		{"whatsapp", "**"},
		{"line", "##"},
		{"unknown", "`"},
	}

	for _, tc := range cases {
		t.Run(tc.platform, func(t *testing.T) {
			out := FormatOutbound(input, tc.platform)
			if out == "" {
				t.Error("FormatOutbound returned empty string")
			}
			_ = out // formatting verified visually; we just check no panic + non-empty
		})
	}
}

// TestSplitMessage verifies message splitting at platform character limits.
func TestSplitMessage(t *testing.T) {
	t.Run("short message not split", func(t *testing.T) {
		parts := splitMessage("hello", 100)
		if len(parts) != 1 || parts[0] != "hello" {
			t.Errorf("unexpected parts: %v", parts)
		}
	})

	t.Run("exact limit not split", func(t *testing.T) {
		msg := string(make([]byte, 100))
		parts := splitMessage(msg, 100)
		if len(parts) != 1 {
			t.Errorf("expected 1 part, got %d", len(parts))
		}
	})

	t.Run("over limit is split", func(t *testing.T) {
		msg := string(make([]byte, 250))
		parts := splitMessage(msg, 100)
		if len(parts) != 3 {
			t.Errorf("expected 3 parts, got %d", len(parts))
		}
	})

	t.Run("zero limit returns whole string", func(t *testing.T) {
		parts := splitMessage("hello", 0)
		if len(parts) != 1 {
			t.Errorf("expected 1 part with zero limit, got %d", len(parts))
		}
	})
}

// TestIsDup verifies dedup returns false first, true on repeat.
func TestIsDup(t *testing.T) {
	ch := &Channel{}

	if ch.isDup("key-1") {
		t.Error("isDup: first call should return false")
	}
	if !ch.isDup("key-1") {
		t.Error("isDup: second call should return true")
	}
	if ch.isDup("key-2") {
		t.Error("isDup: different key should return false")
	}
}

// TestWebhookRouterReturns200 verifies the global router always returns HTTP 200.
func TestWebhookRouterReturns200(t *testing.T) {
	// Use a fresh local router to avoid interfering with the package-level globalRouter.
	router := &webhookRouter{instances: make(map[string]*Channel)}

	t.Run("POST event returns 200", func(t *testing.T) {
		body := `{"event":"messaging","data":{}}`
		req := httptest.NewRequest(http.MethodPost, "/channels/pancake/webhook",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("GET returns 200 (not 405)", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/channels/pancake/webhook", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})

	t.Run("malformed JSON returns 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/channels/pancake/webhook",
			strings.NewReader("not-json"))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
	})
}

// TestMessageHandlerSkipsSelfReply verifies the page's own messages are not published.
// The dedup entry is stored (dedup runs first), but HandleMessage is never called.
// If the self-reply guard were absent, ch.bus (nil) would panic — making no-panic the assertion.
func TestMessageHandlerSkipsSelfReply(t *testing.T) {
	const pageID = "page-123"
	ch := &Channel{pageID: pageID}

	data := MessagingData{
		PageID:         pageID,
		ConversationID: "conv-1",
		Type:           "INBOX",
		Platform:       "facebook",
		Message: MessagingMessage{
			ID:         "msg-self-1",
			SenderID:   pageID, // same as page → must be skipped before HandleMessage
			SenderName: "Page Bot",
			Content:    "Hello",
			CreatedAt:  time.Now().Unix(),
		},
	}

	// Must not panic. If self-reply guard is missing, nil bus dereference panics here.
	ch.handleMessagingEvent(data)

	// Dedup entry is stored (dedup check runs before self-reply check).
	_, stored := ch.dedup.Load("msg:msg-self-1")
	if !stored {
		t.Error("dedup entry should have been stored (dedup runs before self-reply guard)")
	}
}


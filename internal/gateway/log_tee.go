package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/pkg/protocol"
)

const (
	ringBufferSize = 100
	redactedValue  = "***"
)

// sensitiveKeys are attribute keys whose values are redacted before forwarding.
var sensitiveKeys = []string{
	"key", "token", "secret", "password", "dsn",
	"credential", "authorization", "cookie",
}

// LogTee is a slog.Handler that forwards log records to subscribed WS clients
// while delegating to an underlying handler for normal output.
type LogTee struct {
	inner slog.Handler

	mu      sync.RWMutex
	clients map[string]*logSubscriber

	// Ring buffer of recent entries for replay on subscribe.
	ringMu  sync.RWMutex
	ring    []map[string]interface{}
	ringPos int
	ringFul bool
}

// logSubscriber tracks a client and its requested minimum log level.
type logSubscriber struct {
	client *Client
	level  slog.Level
}

// NewLogTee wraps an existing slog.Handler so log records are also forwarded
// to any WebSocket clients that have started log tailing.
func NewLogTee(inner slog.Handler) *LogTee {
	return &LogTee{
		inner:   inner,
		clients: make(map[string]*logSubscriber),
		ring:    make([]map[string]interface{}, ringBufferSize),
	}
}

func (t *LogTee) Enabled(ctx context.Context, level slog.Level) bool {
	// Always accept if inner handler wants it.
	if t.inner.Enabled(ctx, level) {
		return true
	}
	// Also accept if any subscriber wants this level (e.g. debug).
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, sub := range t.clients {
		if level >= sub.level {
			return true
		}
	}
	return false
}

func (t *LogTee) Handle(ctx context.Context, r slog.Record) error {
	// Build entry for WS clients.
	t.mu.RLock()
	n := len(t.clients)
	t.mu.RUnlock()

	needEntry := n > 0 // need to broadcast
	// Always build entry for ring buffer regardless of subscribers.
	entry := t.buildEntry(r)

	// Store in ring buffer.
	t.ringMu.Lock()
	t.ring[t.ringPos] = entry
	t.ringPos = (t.ringPos + 1) % ringBufferSize
	if t.ringPos == 0 {
		t.ringFul = true
	}
	t.ringMu.Unlock()

	// Forward to subscribers.
	if needEntry {
		evt := protocol.NewEvent("log", entry)
		level := r.Level

		t.mu.RLock()
		for _, sub := range t.clients {
			if level >= sub.level {
				sub.client.SendEvent(*evt)
			}
		}
		t.mu.RUnlock()
	}

	// Forward to inner handler only if it accepts this level.
	if t.inner.Enabled(ctx, r.Level) {
		return t.inner.Handle(ctx, r)
	}
	return nil
}

// buildEntry creates the WS payload from a log record, redacting sensitive attrs.
func (t *LogTee) buildEntry(r slog.Record) map[string]interface{} {
	entry := map[string]interface{}{
		"timestamp": r.Time.UnixMilli(),
		"level":     levelName(r.Level),
		"message":   r.Message,
	}

	attrs := map[string]interface{}{}
	r.Attrs(func(a slog.Attr) bool {
		key := a.Key
		val := a.Value.String()

		// Extract source hint.
		if key == "component" || key == "source" || key == "module" {
			entry["source"] = val
			return true
		}

		// Redact sensitive values.
		if isSensitiveKey(key) {
			attrs[key] = redactedValue
		} else {
			attrs[key] = val
		}
		return true
	})

	if len(attrs) > 0 {
		entry["attrs"] = attrs
	}

	return entry
}

func (t *LogTee) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogTee{
		inner:   t.inner.WithAttrs(attrs),
		clients: t.clients,
		ring:    t.ring,
	}
}

func (t *LogTee) WithGroup(name string) slog.Handler {
	return &LogTee{
		inner:   t.inner.WithGroup(name),
		clients: t.clients,
		ring:    t.ring,
	}
}

// Subscribe adds a client to the log tailing set at the given level.
// Pass slog.LevelInfo for default, slog.LevelDebug for verbose.
func (t *LogTee) Subscribe(client *Client, level slog.Level) {
	t.mu.Lock()
	t.clients[client.ID()] = &logSubscriber{client: client, level: level}
	t.mu.Unlock()

	// Replay ring buffer entries at the requested level.
	t.ringMu.RLock()
	var entries []map[string]interface{}
	if t.ringFul {
		// Buffer is full — read from ringPos (oldest) to ringPos-1 (newest).
		for i := 0; i < ringBufferSize; i++ {
			idx := (t.ringPos + i) % ringBufferSize
			e := t.ring[idx]
			if e != nil && logLevelValue(e["level"]) >= level {
				entries = append(entries, e)
			}
		}
	} else {
		// Buffer not full — read from 0 to ringPos-1.
		for i := 0; i < t.ringPos; i++ {
			e := t.ring[i]
			if e != nil && logLevelValue(e["level"]) >= level {
				entries = append(entries, e)
			}
		}
	}
	t.ringMu.RUnlock()

	for _, e := range entries {
		client.SendEvent(*protocol.NewEvent("log", e))
	}

	// Send sentinel so the client knows tailing started.
	client.SendEvent(*protocol.NewEvent("log", map[string]interface{}{
		"timestamp": time.Now().UnixMilli(),
		"level":     "info",
		"message":   "Log tailing started",
		"source":    "gateway",
	}))
}

// Unsubscribe removes a client from the log tailing set.
func (t *LogTee) Unsubscribe(clientID string) {
	t.mu.Lock()
	delete(t.clients, clientID)
	t.mu.Unlock()
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// logLevelValue converts a level name string back to slog.Level for filtering.
func logLevelValue(v interface{}) slog.Level {
	s, _ := v.(string)
	switch s {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	case "info":
		return slog.LevelInfo
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

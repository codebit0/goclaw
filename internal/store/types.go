package store

import (
	"time"

	"github.com/google/uuid"
)

// BaseModel provides common fields for all database models.
type BaseModel struct {
	ID        uuid.UUID `json:"id" db:"id"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// GenNewID generates a new UUID v7 (time-ordered).
func GenNewID() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

// StoreConfig configures the store layer.
type StoreConfig struct {
	// PostgresDSN is the Postgres connection string (required for postgres backend).
	PostgresDSN string

	// SQLitePath is the path to the SQLite database file (required for sqlite backend).
	SQLitePath string

	// StorageBackend selects the database backend: "postgres" (default) or "sqlite".
	StorageBackend string

	// SkillsStorageDir is the directory for skill file content (default: dataDir/skills-store/).
	SkillsStorageDir string

	// Workspace is the default agent workspace path.
	Workspace string

	// GlobalSkillsDir is the global skills directory (e.g. ~/.goclaw/skills).
	GlobalSkillsDir string

	// BuiltinSkillsDir is the builtin skills directory (bundled with binary).
	BuiltinSkillsDir string

	// EncryptionKey is the AES-256 key for encrypting sensitive data (API keys).
	// If empty, sensitive data is stored in plain text.
	EncryptionKey string
}

// BrowserOpts holds per-agent browser configuration injected via context.
// Used by the browser tool to customize browser launch args and viewport per agent.
type BrowserOpts struct {
	LaunchArgs   []string `json:"browser_launch_args,omitempty"`
	WindowWidth  int      `json:"browser_window_width,omitempty"`
	WindowHeight int      `json:"browser_window_height,omitempty"`
	UseProxy     bool     `json:"browser_use_proxy,omitempty"`
}

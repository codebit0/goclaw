package cmd

import (
	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/gateway"
	httpapi "github.com/nextlevelbuilder/goclaw/internal/http"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
	"github.com/nextlevelbuilder/goclaw/pkg/browser"
)

// gatewayDeps holds shared dependencies used across the extracted gateway setup functions.
// It is populated in runGateway() and passed to helper methods to avoid long parameter lists.
type gatewayDeps struct {
	cfg              *config.Config
	server           *gateway.Server
	msgBus           *bus.MessageBus
	pgStores         *store.Stores
	providerRegistry *providers.Registry
	channelMgr       *channels.Manager
	agentRouter      *agent.Router
	toolsReg         *tools.Registry
	skillsLoader     *skills.Loader // optional: enables skill creation in evolution approval
	workspace        string
	dataDir          string
	browserMgr       *browser.Manager          // optional: browser automation manager
	browserLiveH     *httpapi.BrowserLiveHandler // optional: browser live view handler
}

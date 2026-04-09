package browser

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// browserTenantKey is a context key for passing tenant ID to browser operations.
type browserTenantKey struct{}

// browserAgentKey is a context key for passing agent key to browser operations.
type browserAgentKey struct{}

// WithTenantID returns a context with the browser tenant ID set.
// This is used to isolate browser pages per tenant via incognito contexts.
func WithTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, browserTenantKey{}, tenantID)
}

// WithAgentKey returns a context with the browser agent key set.
// This is used to track which agent opened which page.
func WithAgentKey(ctx context.Context, agentKey string) context.Context {
	return context.WithValue(ctx, browserAgentKey{}, agentKey)
}

// tenantIDFromCtx extracts the tenant ID from context.
func tenantIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(browserTenantKey{}).(string); ok {
		return v
	}
	return ""
}

// agentKeyFromCtx extracts the agent key from context.
func agentKeyFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(browserAgentKey{}).(string); ok {
		return v
	}
	return ""
}

// browserSessionKey is a context key for passing session key to browser operations.
type browserSessionKey struct{}

// WithSessionKey returns a context with the browser session key set.
// This is used to track which session opened which page.
func WithSessionKey(ctx context.Context, sessionKey string) context.Context {
	return context.WithValue(ctx, browserSessionKey{}, sessionKey)
}

// sessionKeyFromCtx extracts the session key from context.
func sessionKeyFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(browserSessionKey{}).(string); ok {
		return v
	}
	return ""
}

// browserProfileNameKey is a context key for passing profile name per-request.
type browserProfileNameKey struct{}

// WithProfileName returns a context with the browser profile name set.
// Used to override the Manager's activeProfile on a per-request basis.
func WithProfileName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, browserProfileNameKey{}, name)
}

// profileNameFromCtx extracts the profile name from context.
func profileNameFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(browserProfileNameKey{}).(string); ok {
		return v
	}
	return ""
}

// browserUseProxyKey is a context key for per-agent proxy opt-in.
type browserUseProxyKey struct{}

// WithUseProxy returns a context with the browser use_proxy flag set.
func WithUseProxy(ctx context.Context, use bool) context.Context {
	return context.WithValue(ctx, browserUseProxyKey{}, use)
}

// useProxyFromCtx extracts the use_proxy flag from context. Returns true if set.
func useProxyFromCtx(ctx context.Context) (bool, bool) {
	v, ok := ctx.Value(browserUseProxyKey{}).(bool)
	return v, ok
}

// proxyAuthCredsKey is a context key for passing proxy auth credentials to NewPage.
type proxyAuthCredsKey struct{}

// ProxyAuthCreds holds proxy authentication credentials for CDP Fetch-based auth.
type ProxyAuthCreds struct {
	Username string
	Password string
}

// WithProxyAuthCreds returns a context with proxy auth credentials set.
// Used by container pool to pass creds to ChromeEngine.NewPage for CDP Fetch auth.
func WithProxyAuthCreds(ctx context.Context, c *ProxyAuthCreds) context.Context {
	return context.WithValue(ctx, proxyAuthCredsKey{}, c)
}

// proxyAuthCredsFromCtx extracts proxy auth credentials from context.
func proxyAuthCredsFromCtx(ctx context.Context) *ProxyAuthCreds {
	if v, ok := ctx.Value(proxyAuthCredsKey{}).(*ProxyAuthCreds); ok {
		return v
	}
	return nil
}

// browserProfileDirKey is a context key for passing profile directory to browser operations.
type browserProfileDirKey struct{}

// WithProfileDir returns a context with the browser profile directory set.
// Used by ContainerPoolEngine to route requests to the correct container.
func WithProfileDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, browserProfileDirKey{}, dir)
}

// profileDirFromCtx extracts the profile directory from context.
func profileDirFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(browserProfileDirKey{}).(string); ok {
		return v
	}
	return ""
}

// browserOptsKey is a context key for per-agent browser options.
type browserOptsKey struct{}

// WithBrowserOpts returns a context with per-agent browser options set.
func WithBrowserOpts(ctx context.Context, opts *store.BrowserOpts) context.Context {
	return context.WithValue(ctx, browserOptsKey{}, opts)
}

// browserOptsFromCtx extracts per-agent browser options from context.
func browserOptsFromCtx(ctx context.Context) *store.BrowserOpts {
	if v, ok := ctx.Value(browserOptsKey{}).(*store.BrowserOpts); ok {
		return v
	}
	return nil
}

// viewportOverride holds optional viewport dimensions passed via tool args.
type viewportOverride struct {
	Width  int
	Height int
}

type viewportOverrideKey struct{}

// WithViewportOverride returns a context with viewport dimensions to apply on new pages.
func WithViewportOverride(ctx context.Context, width, height int) context.Context {
	return context.WithValue(ctx, viewportOverrideKey{}, &viewportOverride{Width: width, Height: height})
}

// viewportOverrideFromCtx extracts viewport override from context.
func viewportOverrideFromCtx(ctx context.Context) *viewportOverride {
	if v, ok := ctx.Value(viewportOverrideKey{}).(*viewportOverride); ok {
		return v
	}
	return nil
}

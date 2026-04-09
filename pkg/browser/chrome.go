package browser

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// stripUserinfo removes any userinfo (user:pass@) from a proxy URL so it's
// safe for Chrome's --proxy-server flag which doesn't support credentials.
func stripUserinfo(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.User = nil
	return parsed.String()
}

// blockedChromeFlags is the set of Chrome flags that agents are not allowed to set via
// ExtraArgs. Allowing these would undermine security boundaries:
//   - remote-debugging-address / remote-debugging-port: expose CDP externally
//   - disable-web-security: bypasses CORS and SOP, enables SSRF via browser
//   - user-data-dir: redirects profile to arbitrary OS paths (path traversal)
//   - proxy-server: overrides the gateway-managed proxy, leaks traffic
//   - remote-allow-origins: opens CDP to arbitrary origins
//   - enable-automation / headless: fingerprinting control already managed by the engine
var blockedChromeFlags = map[string]bool{
	"remote-debugging-address": true,
	"remote-debugging-port":    true,
	"disable-web-security":     true,
	"user-data-dir":            true,
	"proxy-server":             true,
	"remote-allow-origins":     true,
	"enable-automation":        true,
	"headless":                 true,
}

// ChromeEngine implements Engine using go-rod (Chrome DevTools Protocol).
type ChromeEngine struct {
	browser   *rod.Browser
	attached  bool // true = don't kill process on Close()
	logger    *slog.Logger
	proxyUser string // proxy auth username (for CDP Fetch auth on new pages)
	proxyPass string // proxy auth password (decrypted)
}

// NewChromeEngine creates a ChromeEngine. The engine is not connected until Launch() is called.
func NewChromeEngine(logger *slog.Logger) *ChromeEngine {
	return &ChromeEngine{logger: logger}
}

func (e *ChromeEngine) Launch(opts LaunchOpts) error {
	switch {
	case opts.AttachURL != "":
		// Attach mode — connect to existing browser, don't kill on Close
		b := rod.New().ControlURL(opts.AttachURL)
		if err := b.Connect(); err != nil {
			return fmt.Errorf("attach to Chrome at %s: %w", opts.AttachURL, err)
		}
		e.browser = b
		e.attached = true
		e.logger.Info("attached to existing Chrome", "cdp", opts.AttachURL)

	case opts.RemoteURL != "":
		// Remote sidecar — resolve via /json/version
		u, err := resolveRemoteCDP(opts.RemoteURL)
		if err != nil {
			return fmt.Errorf("resolve remote Chrome at %s: %w", opts.RemoteURL, err)
		}
		b := rod.New().ControlURL(u)
		if err := b.Connect(); err != nil {
			return fmt.Errorf("connect to remote Chrome: %w", err)
		}
		e.browser = b
		e.attached = false
		e.logger.Info("connected to remote Chrome", "cdp", u, "remote", opts.RemoteURL)

	default:
		// Local browser — launch via rod launcher
		l := launcher.New().
			Headless(opts.Headless).
			Set("disable-gpu").
			Set("no-default-browser-check")

		if opts.BinaryPath != "" {
			l.Bin(opts.BinaryPath)
		}
		if opts.ProfileDir != "" {
			l.UserDataDir(opts.ProfileDir)
		}
		if opts.ProxyURL != "" {
			l.Set("proxy-server", stripUserinfo(opts.ProxyURL))
		}
		if len(opts.ExtensionPaths) > 0 {
			l.Set("load-extension", strings.Join(opts.ExtensionPaths, ","))
		}

		// Apply window size (per-agent or default)
		if opts.WindowWidth > 0 && opts.WindowHeight > 0 {
			l.Set("window-size", fmt.Sprintf("%d,%d", opts.WindowWidth, opts.WindowHeight))
		}

		// Apply per-agent extra launch args.
		// Blocked flags are skipped to prevent security bypass — agents must not be
		// able to disable web security, expose the debug port publicly, or redirect
		// Chrome's data directory to arbitrary system paths.
		for _, arg := range opts.ExtraArgs {
			parts := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)
			if len(parts) == 0 || parts[0] == "" {
				continue
			}
			flagName := parts[0]
			if blockedChromeFlags[flagName] {
				e.logger.Warn("security.browser: blocked dangerous Chrome flag in ExtraArgs",
					"flag", flagName)
				continue
			}
			if len(parts) == 2 {
				l.Set(flags.Flag(flagName), parts[1])
			} else {
				l.Set(flags.Flag(flagName))
			}
		}

		// Apply stealth flags to reduce automation detection
		StealthFlags(l)

		u, err := l.Launch()
		if err != nil {
			return fmt.Errorf("launch browser: %w", err)
		}
		b := rod.New().ControlURL(u)
		if err := b.Connect(); err != nil {
			return fmt.Errorf("connect to browser: %w", err)
		}
		e.browser = b
		e.attached = false
		e.logger.Info("browser launched", "cdp", u, "headless", opts.Headless, "profile", opts.ProfileDir, "binary", opts.BinaryPath)
	}
	// Store proxy credentials for CDP Fetch-based auth on new pages
	e.proxyUser = opts.ProxyUser
	e.proxyPass = opts.ProxyPass
	return nil
}

func (e *ChromeEngine) Close() error {
	if e.browser == nil {
		return nil
	}
	if e.attached {
		// Don't kill user's browser — just disconnect
		e.browser = nil
		return nil
	}
	err := e.browser.Close()
	e.browser = nil
	return err
}

func (e *ChromeEngine) NewPage(ctx context.Context, url string) (Page, error) {
	if e.browser == nil {
		return nil, fmt.Errorf("browser not running")
	}

	// Inject proxy auth creds from engine config if not already in context.
	// Host mode stores creds at launch time; container pool mode sets them per-request.
	if proxyAuthCredsFromCtx(ctx) == nil && e.proxyUser != "" {
		ctx = WithProxyAuthCreds(ctx, &ProxyAuthCreds{
			Username: e.proxyUser,
			Password: e.proxyPass,
		})
	}

	// Create a blank page first so we can inject stealth scripts
	// BEFORE any page JS runs (prevents bot detection).
	rodPage, err := e.browser.Page(proto.TargetCreateTarget{URL: ""})
	if err != nil {
		return nil, fmt.Errorf("new page: %w", err)
	}

	// Inject stealth + fingerprint via Page.addScriptToEvaluateOnNewDocument.
	// RunImmediately=true ensures the script runs on the CURRENT about:blank
	// context AND on all future navigations. Without it, Chrome re-applies
	// navigator.webdriver=true during navigation before our script fires.
	fp := GenerateFingerprint("")
	stealthScript := stealthOnNewDocumentJS + "\n" + FingerprintOnNewDocumentJS(fp)
	_, _ = proto.PageAddScriptToEvaluateOnNewDocument{
		Source:         stealthScript,
		RunImmediately: true,
	}.Call(rodPage)

	// Override UA at CDP/network level so HTTP request headers match the
	// fingerprint. JS-only overrides don't change the actual User-Agent header
	// sent with HTTP requests — Google checks this header server-side.
	_ = proto.NetworkSetUserAgentOverride{
		UserAgent:      fp.UserAgent,
		AcceptLanguage: strings.Join(fp.Languages, ","),
		Platform:       fp.Platform,
	}.Call(rodPage)

	// Override navigator.language/languages at CDP level.
	// JS overrides on Navigator.prototype get reset by Chrome on navigation,
	// but Emulation.setLocaleOverride persists across navigations.
	_ = proto.EmulationSetLocaleOverride{
		Locale: fp.Languages[0],
	}.Call(rodPage)

	// Set viewport to match fingerprint screen dimensions.
	// Mismatch between window.outerWidth/outerHeight and screen.width/height
	// is a detection signal (PHANTOM_WINDOW_HEIGHT check).
	_ = proto.EmulationSetDeviceMetricsOverride{
		Width:             fp.ScreenWidth,
		Height:            fp.ScreenHeight,
		DeviceScaleFactor: 1,
		Mobile:            false,
	}.Call(rodPage)

	// Bypass CSP so stealth scripts can override properties on strict pages.
	_ = proto.PageSetBypassCSP{Enabled: true}.Call(rodPage)

	// Setup CDP Fetch-based proxy authentication.
	// Chrome's --proxy-server flag doesn't support credentials in the URL, so we
	// intercept 407 Proxy-Auth-Required challenges via the Fetch domain and respond
	// with the decrypted username/password.
	if creds := proxyAuthCredsFromCtx(ctx); creds != nil {
		_ = proto.FetchEnable{
			HandleAuthRequests: true,
		}.Call(rodPage)
		go rodPage.EachEvent(func(e *proto.FetchAuthRequired) {
			_ = proto.FetchContinueWithAuth{
				RequestID: e.RequestID,
				AuthChallengeResponse: &proto.FetchAuthChallengeResponse{
					Response: proto.FetchAuthChallengeResponseResponseProvideCredentials,
					Username: creds.Username,
					Password: creds.Password,
				},
			}.Call(rodPage)
		}, func(e *proto.FetchRequestPaused) {
			// Resume non-auth paused requests (Fetch.enable pauses ALL requests).
			_ = proto.FetchContinueRequest{
				RequestID: e.RequestID,
			}.Call(rodPage)
		})()
	}

	// Now navigate — stealth scripts will run before page JS.
	if url != "" && url != "about:blank" {
		if err := rodPage.Navigate(url); err != nil {
			rodPage.Close()
			return nil, fmt.Errorf("navigate to %s: %w", url, err)
		}
	}

	return newChromePage(rodPage), nil
}

func (e *ChromeEngine) Pages() ([]Page, error) {
	if e.browser == nil {
		return nil, fmt.Errorf("browser not running")
	}
	rodPages, err := e.browser.Pages()
	if err != nil {
		return nil, err
	}
	pages := make([]Page, len(rodPages))
	for i, p := range rodPages {
		pages[i] = newChromePage(p)
	}
	return pages, nil
}

func (e *ChromeEngine) Incognito() (Engine, error) {
	if e.browser == nil {
		return nil, fmt.Errorf("browser not running")
	}
	b, err := e.browser.Incognito()
	if err != nil {
		return nil, fmt.Errorf("create incognito context: %w", err)
	}
	return &ChromeEngine{browser: b, logger: e.logger}, nil
}

func (e *ChromeEngine) IsConnected() bool {
	if e.browser == nil {
		return false
	}
	_, err := e.browser.Pages()
	return err == nil
}

func (e *ChromeEngine) Name() string { return "chrome" }

// RodBrowser returns the underlying rod.Browser for reconnect scenarios.
// This is a Chrome-specific escape hatch; callers must type-assert.
func (e *ChromeEngine) RodBrowser() *rod.Browser { return e.browser }

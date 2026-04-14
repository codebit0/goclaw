package http

import (
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/nextlevelbuilder/goclaw/internal/permissions"
	"github.com/nextlevelbuilder/goclaw/internal/skills"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// validPkgName allows alphanumeric, hyphens, underscores, dots, @, / (for scoped npm).
// `github:` specs are validated separately (via skills.ParseGitHubSpec) and bypass this regex.
// Rejects names starting with - to prevent argument injection.
var validPkgName = regexp.MustCompile(`^[a-zA-Z0-9@][a-zA-Z0-9._+\-/@]*$`)

// validRepoPath matches "owner/repo" used by the releases endpoint.
var validRepoPath = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// PackagesHandler handles runtime package management HTTP endpoints.
type PackagesHandler struct{}

// NewPackagesHandler creates a handler for package management endpoints.
func NewPackagesHandler() *PackagesHandler {
	return &PackagesHandler{}
}

// RegisterRoutes registers all package management routes on the given mux.
func (h *PackagesHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/packages", h.readAuth(h.handleList))
	mux.HandleFunc("POST /v1/packages/install", h.adminAuth(h.handleInstall))
	mux.HandleFunc("POST /v1/packages/uninstall", h.adminAuth(h.handleUninstall))
	mux.HandleFunc("GET /v1/packages/runtimes", h.readAuth(h.handleRuntimes))
	mux.HandleFunc("GET /v1/packages/github-releases", h.readAuth(h.handleGitHubReleases))
	mux.HandleFunc("GET /v1/shell-deny-groups", h.readAuth(h.handleDenyGroups))
}

// readAuth allows viewer+ for read operations.
func (h *PackagesHandler) readAuth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth("", next)
}

// adminAuth requires admin role for write operations (install/uninstall).
// Prevents agents from calling these endpoints even if they obtain the gateway token,
// since agent requests via browser pairing only get operator role.
func (h *PackagesHandler) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(permissions.RoleAdmin, next)
}

// handleList returns all installed packages grouped by category (system/pip/npm).
func (h *PackagesHandler) handleList(w http.ResponseWriter, r *http.Request) {
	pkgs := skills.ListInstalledPackages(r.Context())
	writeJSON(w, http.StatusOK, pkgs)
}

// parseAndValidatePackage reads and validates a package name from the request body.
// Returns the validated package string or writes an error response and returns empty.
func parseAndValidatePackage(w http.ResponseWriter, r *http.Request) string {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Package string `json:"package"`
	}
	if !bindJSON(w, r, extractLocale(r), &body) {
		return ""
	}
	if body.Package == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "package required"})
		return ""
	}

	// github: packages carry the scheme prefix through the whole pipeline — validate
	// the bare spec via the installer parser rather than the generic regex.
	if strings.HasPrefix(body.Package, "github:") {
		if _, err := skills.ParseGitHubSpec(body.Package); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid github spec"})
			return ""
		}
		return body.Package
	}

	// Strip prefix for validation, then validate the bare package name.
	name := body.Package
	for _, prefix := range []string{"pip:", "npm:"} {
		if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			name = name[len(prefix):]
			break
		}
	}
	if !validPkgName.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid package name"})
		return ""
	}

	return body.Package
}

// handleInstall installs a single package.
// Body: {"package": "github-cli"} or {"package": "pip:pandas"} or {"package": "npm:typescript"}
//
// Phase 0b hotfix: server-wide package installation (pip/npm/apk) must be
// restricted to master-scope callers. Non-master tenant admins previously
// reached this handler because the adminAuth middleware only checks role, not
// tenant scope — a supply-chain vector (CRITICAL-2 in the audit report).
func (h *PackagesHandler) handleInstall(w http.ResponseWriter, r *http.Request) {
	if !requireMasterScope(w, r) {
		return
	}
	pkg := parseAndValidatePackage(w, r)
	if pkg == "" {
		return
	}
	ok, errMsg := skills.InstallSingleDep(r.Context(), pkg)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": errMsg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUninstall removes a single package.
// Body: {"package": "github-cli"} or {"package": "pip:pandas"} or {"package": "npm:typescript"}
//
// Phase 0b hotfix: same master-scope guard as handleInstall — uninstall can
// break system skills, causing server-wide DoS for every tenant.
func (h *PackagesHandler) handleUninstall(w http.ResponseWriter, r *http.Request) {
	if !requireMasterScope(w, r) {
		return
	}
	pkg := parseAndValidatePackage(w, r)
	if pkg == "" {
		return
	}
	ok, errMsg := skills.UninstallPackage(r.Context(), pkg)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": errMsg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRuntimes returns the availability of prerequisite runtimes.
func (h *PackagesHandler) handleRuntimes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, skills.CheckRuntimes())
}

// handleGitHubReleases proxies the GitHub Releases API for the picker UI.
// GET /v1/packages/github-releases?repo=owner/repo&limit=10
// Auth: viewer+ (read-only, no secrets exposed).
// Throttled via per-user rate limiter to protect the shared GitHub API quota.
func (h *PackagesHandler) handleGitHubReleases(w http.ResponseWriter, r *http.Request) {
	if !enforceGitHubReleasesLimit(w, r) {
		return
	}
	gh := skills.DefaultGitHubInstaller()
	if gh == nil || gh.Client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "github installer not configured"})
		return
	}
	repo := r.URL.Query().Get("repo")
	if !validRepoPath.MatchString(repo) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid repo; expected owner/repo"})
		return
	}
	parts := strings.SplitN(repo, "/", 2)
	owner, repoName := parts[0], parts[1]

	if !gh.AllowedOrg(owner) {
		// Return 404 rather than 403 so allowlist membership is not enumerable.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	limit := 10
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= 50 {
			limit = n
		}
	}

	releases, err := gh.Client.ListReleases(r.Context(), owner, repoName, limit)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	type releaseDTO struct {
		Tag             string              `json:"tag"`
		Name            string              `json:"name"`
		PublishedAt     string              `json:"published_at"`
		Prerelease      bool                `json:"prerelease"`
		MatchingAssets  []skills.GitHubAsset `json:"matching_assets"`
		AllAssetsCount  int                 `json:"all_assets_count"`
	}
	out := make([]releaseDTO, 0, len(releases))
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		if pick, perr := skills.SelectAsset(rel.Assets, "linux", runtime.GOARCH); perr == nil && pick != nil {
			out = append(out, releaseDTO{
				Tag:            rel.TagName,
				Name:           rel.Name,
				PublishedAt:    rel.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"),
				Prerelease:     rel.Prerelease,
				MatchingAssets: []skills.GitHubAsset{*pick},
				AllAssetsCount: len(rel.Assets),
			})
		} else {
			out = append(out, releaseDTO{
				Tag:            rel.TagName,
				Name:           rel.Name,
				PublishedAt:    rel.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"),
				Prerelease:     rel.Prerelease,
				MatchingAssets: nil,
				AllAssetsCount: len(rel.Assets),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": out})
}

// handleDenyGroups returns all registered shell deny groups with name, description, and default state.
func (h *PackagesHandler) handleDenyGroups(w http.ResponseWriter, _ *http.Request) {
	type groupInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Default     bool   `json:"default"`
	}
	groups := make([]groupInfo, 0, len(tools.DenyGroupRegistry))
	for _, name := range tools.DenyGroupNames() {
		g := tools.DenyGroupRegistry[name]
		groups = append(groups, groupInfo{
			Name:        g.Name,
			Description: g.Description,
			Default:     g.Default,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

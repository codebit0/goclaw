package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	shellwords "github.com/mattn/go-shellwords"
	"github.com/nextlevelbuilder/goclaw/internal/sandbox"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"golang.org/x/text/unicode/norm"
)

// Dangerous command patterns organized into configurable deny groups.
// Defense-in-depth: patterns complement Docker hardening (cap-drop ALL,
// no-new-privileges, pids-limit, memory limit).
// Sources: OWASP Agentic AI Top 10, Claude Code CVE-2025-66032, MITRE ATT&CK,
// PayloadsAllTheThings, Trail of Bits prompt-injection-to-RCE research.
// Groups and patterns defined in shell_deny_groups.go.

// DefaultDenyPatterns returns all patterns from groups where Default=true.
// Backward-compatible wrapper for code that doesn't use per-agent overrides.
func DefaultDenyPatterns() []*regexp.Regexp {
	return ResolveDenyPatterns(nil)
}

// ExecTool executes shell commands, optionally inside a sandbox container.
type ExecTool struct {
	workspace        string
	timeout          time.Duration
	pathDenyPatterns []*regexp.Regexp // always-on path-based denials (DenyPaths)
	pathDenyRoots    []string         // raw deny roots for nested workspace exemptions
	denyExemptions   []string         // substrings that exempt a command from deny
	restrict         bool
	sandboxMgr       sandbox.Manager      // nil = no sandbox, execute on host
	approvalMgr      *ExecApprovalManager // nil = no approval needed
	agentID          string               // for approval request context
	secureCLIStore   store.SecureCLIStore // nil = no credentialed exec
}

// NewExecTool creates an exec tool that runs commands directly on the host.
func NewExecTool(workspace string, restrict bool) *ExecTool {
	return &ExecTool{
		workspace: workspace,
		timeout:   60 * time.Second,
		restrict:  restrict,
	}
}

// NewSandboxedExecTool creates an exec tool that routes commands through a sandbox container.
func NewSandboxedExecTool(workspace string, restrict bool, mgr sandbox.Manager) *ExecTool {
	return &ExecTool{
		workspace:  workspace,
		timeout:    300 * time.Second, // sandbox allows longer timeout
		restrict:   restrict,
		sandboxMgr: mgr,
	}
}

// SetSandboxKey is a no-op; sandbox key is now read from ctx (thread-safe).
func (t *ExecTool) SetSandboxKey(key string) {}

// DenyPaths adds always-on deny patterns that block commands referencing the given paths.
// These are NOT configurable via deny groups — they always apply regardless of group config.
func (t *ExecTool) DenyPaths(paths ...string) {
	for _, p := range paths {
		escaped := regexp.QuoteMeta(p)
		t.pathDenyPatterns = append(t.pathDenyPatterns, regexp.MustCompile(escaped))
		t.pathDenyRoots = append(t.pathDenyRoots, p)
	}
}

// AllowPathExemptions adds path prefixes that exempt a command from deny pattern matches.
// Each shell argument is checked individually — commands like "cat .goclaw/skills-store/tool.py"
// are exempt because the argument ".goclaw/skills-store/tool.py" starts with the prefix.
func (t *ExecTool) AllowPathExemptions(prefixes ...string) {
	t.denyExemptions = append(t.denyExemptions, prefixes...)
}

// normalizeCommand applies NFKC Unicode normalization and strips zero-width
// characters before deny pattern matching, preventing Unicode-based bypasses.
func normalizeCommand(s string) string {
	// NFKC normalization: folds compatibility characters (e.g. fullwidth letters)
	s = norm.NFKC.String(s)
	// Strip zero-width characters that are invisible but can fragment tokens
	s = strings.NewReplacer(
		"\u200b", "", // zero-width space
		"\u200c", "", // zero-width non-joiner
		"\u200d", "", // zero-width joiner
		"\u2060", "", // word joiner
		"\ufeff", "", // BOM / zero-width no-break space
	).Replace(s)
	return s
}

func (t *ExecTool) dynamicPathExemptions(ctx context.Context) []string {
	var exemptions []string
	seen := make(map[string]struct{}, 4)
	workspace := ToolWorkspaceFromCtx(ctx)
	teamWorkspace := ToolTeamWorkspaceFromCtx(ctx)

	var dirs []string
	if teamWorkspace != "" {
		dirs = append(dirs, teamWorkspace)
	}
	if workspace != "" && filepath.Clean(workspace) != filepath.Clean(teamWorkspace) {
		dirs = append(dirs, filepath.Join(workspace, ".uploads"))
	}

	for _, dir := range dirs {
		if dir == "" || strings.Contains(dir, "..") {
			continue
		}
		cleanDir := filepath.Clean(dir)
		if !t.isNestedUnderDeniedRoot(cleanDir) {
			continue
		}
		for _, ex := range []string{cleanDir, cleanDir + string(filepath.Separator)} {
			if _, ok := seen[ex]; ok {
				continue
			}
			seen[ex] = struct{}{}
			exemptions = append(exemptions, ex)
		}
	}
	return exemptions
}

func (t *ExecTool) isNestedUnderDeniedRoot(path string) bool {
	for _, root := range t.pathDenyRoots {
		cleanRoot := filepath.Clean(root)
		if cleanRoot == "." || cleanRoot == string(filepath.Separator) {
			continue
		}
		if filepath.IsAbs(cleanRoot) != filepath.IsAbs(path) {
			continue
		}
		if path == cleanRoot {
			continue
		}
		if strings.HasPrefix(path, cleanRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func matchesPathExemption(path string, exemptions []string) bool {
	sep := string(filepath.Separator)
	for _, ex := range exemptions {
		if ex == "" {
			continue
		}
		if path == ex {
			return true
		}
		if strings.HasSuffix(ex, sep) {
			if strings.HasPrefix(path, ex) {
				return true
			}
			continue
		}
		if strings.HasPrefix(path, ex+sep) {
			return true
		}
	}
	return false
}

func parseExecCommandWords(command string) []string {
	var words []string
	for _, segment := range splitExecCommandSegments(command) {
		parser := shellwords.NewParser()
		parser.ParseBacktick = false
		parser.ParseEnv = false

		segmentWords, err := parser.Parse(segment)
		if err != nil || len(segmentWords) == 0 {
			words = append(words, strings.Fields(segment)...)
			continue
		}
		words = append(words, segmentWords...)
	}
	if len(words) == 0 {
		return strings.Fields(command)
	}
	return words
}

func splitExecCommandSegments(command string) []string {
	var segments []string
	start := 0
	inSingle := false
	inDouble := false

	for i := 0; i < len(command); i++ {
		ch := command[i]
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			}
		case inDouble:
			if ch == '\\' && i+1 < len(command) {
				i++
			} else if ch == '"' {
				inDouble = false
			}
		default:
			switch ch {
			case '\\':
				if i+1 < len(command) {
					i++
				}
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case ';', '|', '&', '<', '>', '\n', '\r':
				if segment := strings.TrimSpace(command[start:i]); segment != "" {
					segments = append(segments, segment)
				}
				start = i + 1
			}
		}
	}

	if tail := strings.TrimSpace(command[start:]); tail != "" {
		segments = append(segments, tail)
	}
	return segments
}

func extractPathCandidates(word string) []string {
	if word == "" {
		return nil
	}

	queue := []string{word}
	seen := make(map[string]struct{}, 4)
	var out []string

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == "" {
			continue
		}
		if _, ok := seen[current]; ok {
			continue
		}
		seen[current] = struct{}{}
		if looksLikePathCandidate(current) {
			out = append(out, current)
		}
		for _, sep := range []string{"=", "@"} {
			if idx := strings.Index(current, sep); idx >= 0 && idx+1 < len(current) {
				queue = append(queue, current[idx+1:])
			}
		}
	}
	return out
}

func looksLikePathCandidate(s string) bool {
	if s == "" {
		return false
	}
	if filepath.IsAbs(s) {
		return true
	}
	return strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, ".uploads/") ||
		strings.HasPrefix(s, ".goclaw/") ||
		strings.HasPrefix(s, "teams/") ||
		strings.HasPrefix(s, "tenants/") ||
		strings.HasPrefix(s, "~/") ||
		strings.Contains(s, string(filepath.Separator))
}

func canonicalizeExecPath(path, baseDir string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(homeDir, strings.TrimPrefix(path, "~/"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	absPath, _ := filepath.Abs(filepath.Clean(path))
	if real, err := filepath.EvalSymlinks(absPath); err == nil {
		return real, nil
	}
	return resolveThroughExistingAncestors(absPath)
}

func matchesAnyPathExemption(word string, exemptions []string, baseDir string) bool {
	for _, candidate := range extractPathCandidates(word) {
		if strings.Contains(candidate, "..") {
			continue
		}
		realCandidate, err := canonicalizeExecPath(candidate, baseDir)
		if err != nil {
			continue
		}
		for _, exemption := range exemptions {
			realExemption, err := canonicalizeExecPath(exemption, baseDir)
			if err != nil {
				continue
			}
			if matchesPathExemption(realCandidate, []string{realExemption}) {
				return true
			}
		}
	}
	return false
}

// SetApprovalManager sets the exec approval manager for this tool.
func (t *ExecTool) SetApprovalManager(mgr *ExecApprovalManager, agentID string) {
	t.approvalMgr = mgr
	t.agentID = agentID
}

// SetSecureCLIStore sets the credential store for credentialed exec.
func (t *ExecTool) SetSecureCLIStore(s store.SecureCLIStore) {
	t.secureCLIStore = s
}

func (t *ExecTool) Name() string        { return "exec" }
func (t *ExecTool) Description() string { return "Execute a shell command and return its output" }
func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Working directory for the command (default: workspace root)",
			},
		},
		"required": []string{"command"},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) *Result {
	command, _ := args["command"].(string)
	if command == "" {
		return ErrorResult("command is required")
	}

	// Reject NUL bytes — they cause silent shell truncation enabling injection.
	if strings.ContainsRune(command, '\x00') {
		return ErrorResult("command contains invalid NUL byte")
	}

	// Normalize command before all deny checks: NFKC + zero-width strip prevents
	// Unicode-based pattern bypass while preserving functional command content.
	normalizedCommand := normalizeCommand(command)

	// Resolve deny patterns: per-agent overrides from context, fallback to all defaults.
	denyOverrides := store.ShellDenyGroupsFromContext(ctx)
	groupPatterns := ResolveDenyPatterns(denyOverrides)

	// Also resolve package_install patterns separately for approval routing.
	var pkgInstallPatterns []*regexp.Regexp
	if pkgGroup, ok := DenyGroupRegistry["package_install"]; ok && IsGroupDenied(denyOverrides, "package_install") {
		pkgInstallPatterns = pkgGroup.Patterns
	}

	// Combine group-based patterns + always-on path denials.
	allPatterns := make([]*regexp.Regexp, 0, len(groupPatterns)+len(t.pathDenyPatterns))
	allPatterns = append(allPatterns, groupPatterns...)
	allPatterns = append(allPatterns, t.pathDenyPatterns...)
	exemptions := append([]string{}, t.denyExemptions...)
	exemptions = append(exemptions, t.dynamicPathExemptions(ctx)...)

	// Check for dangerous commands (applies to both host and sandbox).
	wordFields := parseExecCommandWords(normalizedCommand)
	pathBaseDir := ToolWorkspaceFromCtx(ctx)
	if pathBaseDir == "" {
		pathBaseDir = t.workspace
	}
	for _, pattern := range allPatterns {
		if pattern.MatchString(normalizedCommand) {
			// Check if exemption applies. Only exempt if EVERY field that
			// individually matches the deny pattern is covered by an exemption.
			// This prevents pipe/comment bypass: "cat /app/data/skills-store/x | cat /app/data/secret"
			// — the second field matches deny but has no exemption → denied.
			// Strips surrounding quotes (LLMs often quote paths) and rejects
			// path traversal ("..") to prevent exemption escape.
			exempt := false
			trimmed := strings.TrimSpace(normalizedCommand)
			fields := wordFields
			if len(fields) == 0 {
				fields = strings.Fields(trimmed)
			}
			matchingFields := 0
			exemptFields := 0
			for _, field := range fields {
				clean := strings.TrimSpace(field)
				if !pattern.MatchString(clean) {
					continue // field doesn't trigger this deny pattern
				}
				matchingFields++
				if matchesAnyPathExemption(clean, exemptions, pathBaseDir) {
					exemptFields++
				}
			}
			// Exempt only if at least one field matched AND all matched fields are exempt.
			if matchingFields > 0 && exemptFields == matchingFields {
				exempt = true
			}
			if exempt {
				continue
			}

			// Package install commands: route through approval flow instead of hard deny.
			// This lets agents "request permission" from admin to install packages.
			if t.approvalMgr != nil && matchesAny(normalizedCommand, pkgInstallPatterns) {
				slog.Info("exec: package install requires approval", "command", truncateCmd(command, 100), "agent", t.agentID)
				decision, err := t.approvalMgr.RequestApproval(command, t.agentID, 2*time.Minute)
				if err != nil {
					return ErrorResult(fmt.Sprintf("package install approval: %v", err))
				}
				if decision == ApprovalDeny {
					return ErrorResult("package installation denied by admin")
				}
				// Approved — skip deny, continue to execution.
				continue
			}

			return ErrorResult(fmt.Sprintf("command denied by safety policy: matches pattern %s", pattern.String()))
		}
	}

	// Memory path hint: shell commands can't access DB-backed memory files.
	if hint := MaybeMemoryExecHint(normalizedCommand); hint != "" {
		return SilentResult(hint)
	}

	// Credentialed exec: if command matches a configured binary, use Direct Exec Mode.
	// This bypasses approval (admin trust) and shell (security).
	if cred, binary, cmdArgs := t.lookupCredentialedBinary(ctx, command); cred != nil {
		cwd := ToolWorkspaceFromCtx(ctx)
		if cwd == "" {
			cwd = t.workspace
		}
		if wd, _ := args["working_dir"].(string); wd != "" {
			if effectiveRestrict(ctx, t.restrict) {
				if resolved, err := resolvePath(wd, t.workspace, true); err == nil {
					cwd = resolved
				}
			} else {
				cwd = wd
			}
		}
		sandboxKey := ToolSandboxKeyFromCtx(ctx)
		return t.executeCredentialed(ctx, cred, binary, cmdArgs, cwd, sandboxKey, command)
	}

	// Exec approval check (matching TS exec-approval.ts pipeline)
	if t.approvalMgr != nil {
		switch t.approvalMgr.CheckCommand(command) {
		case "deny":
			return ErrorResult("command denied by exec approval policy")
		case "ask":
			decision, err := t.approvalMgr.RequestApproval(command, t.agentID, 2*time.Minute)
			if err != nil {
				return ErrorResult(fmt.Sprintf("exec approval: %v", err))
			}
			if decision == ApprovalDeny {
				return ErrorResult("command denied by user")
			}
		}
	}

	// Use per-user workspace from context if available, fallback to struct field.
	// The context workspace is tenant-scoped; t.workspace is the global (master) workspace.
	cwd := ToolWorkspaceFromCtx(ctx)
	if cwd == "" {
		cwd = t.workspace
	}
	if wd, _ := args["working_dir"].(string); wd != "" {
		if effectiveRestrict(ctx, t.restrict) {
			// Validate working_dir against the tenant-scoped workspace (not the
			// global workspace) so non-master tenants can't escape their scope.
			// Also allow team workspace as a valid target (same as filesystem tools).
			wsBase := ToolWorkspaceFromCtx(ctx)
			if wsBase == "" {
				wsBase = t.workspace
			}
			allowed := allowedWithTeamWorkspace(ctx, nil)
			resolved, err := resolvePathWithAllowed(wd, wsBase, true, allowed)
			if err != nil {
				return ErrorResult(err.Error())
			}
			cwd = resolved
		} else {
			cwd = wd
		}
	}

	// Sandbox routing (sandboxKey from ctx — thread-safe)
	sandboxKey := ToolSandboxKeyFromCtx(ctx)
	if t.sandboxMgr != nil && sandboxKey != "" {
		return t.executeInSandbox(ctx, command, cwd, sandboxKey)
	}

	// Host execution
	return t.executeOnHost(ctx, command, cwd)
}

// matchesAny checks if a command matches any pattern in the list.
func matchesAny(command string, patterns []*regexp.Regexp) bool {
	for _, p := range patterns {
		if p.MatchString(command) {
			return true
		}
	}
	return false
}

// executeOnHost runs a command directly on the host (original behavior).
func (t *ExecTool) executeOnHost(ctx context.Context, command, cwd string) *Result {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd

	// Limit output to 1MB to prevent OOM from runaway commands.
	stdout := &limitedBuffer{max: 1 << 20}
	stderr := &limitedBuffer{max: 1 << 20}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	var result string
	if stdout.Len() > 0 {
		result = stdout.String()
	}
	if stderr.Len() > 0 {
		if result != "" {
			result += "\n"
		}
		result += "STDERR:\n" + stderr.String()
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return ErrorResult(fmt.Sprintf("command timed out after %s", t.timeout))
		}
		if result == "" {
			result = err.Error()
		}
		return ErrorResult(result)
	}

	if result == "" {
		result = "(command completed with no output)"
	}

	return SilentResult(capExecOutput(result, execMaxOutputChars))
}

// executeInSandbox routes a command through a Docker sandbox container.
func (t *ExecTool) executeInSandbox(ctx context.Context, command, cwd, sandboxKey string) *Result {
	sb, err := t.sandboxMgr.Get(ctx, sandboxKey, t.workspace, SandboxConfigFromCtx(ctx))
	if err != nil {
		if errors.Is(err, sandbox.ErrSandboxDisabled) {
			return t.executeOnHost(ctx, command, cwd)
		}
		// Docker unavailable (binary missing, daemon down) → fail closed.
		// Do NOT silently fallback to host — that defeats the purpose of sandboxing.
		slog.Warn("security.sandbox_unavailable",
			"error", err,
			"command", truncateCmd(command, 80),
		)
		return ErrorResult(fmt.Sprintf("sandbox unavailable: %v (will not fall back to unsandboxed host execution)", err))
	}

	// Map host workdir to container workdir via SandboxCwd helper.
	containerCwd, cwdErr := SandboxCwd(ctx, t.workspace, sandbox.DefaultContainerWorkdir)
	if cwdErr != nil {
		return ErrorResult(fmt.Sprintf("sandbox path mapping: %v", cwdErr))
	}

	result, err := sb.Exec(ctx, []string{"sh", "-c", command}, containerCwd) //nolint: no ExecOption for normal exec
	if err != nil {
		return ErrorResult(fmt.Sprintf("sandbox exec: %v", err))
	}

	// Format output same as host execution
	output := result.Stdout
	if result.Stderr != "" {
		if output != "" {
			output += "\n"
		}
		output += "STDERR:\n" + result.Stderr
	}
	if result.ExitCode != 0 {
		if output == "" {
			output = fmt.Sprintf("command exited with code %d", result.ExitCode)
		}
		output += MaybeSandboxHint(result.ExitCode, output)
		return ErrorResult(output)
	}
	if output == "" {
		output = "(command completed with no output)"
	}

	return SilentResult(capExecOutput(output, execMaxOutputChars))
}

// limitedBuffer caps output to prevent OOM from runaway commands.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (lb *limitedBuffer) Write(p []byte) (int, error) {
	if lb.truncated {
		return len(p), nil
	}
	remaining := lb.max - lb.buf.Len()
	if remaining <= 0 {
		lb.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		lb.buf.Write(p[:remaining])
		lb.truncated = true
		return len(p), nil
	}
	return lb.buf.Write(p)
}

func (lb *limitedBuffer) String() string {
	s := lb.buf.String()
	if lb.truncated {
		s += "\n[output truncated at 1MB]"
	}
	return s
}

func (lb *limitedBuffer) Len() int { return lb.buf.Len() }

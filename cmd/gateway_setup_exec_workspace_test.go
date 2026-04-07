package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

func testExecToolFromGatewaySetup(t *testing.T, workspace, dataDir string) *tools.ExecTool {
	t.Helper()

	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.Agents.Defaults.Workspace = workspace
	cfg.Tools.Browser.Enabled = false

	providerRegistry := providers.NewRegistry(nil)
	toolsReg, _, _, _, _, _, _, _, _, _, _ := setupToolRegistry(cfg, workspace, providerRegistry)

	execToolAny, ok := toolsReg.Get("exec")
	if !ok {
		t.Fatal("exec tool not registered")
	}
	execTool, ok := execToolAny.(*tools.ExecTool)
	if !ok {
		t.Fatalf("exec tool type = %T, want *tools.ExecTool", execToolAny)
	}
	return execTool
}

func TestSetupToolRegistryExecWorkspacePaths(t *testing.T) {
	dataDir := t.TempDir()
	workspace := filepath.Join(dataDir, "personal")
	teamWorkspace := filepath.Join(dataDir, "teams", "team-123")
	for _, dir := range []string{workspace, teamWorkspace} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}

	execTool := testExecToolFromGatewaySetup(t, workspace, dataDir)

	tests := []struct {
		name        string
		ctx         context.Context
		commandPath string
		wantDenied  bool
	}{
		{
			name:        "personal_workspace_uploads_allowed",
			ctx:         tools.WithToolWorkspace(context.Background(), workspace),
			commandPath: filepath.Join(workspace, ".uploads", "issue-739.png"),
		},
		{
			name:        "team_workspace_files_allowed",
			ctx:         tools.WithToolTeamWorkspace(tools.WithToolWorkspace(context.Background(), workspace), teamWorkspace),
			commandPath: filepath.Join(teamWorkspace, "issue-739.png"),
		},
		{
			name:        "unrelated_data_dir_path_denied",
			ctx:         tools.WithToolWorkspace(context.Background(), workspace),
			commandPath: filepath.Join(dataDir, "config.json"),
			wantDenied:  true,
		},
		{
			name:        "workspace_local_dotgoclaw_denied",
			ctx:         tools.WithToolWorkspace(context.Background(), workspace),
			commandPath: filepath.Join(workspace, ".goclaw", "secrets.json"),
			wantDenied:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := execTool.Execute(tc.ctx, map[string]any{
				"command": "printf '%s' " + tc.commandPath,
			})

			denied := strings.Contains(result.ForLLM, "command denied by safety policy")
			if denied != tc.wantDenied {
				t.Fatalf("denied = %v, want %v; output = %s", denied, tc.wantDenied, result.ForLLM)
			}
			if !tc.wantDenied && !strings.Contains(result.ForLLM, tc.commandPath) {
				t.Fatalf("expected output to contain %q, got: %s", tc.commandPath, result.ForLLM)
			}
		})
	}
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFirstRunTunnelMetadataAndEscapedWorkspace(t *testing.T) {
	state := t.TempDir()
	workspace := `C:\Users\Example User\projects\codexpc`
	config := fmt.Sprintf(
		"workspace = %q\nallowed_roots = [%q]\ntool_profile = \"core\"\ntunnel_profile = \"codex\"\ntunnel_id = \"tunnel_0123456789abcdef0123456789abcdef\"\norganization = \"Example\"\n",
		workspace,
		workspace,
	)
	if err := os.WriteFile(filepath.Join(state, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEXPC_STATE_DIR", state)
	t.Setenv("CODEXPC_WORKSPACE", "")
	t.Setenv("CODEXPC_ALLOWED_ROOTS", "")
	t.Setenv("CODEXPC_TOOL_PROFILE", "")

	settings, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Workspace != workspace {
		t.Fatalf("workspace = %q, want %q", settings.Workspace, workspace)
	}
	if len(settings.AllowedRoots) != 1 || settings.AllowedRoots[0] != workspace {
		t.Fatalf("allowed roots = %#v, want [%q]", settings.AllowedRoots, workspace)
	}
	if settings.TunnelProfile != "codex" {
		t.Fatalf("tunnel profile = %q", settings.TunnelProfile)
	}
	if settings.TunnelID != "tunnel_0123456789abcdef0123456789abcdef" {
		t.Fatalf("tunnel id = %q", settings.TunnelID)
	}
	if settings.Organization != "Example" {
		t.Fatalf("organization = %q", settings.Organization)
	}
}

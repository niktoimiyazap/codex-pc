package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Settings struct {
	StateDir                 string
	Workspace                string
	AllowedRoots             []string
	DefaultStartupTimeoutSec float64
	DefaultToolTimeoutSec    float64
	MaxOutputChars           int
	MCPInventoryTTLSec       float64
	ToolProfile              string
	TunnelProfile            string
	TunnelID                 string
	Organization             string
	LogLevel                 string
}

func Load() (Settings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Settings{}, err
	}
	state := os.Getenv("CODEXPC_STATE_DIR")
	if state == "" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			state = filepath.Join(local, "CodexPCConnector")
		} else {
			state = filepath.Join(home, ".codexpc")
		}
	}
	_ = os.MkdirAll(state, 0o755)

	s := Settings{
		StateDir: state, Workspace: home, AllowedRoots: []string{home},
		DefaultStartupTimeoutSec: 45, DefaultToolTimeoutSec: 120, MaxOutputChars: 100000,
		MCPInventoryTTLSec: 300, ToolProfile: "core", TunnelProfile: "codex", LogLevel: "INFO",
	}
	configPath := filepath.Join(state, "config.toml")
	if f, err := os.Open(configPath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(stripComment(scanner.Text()))
			if line == "" || strings.HasPrefix(line, "[") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			switch key {
			case "workspace":
				s.Workspace = unquote(val)
			case "allowed_roots":
				s.AllowedRoots = parseArray(val)
			case "default_startup_timeout_sec":
				if v, e := strconv.ParseFloat(val, 64); e == nil {
					s.DefaultStartupTimeoutSec = v
				}
			case "default_tool_timeout_sec":
				if v, e := strconv.ParseFloat(val, 64); e == nil {
					s.DefaultToolTimeoutSec = v
				}
			case "max_output_chars":
				if v, e := strconv.Atoi(val); e == nil {
					s.MaxOutputChars = v
				}
			case "mcp_inventory_ttl_sec":
				if v, e := strconv.ParseFloat(val, 64); e == nil {
					s.MCPInventoryTTLSec = v
				}
			case "tool_profile":
				s.ToolProfile = strings.ToLower(unquote(val))
			case "tunnel_profile":
				s.TunnelProfile = unquote(val)
			case "tunnel_id":
				s.TunnelID = unquote(val)
			case "organization":
				s.Organization = unquote(val)
			case "log_level":
				s.LogLevel = strings.ToUpper(unquote(val))
			}
		}
	}
	if v := os.Getenv("CODEXPC_WORKSPACE"); v != "" {
		s.Workspace = v
	}
	if v := os.Getenv("CODEXPC_ALLOWED_ROOTS"); v != "" {
		s.AllowedRoots = filepath.SplitList(v)
	}
	if v := os.Getenv("CODEXPC_TOOL_PROFILE"); v != "" {
		s.ToolProfile = strings.ToLower(v)
	}
	return s, nil
}

func stripComment(line string) string {
	quoted := false
	escaped := false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return line[:i]
		}
	}
	return line
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if decoded, err := strconv.Unquote(v); err == nil {
			return decoded
		}
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}

func parseArray(v string) []string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil
	}
	v = strings.TrimSpace(v[1 : len(v)-1])
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if x := unquote(strings.TrimSpace(p)); x != "" {
			out = append(out, x)
		}
	}
	return out
}

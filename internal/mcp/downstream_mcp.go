package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func probeCodexMCPTools(parent context.Context, name string) ([]any, error) {
	getCtx, cancelGet := context.WithTimeout(parent, 4*time.Second)
	defer cancelGet()
	get := exec.CommandContext(getCtx, "codex", "mcp", "get", name, "--json")
	get.Env = os.Environ()
	raw, err := get.Output()
	if err != nil {
		return nil, fmt.Errorf("read MCP config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode MCP config: %w", err)
	}
	transport, _ := cfg["transport"].(map[string]any)
	if transport == nil {
		return nil, fmt.Errorf("missing transport")
	}
	if stringValue(transport["type"]) != "stdio" {
		return nil, fmt.Errorf("tool inventory for %s transport is not implemented yet", stringValue(transport["type"]))
	}
	command := stringValue(transport["command"])
	if command == "" {
		return nil, fmt.Errorf("stdio command is empty")
	}
	commandArgs := stringSlice(transport["args"])
	command, commandArgs = normalizeInventoryCommand(command, commandArgs)
	probeCtx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, command, commandArgs...)
	if cwd := stringValue(transport["cwd"]); cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = os.Environ()
	if envMap, ok := transport["env"].(map[string]any); ok {
		for key, value := range envMap {
			cmd.Env = append(cmd.Env, key+"="+stringValue(value))
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	enc := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "codexpc-mcp-inventory", "version": "0.4.0"}}}); err != nil {
		return nil, err
	}
	if _, err := readMCPResponseCtx(probeCtx, scanner, stdout, 1); err != nil {
		return nil, fmt.Errorf("initialize %s: %w%s", name, err, stderrSuffix(stderr.String()))
	}
	if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	all := make([]any, 0)
	requestID := 2
	cursor := ""
	seenCursors := make(map[string]bool)
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": "tools/list", "params": params}); err != nil {
			return nil, err
		}
		resp, err := readMCPResponse(scanner, requestID)
		if err != nil {
			return nil, fmt.Errorf("tools/list %s: %w%s", name, err, stderrSuffix(stderr.String()))
		}
		result, _ := resp["result"].(map[string]any)
		if tools, ok := result["tools"].([]any); ok {
			all = append(all, tools...)
		}
		next := stringValue(result["nextCursor"])
		if next == "" {
			break
		}
		if next == cursor || seenCursors[next] {
			return nil, fmt.Errorf("tools/list %s returned a repeated pagination cursor", name)
		}
		seenCursors[next] = true
		cursor = next
		requestID++
	}
	return all, nil
}

func normalizeInventoryCommand(command string, args []string) (string, []string) {
	base := strings.ToLower(filepath.Base(command))
	if base != "uv.exe" && base != "uv" {
		return command, args
	}
	if len(args) < 4 || args[0] != "run" || args[1] != "--directory" {
		return command, args
	}
	dir := args[2]
	entry := args[3]
	python := filepath.Join(dir, ".venv", "Scripts", "python.exe")
	if _, err := os.Stat(python); err != nil {
		return command, args
	}
	if !filepath.IsAbs(entry) {
		entry = filepath.Join(dir, entry)
	}
	directArgs := append([]string{entry}, args[4:]...)
	return python, directArgs
}

func readMCPResponseCtx(ctx context.Context, scanner *bufio.Scanner, closer io.Closer, expectedID int) (map[string]any, error) {
	type result struct {
		msg map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() { msg, err := readMCPResponse(scanner, expectedID); ch <- result{msg: msg, err: err} }()
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-ctx.Done():
		_ = closer.Close()
		return nil, ctx.Err()
	}
}

func readMCPResponse(scanner *bufio.Scanner, expectedID int) (map[string]any, error) {
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg map[string]any
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		id, ok := numberAsInt(msg["id"])
		if !ok || int(id) != expectedID {
			continue
		}
		if rpcErr, ok := msg["error"].(map[string]any); ok {
			return nil, fmt.Errorf("rpc error: %s", stringValue(rpcErr["message"]))
		}
		return msg, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func stringSlice(v any) []string {
	if raw, ok := v.([]any); ok {
		out := make([]string, 0, len(raw))
		for _, item := range raw {
			out = append(out, stringValue(item))
		}
		return out
	}
	if raw, ok := v.([]string); ok {
		return raw
	}
	return nil
}

func stderrSuffix(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	if len(v) > 500 {
		v = v[len(v)-500:]
	}
	return "; stderr: " + v
}

func readCodexMCPConfig() ([]map[string]any, string, error) {
	cmd := exec.Command("codex", "mcp", "list", "--json")
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return nil, "codex mcp list --json", fmt.Errorf("read effective Codex MCP config: %w", err)
	}
	var raw []map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, "codex mcp list --json", fmt.Errorf("decode effective Codex MCP config: %w", err)
	}
	servers := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		server := map[string]any{
			"name":                item["name"],
			"enabled":             item["enabled"],
			"disabledReason":      item["disabled_reason"],
			"authStatus":          item["auth_status"],
			"startup_timeout_sec": item["startup_timeout_sec"],
			"tool_timeout_sec":    item["tool_timeout_sec"],
			"source":              "codex_effective_config",
		}
		if transport, ok := item["transport"].(map[string]any); ok {
			safe := map[string]any{"type": transport["type"]}
			for _, key := range []string{"command", "args", "cwd", "url", "bearer_token_env_var", "env_vars"} {
				if value, exists := transport[key]; exists && value != nil {
					safe[key] = value
				}
			}
			server["transport"] = safe
			server["command"] = safe["command"]
			server["url"] = safe["url"]
		}
		servers = append(servers, server)
	}
	return servers, "codex mcp list --json", nil
}

func tomlString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		if value[0] == '"' {
			var out string
			if err := json.Unmarshal([]byte(value), &out); err == nil {
				return out
			}
		}
		return value[1 : len(value)-1]
	}
	return value
}

func tomlStringArray(value string) []string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil
	}
	body := strings.TrimSpace(value[1 : len(value)-1])
	if body == "" {
		return []string{}
	}
	var out []string
	var token strings.Builder
	quote := byte(0)
	escaped := false
	flush := func() {
		v := strings.TrimSpace(token.String())
		if v != "" {
			out = append(out, tomlString(v))
		}
		token.Reset()
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if escaped {
			token.WriteByte(c)
			escaped = false
			continue
		}
		if quote == '"' && c == '\\' {
			token.WriteByte(c)
			escaped = true
			continue
		}
		if quote != 0 {
			token.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			token.WriteByte(c)
			continue
		}
		if c == ',' {
			flush()
			continue
		}
		token.WriteByte(c)
	}
	flush()
	return out
}

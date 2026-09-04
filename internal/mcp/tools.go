package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (t Tool) MarshalJSON() ([]byte, error) {
	type wireTool struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"inputSchema"`
		Annotations map[string]any `json:"annotations"`
	}
	return json.Marshal(wireTool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
		Annotations: toolAnnotations(t.Name),
	})
}

func toolAnnotations(name string) map[string]any {

	readOnly := map[string]bool{
		"connector_status":  true,
		"wait":              true,
		"read_rules":        true,
		"fs_read_file":      true,
		"fs_read_directory": true,
		"fs_search":         true,
		"command_inspect":   true,
		"command_poll":      true,
		"command_list":      true,
		"secret_vault":      true,
		"credential_value":  true,
		"mcp_discover":      true,
		"mcp_resource_read": true,
	}
	destructive := map[string]bool{

		"fs_remove":     true,
		"fs_write_file": true,

		"multi_tool": true,
	}
	idempotent := map[string]bool{
		"fs_create_directory":         true,
		"command_resize":              true,
		"command_terminate":           true,
		"command_emergency_terminate": true,
		"mcp_reload":                  true,
	}
	openWorld := map[string]bool{

		"command_inspect": true,
		"command_exec":    true,
		"shell_exec":      true,
		"computer":        true,
		"mcp_call":        true,
		"mcp_oauth_login": true,
	}
	return map[string]any{
		"readOnlyHint":    readOnly[name],
		"destructiveHint": destructive[name],
		"idempotentHint":  idempotent[name],
		"openWorldHint":   openWorld[name],
	}
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func toolRequiresSession(name string) bool {
	switch name {
	case "connector_status", "session_create", "session_list", "command_list", "command_emergency_terminate":
		return false
	default:
		return true
	}
}

func addSessionContextToTools(all []Tool) []Tool {
	for i := range all {
		if !toolRequiresSession(all[i].Name) {
			continue
		}
		props, _ := all[i].InputSchema["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
			all[i].InputSchema["properties"] = props
		}
		props["session_id"] = map[string]any{
			"type":        "string",
			"minLength":   1,
			"description": "Required CodexPC chat session id returned by session_create. Reuse the same id for all tools belonging to the same user conversation.",
		}
		required, _ := all[i].InputSchema["required"].([]string)
		found := false
		for _, item := range required {
			if item == "session_id" {
				found = true
				break
			}
		}
		if !found {
			required = append(required, "session_id")
		}
		all[i].InputSchema["required"] = required
		all[i].Description = "Requires an active CodexPC session_id. If no session exists for this conversation, call session_create first. " + all[i].Description
	}
	return all
}

func tools() []Tool {
	all := []Tool{
		{"connector_status", "Returns connector and original Codex app-server health metadata. This health check does not require a chat session.", objSchema(map[string]any{})},
		{"session_create", "MUST be the first CodexPC tool used when starting work for a new user conversation. Creates a named persistent CodexPC chat session and returns session_id. Choose a short useful name based on the user's task, then pass that session_id to every subsequent connector tool in this conversation. For project work, call read_rules immediately after session_create and before inspecting, editing, or running project commands.", objSchema(map[string]any{"name": map[string]any{"type": "string", "minLength": 1, "maxLength": 80, "description": "Short human-readable chat name derived from the user's current task."}}, "name")},
		{"session_list", "Lists existing CodexPC chat sessions. Use only when you need to recover or inspect existing session ids; for a new conversation prefer session_create.", objSchema(map[string]any{})},
		{"read_rules", "MUST be called at the start of project work, immediately after session_create and before inspecting, editing, building, deploying, or running project commands. Reads the applicable AGENTS.md rule chain: the canonical global rules plus AGENTS.md and .agents/AGENTS.md files from parent directories down to the target project. Pass path when the project or working directory is known; otherwise it uses the connector workspace. This tool is read-only and does not require the Codex app-server backend.", objSchema(map[string]any{"path": map[string]any{"type": "string", "description": "Optional project, working-directory, or file path used to resolve applicable AGENTS.md rules. Defaults to the connector workspace."}})},
		{"wait", "Pauses inside the current assistant turn and then returns control to the model. IMPORTANT approval protocol: when command_exec returns awaiting_approval, do not end the assistant response and do not ask the user to send another message. Call wait, then command_poll, and keep the same turn alive until the user approves/denies or the pending request expires. This tool has no side effects and does not create a terminal process.", objSchema(map[string]any{"seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 900, "default": 5, "description": "How many seconds to wait before returning. For frontend approval, prefer a reasonable wait such as 5-15 seconds, then poll; repeat if still pending."}, "reason": map[string]any{"type": "string", "maxLength": 160, "description": "Optional short reason for waiting, e.g. waiting for frontend approval or a long build."}})},
		{"secret_vault", "Lists safe metadata for credentials stored in the user's local CodexPC vault. This tool never returns stored credential values. Use the returned opaque id with command_exec.credential_refs for normal credential use.", objSchema(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"list"}}}, "action")},
		{"credential_value", "Reads a user-selected stored credential value for direct inspection. Use only when the user explicitly asks ChatGPT to inspect the exact saved value. The request requires explicit user approval before the value is returned. For normal command execution, use secret_vault plus command_exec.credential_refs instead.", objSchema(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"request", "poll"}}, "id": map[string]any{"type": "string", "maxLength": 128, "description": "Opaque credential id returned by secret_vault.list."}, "purpose": map[string]any{"type": "string", "minLength": 1, "maxLength": 240, "description": "Why direct inspection of this saved value is needed."}, "user_requested_exact_value": map[string]any{"type": "boolean", "description": "Must be true and only used when the user explicitly requested the exact stored value."}, "request_id": map[string]any{"type": "string", "maxLength": 128}}, "action")},
		{"multi_tool", "Preferred efficiency tool for batching two or more independent CodexPC operations. For filesystem work, batch the dedicated fs_* tools here rather than replacing them with terminal commands. Batch file reads, directory reads, independent edits/writes to different files, and other unrelated operations whenever dependencies do not require ordering. Only split work when one operation depends on another, user intent requires sequencing, or safety/consistency would be reduced. Duplicate mutations to the same target are rejected.", objSchema(map[string]any{"operations": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": objSchema(map[string]any{"tool": map[string]any{"type": "string"}, "arguments": map[string]any{"type": "object", "additionalProperties": true}}, "tool", "arguments")}, "parallel": map[string]any{"type": "boolean", "default": true}}, "operations")},
		{"fs_read_file", "Canonical tool for reading file contents. Prefer this over command_exec, shell_exec, type, cat, Get-Content, or similar terminal commands when the task is simply to read a known file. Uses the original Codex app-server fs/readFile method and supports line pagination: offset is the 1-indexed starting line and limit is the maximum number of lines to return. Prefer offset/limit for large files and multi_tool for multiple independent reads.", objSchema(map[string]any{"path": map[string]any{"type": "string"}, "encoding": map[string]any{"type": "string", "enum": []string{"utf-8", "utf-8-sig", "utf-16-le", "cp1251", "cp866", "base64"}, "default": "utf-8"}, "offset": map[string]any{"type": "integer", "minimum": 1, "default": 1, "description": "1-indexed line number to start reading from."}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "description": "Maximum number of lines to return."}}, "path")},
		{"fs_edit_file", "Canonical and preferred tool for modifying an existing text file. Use this instead of command_exec or shell_exec with PowerShell/sed/python/echo/redirection solely to edit file contents. Read the file first with fs_read_file, pass its sha256, and replace exact text fragments. If the file changed after the read, a targeted edit can still succeed when its fragment resolves unambiguously in the current file; replace_all still requires a fresh hash. Use multi_tool to batch independent edits to different files.", objSchema(map[string]any{"path": map[string]any{"type": "string"}, "expected_sha256": map[string]any{"type": "string", "minLength": 64, "maxLength": 64}, "encoding": map[string]any{"type": "string", "default": "utf-8"}, "edits": map[string]any{"type": "array", "minItems": 1, "items": objSchema(map[string]any{"old_text": map[string]any{"type": "string", "minLength": 1}, "new_text": map[string]any{"type": "string"}, "expected_count": map[string]any{"type": "integer", "minimum": 1, "default": 1}, "replace_all": map[string]any{"type": "boolean", "default": false}}, "old_text", "new_text")}, "dry_run": map[string]any{"type": "boolean", "default": false}}, "path", "expected_sha256", "edits")},
		{"fs_write_file", "Canonical and preferred tool for creating a file or intentionally replacing its complete contents. Use this instead of command_exec or shell_exec with Set-Content, Out-File, echo >, heredocs, Python one-liners, or other terminal-based file writing. Use fs_edit_file for targeted changes to an existing text file. If writing multiple independent files, prefer multi_tool.", objSchema(map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "encoding": map[string]any{"type": "string", "default": "utf-8"}}, "path", "content")},
		{"fs_read_directory", "Canonical tool for listing direct children of a known directory. Prefer this over command_exec or shell_exec with dir, ls, or Get-ChildItem when only a directory listing is needed. Uses the original Codex app-server. Prefer multi_tool for multiple independent directories.", objSchema(map[string]any{"path": map[string]any{"type": "string"}}, "path")},
		{"fs_search", "PRIMARY and canonical tool for local filesystem search. If the task is to locate local files/folders by name, use mode=name (the default). Use mode=content only when text inside files is actually needed; mode=both is more expensive on large trees. Do NOT use terminal search commands for local search. Searches recursively, skips common dependency/cache trees by default, and avoids reading obvious binary/non-text files for content search.", objSchema(map[string]any{"path": map[string]any{"type": "string", "description": "Directory to search recursively."}, "query": map[string]any{"type": "string", "minLength": 1, "description": "Substring or regex to search for."}, "mode": map[string]any{"type": "string", "enum": []string{"name", "content", "both"}, "default": "name"}, "case_sensitive": map[string]any{"type": "boolean", "default": false}, "regex": map[string]any{"type": "boolean", "default": false}, "glob": map[string]any{"type": "string", "description": "Optional filename glob such as *.go or *.ts."}, "include_hidden": map[string]any{"type": "boolean", "default": false}, "max_results": map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "default": 100}}, "path", "query")},
		{"fs_create_directory", "Creates a directory through original Codex app-server.", objSchema(map[string]any{"path": map[string]any{"type": "string"}, "recursive": map[string]any{"type": "boolean", "default": true}}, "path")},
		{"fs_copy", "Copies a file or directory through original Codex app-server.", objSchema(map[string]any{"source_path": map[string]any{"type": "string"}, "destination_path": map[string]any{"type": "string"}, "recursive": map[string]any{"type": "boolean", "default": false}}, "source_path", "destination_path")},
		{"fs_remove", "Removes a file or directory through original Codex app-server.", objSchema(map[string]any{"path": map[string]any{"type": "string"}, "recursive": map[string]any{"type": "boolean", "default": true}, "force": map[string]any{"type": "boolean", "default": true}}, "path")},
		{"command_inspect", "Preferred terminal tool for inspection-only commands: repository status, logs, searches, version checks, network diagnostics, and direct SSH remote reads. On Windows it uses the connector's native process runner and is not placed inside a Codex filesystem sandbox; read-only is a semantic contract, not a restricted execution profile. Direct nested SSH one-liners (for example ssh host 'journalctl ...', ssh host 'cat ...', or ssh host 'systemctl status ...') are expected and supported. Do NOT create a temporary local script merely to work around SSH or quoting for an inspection command; invoke ssh directly as argv. Use command_exec only when the requested operation intentionally mutates state. Choose timeout_ms yourself when a time budget is useful: it is a soft notification threshold, never a kill switch. If reached, the process keeps running and the result reports timeout_reached=true so you can decide whether to keep polling or terminate it explicitly.", objSchema(map[string]any{"command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}, "cwd": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "integer", "minimum": 1, "description": "Optional soft timeout chosen by the model in milliseconds. Reaching it does not terminate the process; the process remains running and snapshots report timeout_reached=true. Omit for no timeout notification."}, "output_bytes_cap": map[string]any{"type": "integer", "minimum": 0}, "yield_time_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 60000, "default": 10000}}, "command")},
		{"command_exec", "Terminal execution for tasks that inherently require running a program or command: builds, tests, package managers, git operations, servers, process control, system/network actions, and similar execution workflows. Do NOT use the terminal as a substitute for dedicated filesystem tools: use fs_read_file to read known files, fs_read_directory to list directories, fs_edit_file for targeted text edits, and fs_write_file to create or fully replace files. Avoid terminal-only wrappers such as Set-Content, Out-File, echo >, heredocs, or Python one-liners when the real task is just file I/O. Prefer command_inspect for genuinely read-only command-based inspection. CRITICAL approval protocol: if this tool returns status=awaiting_approval, do not end the assistant response, do not tell the user to come back or send another message, and do not treat the task as complete. Immediately use wait, then command_poll with the returned process_id, and continue wait -> poll in the SAME assistant turn until approval is resolved, denied, or expired. After approval, continue the original task automatically. Waits up to yield_time_ms for ordinary execution; if still running, returns process_id. Use background=true proactively for long-lived processes that are meant to keep running while you continue working: dev servers, watchers, bots, local services, tunnels, tail/follow commands, and similar persistent jobs. Background mode returns quickly with a process_id; continue other work and use command_poll only when you need fresh output or completion state. Follow sessions with command_poll, command_write, or command_terminate. If several independent operations are needed, prefer multi_tool.", objSchema(map[string]any{"command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}, "intent": map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "description": "Brief factual purpose of the terminal command, e.g. 'Inspect repository status' or 'Run project tests'."}, "safety_context": map[string]any{"type": "string", "minLength": 1, "maxLength": 300, "description": "Optional factual safety context for benign commands. Explain relevant constraints such as read-only inspection, workspace-scoped changes, or no network/system configuration changes. Never use this field to misrepresent a command."}, "require_approval": map[string]any{"type": "boolean", "default": false, "description": "Set true when the command contains or transmits API keys, tokens, passwords, credentials, private secrets, or similarly sensitive user data. Execution pauses until the user approves it in the CodexPC frontend."}, "approval_reason": map[string]any{"type": "string", "maxLength": 200, "description": "Short user-facing reason why approval is needed. Never include the secret value itself."}, "cwd": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "integer", "minimum": 1, "description": "Optional soft timeout chosen by the model in milliseconds. Reaching it never kills the process; snapshots report timeout_reached=true while it keeps running. Omit for no timeout notification."}, "disable_timeout": map[string]any{"type": "boolean", "default": false, "description": "Compatibility flag. When true, no soft timeout notification is armed."}, "output_bytes_cap": map[string]any{"type": "integer", "minimum": 0}, "disable_output_cap": map[string]any{"type": "boolean", "default": false}, "yield_time_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 60000, "default": 10000}, "background": map[string]any{"type": "boolean", "default": false, "description": "Run as a persistent background job and return quickly with process_id. Prefer true for dev servers, watchers, bots, services, tunnels, tail/follow jobs, and other processes expected to remain alive while you continue with other tools."}, "tty": map[string]any{"type": "boolean", "default": false}, "stream_stdin": map[string]any{"type": "boolean", "default": false}, "rows": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}, "cols": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}, "env": map[string]any{"type": "object", "additionalProperties": true}, "credential_refs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Map environment variable names to opaque credential ids returned by secret_vault.list. These are references, not credential values. The connector resolves them locally only after approval."}}, "command")},
		{"shell_exec", "Runs a raw shell script through one stable shell boundary instead of reconstructing nested argv quoting. Prefer this when shell semantics are themselves required: package-manager installs, build/deploy scripts, multi-command pipelines, &&/|| chains, environment expansion, or commands copied from a normal terminal. Do NOT choose shell_exec merely to read, create, overwrite, or edit files; use the dedicated fs_* tools for file I/O. Redirection is appropriate only as an intrinsic part of a broader shell workflow, not as a replacement for fs_write_file/fs_edit_file. On Windows, powershell uses -NoProfile -NonInteractive -ExecutionPolicy Bypass. The process is session-oriented and supports the same polling, stdin, timeout, approval, environment, and credential options as command_exec. Use background=true proactively for long-lived dev servers, watchers, bots, services, tunnels, and other processes that should stay alive while the model continues working.", objSchema(map[string]any{"script": map[string]any{"type": "string", "minLength": 1}, "shell": map[string]any{"type": "string", "enum": []string{"powershell", "pwsh", "cmd", "bash", "sh"}, "description": "Shell to execute the script. Defaults to powershell on Windows and sh elsewhere."}, "intent": map[string]any{"type": "string", "minLength": 1, "maxLength": 200}, "safety_context": map[string]any{"type": "string", "minLength": 1, "maxLength": 300}, "require_approval": map[string]any{"type": "boolean", "default": false}, "approval_reason": map[string]any{"type": "string", "maxLength": 200}, "cwd": map[string]any{"type": "string"}, "timeout_ms": map[string]any{"type": "integer", "minimum": 1, "description": "Optional soft timeout chosen by the model in milliseconds. Reaching it never kills the process; snapshots report timeout_reached=true while it keeps running. Omit for no timeout notification."}, "disable_timeout": map[string]any{"type": "boolean", "default": false, "description": "Compatibility flag. When true, no soft timeout notification is armed."}, "output_bytes_cap": map[string]any{"type": "integer", "minimum": 0}, "disable_output_cap": map[string]any{"type": "boolean", "default": false}, "yield_time_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 60000, "default": 10000}, "background": map[string]any{"type": "boolean", "default": false, "description": "Run as a persistent background job and return quickly with process_id. Prefer true for dev servers, watchers, bots, services, tunnels, tail/follow jobs, and other processes expected to remain alive while you continue with other tools."}, "stream_stdin": map[string]any{"type": "boolean", "default": false}, "env": map[string]any{"type": "object", "additionalProperties": true}, "credential_refs": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}}, "script")},
		{"command_poll", "Read-only poll for a command_exec session, including sessions waiting for frontend approval. Returns only new stdout/stderr plus current status. If status is still awaiting_approval, do not end the assistant response: call wait and command_poll again in the same turn. If approval resolves, continue the original task automatically. This tool never writes to stdin or changes the process.", objSchema(map[string]any{"process_id": map[string]any{"type": "string"}, "yield_time_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000, "default": 250}}, "process_id")},
		{"command_write", "Writes explicit stdin data to an existing interactive command_exec session. Use command_poll when no input needs to be sent. Set append_newline to send Enter after input, or close_stdin to close stdin.", objSchema(map[string]any{"process_id": map[string]any{"type": "string"}, "input": map[string]any{"type": "string"}, "append_newline": map[string]any{"type": "boolean", "default": false}, "close_stdin": map[string]any{"type": "boolean", "default": false}}, "process_id")},
		{"command_resize", "Resizes a running PTY-backed command_exec terminal.", objSchema(map[string]any{"process_id": map[string]any{"type": "string"}, "rows": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}, "cols": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "process_id", "rows", "cols")},
		{"command_terminate", "Terminates a running command_exec session owned by the current chat and returns its latest output/status.", objSchema(map[string]any{"process_id": map[string]any{"type": "string"}}, "process_id")},
		{"command_list", "Emergency control-plane tool. Lists connector-managed terminal processes across all chats so a stuck session cannot hide the process that caused it. Does not require a chat session.", objSchema(map[string]any{})},
		{"command_emergency_terminate", "Emergency control-plane tool. Terminates a connector-managed process regardless of chat ownership. On Windows native commands this kills the OS process tree directly and does not depend on the Codex app-server being responsive. Does not require a chat session.", objSchema(map[string]any{"process_id": map[string]any{"type": "string"}}, "process_id")},
		{"computer", "Controls Windows desktop: screenshot, screen_info, move, click, scroll, type, keypress.", objSchema(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"screenshot", "screen_info", "move", "click", "scroll", "type", "keypress"}}, "x": map[string]any{"type": "integer"}, "y": map[string]any{"type": "integer"}, "duration_ms": map[string]any{"type": "integer"}, "button": map[string]any{"type": "string"}, "clicks": map[string]any{"type": "integer"}, "delta_x": map[string]any{"type": "integer"}, "delta_y": map[string]any{"type": "integer"}, "text": map[string]any{"type": "string"}, "interval_ms": map[string]any{"type": "integer"}, "keys": map[string]any{}}, "action")},
		{"mcp_discover", "Lists configured MCP servers quickly. When server_name is provided, it automatically returns that server's complete tool inventory. Use refresh=true to enumerate complete tools for all matching servers. Tool lists are never truncated by limit; pagination is followed until the MCP server reports no next cursor. Sensitive env/header values are never returned.", objSchema(map[string]any{"query": map[string]any{"type": "string", "default": ""}, "server_name": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "Compatibility field for server/app result sizing; it never truncates MCP tools."}, "refresh": map[string]any{"type": "boolean", "default": false, "description": "Enumerate complete tool inventories for all matching MCP servers. A specific server_name enables this automatically."}})},
		{"mcp_call", "Calls a configured MCP tool through original Codex app-server mcpServer/tool/call.", objSchema(map[string]any{"server_name": map[string]any{"type": "string"}, "tool_name": map[string]any{"type": "string"}, "arguments": map[string]any{"type": "object", "additionalProperties": true}, "meta": map[string]any{"type": "object", "additionalProperties": true}}, "server_name", "tool_name")},
		{"mcp_resource_read", "Reads a configured MCP resource through original Codex app-server mcpServer/resource/read.", objSchema(map[string]any{"server_name": map[string]any{"type": "string"}, "uri": map[string]any{"type": "string"}}, "server_name", "uri")},
		{"mcp_reload", "Reloads Codex MCP configuration from disk through original app-server config/mcpServer/reload and invalidates the connector inventory cache.", objSchema(map[string]any{})},
		{"mcp_oauth_login", "Starts the original Codex app-server OAuth flow for a configured MCP server and returns its authorization URL. This applies to streamable HTTP MCP servers; stdio servers do not support OAuth login.", objSchema(map[string]any{"server_name": map[string]any{"type": "string"}, "scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}}, "server_name")},
	}
	all = addSessionContextToTools(all)

	if runtime.GOOS != "windows" {
		return all
	}

	unsupported := map[string]bool{
		"command_resize": true,
	}
	out := make([]Tool, 0, len(all)-len(unsupported))
	for _, tool := range all {
		if unsupported[tool.Name] {
			continue
		}
		if tool.Name == "command_exec" {
			props, _ := tool.InputSchema["properties"].(map[string]any)
			for _, name := range []string{"tty", "rows", "cols"} {
				delete(props, name)
			}
			tool.Description = "General Windows terminal execution for tasks that inherently require running a program or command (builds, tests, package managers, git, servers, process control, system/network actions). LOCAL FILE SEARCH IS NOT A TERMINAL TASK: when locating local files/folders or searching local file contents, you MUST use fs_search. Do NOT run rg, grep, findstr, Get-ChildItem -Recurse, Select-String, dir /s, recursive Python scripts, or equivalent local search commands; the connector may reject them with USE_FS_SEARCH. Likewise use fs_read_file, fs_read_directory, fs_edit_file, and fs_write_file for direct file I/O. To use a saved credential, pass credential_refs as environment-variable-name -> opaque credential id from Secret Vault from secret_vault.list; the plaintext is decrypted and injected locally only after frontend approval and is never returned to ChatGPT. If status=awaiting_approval, keep this assistant turn alive with wait then command_poll until approval resolves. Commands can outlive the MCP request deadline; follow running sessions with command_poll, command_write, or command_terminate. command_inspect uses the same native Windows runner for inspection-only work, including direct SSH remote reads; do not create temporary scripts as an SSH workaround. PTY/resize are not available on Windows yet."
		}
		if tool.Name == "command_inspect" {
			tool.Description = "Preferred Windows inspection runner for status, logs, diagnostics, and direct SSH remote reads. It is NOT the local filesystem search tool. For any local file/folder name search or local content search, MUST use fs_search; do not use rg, grep, findstr, Get-ChildItem -Recurse, Select-String, dir /s, or recursive scripts. Such local search commands may be rejected with USE_FS_SEARCH. It runs natively on Windows rather than inside a Codex filesystem sandbox. Invoke read-only SSH directly as argv; nested SSH reads such as journalctl/cat/systemctl status/grep are supported and are not affected by the local-search rule. Never create a temporary local script merely to bypass supposed command_inspect SSH restrictions."
		}
		out = append(out, tool)
	}
	return out
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]any, callID string) (any, error) {
	if toolNeedsBackend(name) {
		if err := s.waitBackend(ctx); err != nil {
			return nil, fmt.Errorf("BACKEND_NOT_READY: %w", err)
		}
	}
	var result any
	switch name {
	case "connector_status":
		backendState := "starting"
		backendError := ""
		if s.backendReady == nil {
			backendState = "ready"
		} else {
			select {
			case <-s.backendReady:
				s.backendMu.Lock()
				if s.backendErr != nil {
					backendState = "failed"
					backendError = s.backendErr.Error()
				} else {
					backendState = "ready"
				}
				s.backendMu.Unlock()
			default:
			}
		}
		result = map[string]any{"connector": "ok", "implementation": "go", "app_server_running": backendState == "ready", "backend_state": backendState, "backend_error": backendError, "uptime_sec": time.Since(s.started).Seconds(), "execution_backend": "codex app-server", "local_desktop_control": true, "session_mode": "required"}
	case "session_create":
		item, e := s.createSession(stringValue(args["name"]))
		if e != nil {
			return nil, e
		}
		if s.logger != nil {
			s.logger.Event("INFO", "chat_session_created", map[string]any{"session_id": item.ID, "session_name": item.Name, "created_at": item.CreatedAt})
		}
		result = map[string]any{"session_id": item.ID, "name": item.Name, "created_at": item.CreatedAt, "instruction": "Reuse this session_id for every subsequent CodexPC tool call in this conversation. Before project work, call read_rules for the target project or working directory."}
	case "session_list":
		items := s.listSessions()
		result = map[string]any{"count": len(items), "sessions": items}
	case "read_rules":
		r, e := s.readRules(args)
		if e != nil {
			return nil, e
		}
		result = r
	case "wait":
		seconds := int64(5)
		if n, ok := numberAsInt(args["seconds"]); ok {
			seconds = n
		}
		if seconds < 1 || seconds > 900 {
			return nil, fmt.Errorf("wait seconds must be between 1 and 900")
		}
		startedWait := time.Now()
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		result = map[string]any{"status": "waited", "seconds": seconds, "elapsed_sec": time.Since(startedWait).Seconds(), "reason": stringValue(args["reason"])}
	case "secret_vault":
		r, e := s.secretVault(args, callID)
		if e != nil {
			return nil, e
		}
		result = r
	case "credential_value":
		r, e := s.credentialValue(args, callID)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_inspect":
		argv := stringSlice(args["command"])
		if localSearchShouldUseFS(argv) {
			return nil, fmt.Errorf("USE_FS_SEARCH: local file/name/content search must use fs_search instead of terminal search commands")
		}
		inspectArgs := make(map[string]any, len(args))
		for k, v := range args {
			inspectArgs[k] = v
		}
		r, e := s.command(ctx, inspectArgs, callID)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_exec":
		argv := stringSlice(args["command"])
		if localSearchShouldUseFS(argv) {
			return nil, fmt.Errorf("USE_FS_SEARCH: local file/name/content search must use fs_search instead of terminal search commands")
		}
		r, e := s.command(ctx, args, callID)
		if e != nil {
			return nil, e
		}
		result = r
	case "shell_exec":
		r, e := s.shellExec(ctx, args, callID)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_poll":
		r, e := s.commandPoll(ctx, args)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_write":
		r, e := s.commandWrite(ctx, args)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_resize":
		r, e := s.commandResize(ctx, args)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_terminate":
		r, e := s.commandTerminate(ctx, args)
		if e != nil {
			return nil, e
		}
		result = r
	case "command_list":
		result = s.commandList()
	case "command_emergency_terminate":
		r, e := s.commandEmergencyTerminate(ctx, stringValue(args["process_id"]))
		if e != nil {
			return nil, e
		}
		result = r
	case "fs_read_file":
		p, e := s.resolvePath(args["path"])
		if e != nil {
			return nil, e
		}
		var r map[string]any
		if e = s.app.Request(ctx, "fs/readFile", map[string]any{"path": p}, &r); e != nil {
			return nil, e
		}
		raw, _ := base64.StdEncoding.DecodeString(stringValue(r["dataBase64"]))
		enc := stringValue(args["encoding"])
		if enc == "" {
			enc = "utf-8"
		}
		meta := map[string]any{"path": p, "sha256": hex.EncodeToString(sum(raw)), "size_bytes": len(raw)}
		if enc == "base64" {
			if _, hasOffset := args["offset"]; hasOffset {
				return nil, fmt.Errorf("offset/limit are only supported for text reads")
			}
			if _, hasLimit := args["limit"]; hasLimit {
				return nil, fmt.Errorf("offset/limit are only supported for text reads")
			}
			meta["data_base64"] = r["dataBase64"]
		} else {
			text := string(raw)
			meta["encoding"] = enc
			meta["final_newline"] = bytes.HasSuffix(raw, []byte("\n")) || bytes.HasSuffix(raw, []byte("\r"))
			if _, hasOffset := args["offset"]; hasOffset || args["limit"] != nil {
				lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
				if meta["final_newline"].(bool) && len(lines) > 0 && lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				total := len(lines)
				offset := int64(1)
				if n, ok := numberAsInt(args["offset"]); ok {
					offset = n
				}
				if offset < 1 {
					return nil, fmt.Errorf("offset must be >= 1")
				}
				start := int(offset - 1)
				if start > total {
					start = total
				}
				end := total
				if n, ok := numberAsInt(args["limit"]); ok {
					if n < 1 || n > 2000 {
						return nil, fmt.Errorf("limit must be between 1 and 2000")
					}
					if candidate := start + int(n); candidate < end {
						end = candidate
					}
				}
				meta["content"] = strings.Join(lines[start:end], "\n")
				meta["offset"] = offset
				meta["start_line"] = start + 1
				meta["end_line"] = end
				meta["total_lines"] = total
				meta["truncated"] = end < total
			} else {
				meta["content"] = text
			}
		}
		result = meta
	case "fs_search":
		r, e := s.fsSearch(ctx, args)
		if e != nil {
			return nil, e
		}
		result = r
	case "fs_write_file":
		p, e := s.resolvePath(args["path"])
		if e != nil {
			return nil, e
		}
		content := stringValue(args["content"])
		enc := stringValue(args["encoding"])
		var data []byte
		if enc == "base64" {
			data, e = base64.StdEncoding.DecodeString(content)
			if e != nil {
				return nil, e
			}
		} else {
			data = []byte(content)
		}
		var r any
		if e = s.app.Request(ctx, "fs/writeFile", map[string]any{"path": p, "dataBase64": base64.StdEncoding.EncodeToString(data)}, &r); e != nil {
			return nil, e
		}
		result = map[string]any{"path": p, "written": true, "size_bytes": len(data), "result": r}
	case "fs_read_directory":
		p, e := s.resolvePath(args["path"])
		if e != nil {
			return nil, e
		}
		var r any
		if e = s.app.Request(ctx, "fs/readDirectory", map[string]any{"path": p}, &r); e != nil {
			return nil, e
		}
		result = r
	case "fs_create_directory":
		p, e := s.resolvePath(args["path"])
		if e != nil {
			return nil, e
		}
		var r any
		if e = s.app.Request(ctx, "fs/createDirectory", map[string]any{"path": p, "recursive": boolValue(args["recursive"], true)}, &r); e != nil {
			return nil, e
		}
		result = r
	case "fs_copy":
		a, e := s.resolvePath(args["source_path"])
		if e != nil {
			return nil, e
		}
		b, e := s.resolvePath(args["destination_path"])
		if e != nil {
			return nil, e
		}
		var r any
		if e = s.app.Request(ctx, "fs/copy", map[string]any{"sourcePath": a, "destinationPath": b, "recursive": boolValue(args["recursive"], false)}, &r); e != nil {
			return nil, e
		}
		result = r
	case "fs_remove":
		p, e := s.resolvePath(args["path"])
		if e != nil {
			return nil, e
		}
		var r any
		if e = s.app.Request(ctx, "fs/remove", map[string]any{"path": p, "recursive": boolValue(args["recursive"], true), "force": boolValue(args["force"], true)}, &r); e != nil {
			return nil, e
		}
		result = r
	case "fs_edit_file":
		r, e := s.editFile(args)
		if e != nil {
			return nil, e
		}
		result = r
	case "mcp_discover":
		r, e := s.discover(ctx, args)
		if e != nil {
			return nil, e
		}
		result = r
	case "mcp_call":
		tid, e := s.ensureThread(ctx)
		if e != nil {
			return nil, e
		}
		p := map[string]any{"threadId": tid, "server": stringValue(args["server_name"]), "tool": stringValue(args["tool_name"]), "arguments": mapValue(args["arguments"])}
		if meta := mapValue(args["meta"]); len(meta) > 0 {
			p["_meta"] = meta
		}
		var r any
		if e = s.app.Request(ctx, "mcpServer/tool/call", p, &r); e != nil {
			return nil, e
		}
		result = r
	case "mcp_resource_read":
		var r any
		if e := s.app.Request(ctx, "mcpServer/resource/read", map[string]any{"server": stringValue(args["server_name"]), "uri": stringValue(args["uri"])}, &r); e != nil {
			return nil, e
		}
		result = r
	case "mcp_reload":
		var r any
		if e := s.app.Request(ctx, "config/mcpServer/reload", map[string]any{}, &r); e != nil {
			return nil, e
		}
		s.invalidateInventory()
		result = map[string]any{"reloaded": true}
	case "mcp_oauth_login":
		p := map[string]any{"name": stringValue(args["server_name"])}
		if scopes, ok := args["scopes"].([]any); ok && len(scopes) > 0 {
			p["scopes"] = scopes
		}
		if timeout, ok := numberAsInt(args["timeout_sec"]); ok && timeout > 0 {
			p["timeoutSecs"] = timeout
		}
		var r any
		if err := s.app.Request(ctx, "mcpServer/oauth/login", p, &r); err != nil {
			return nil, err
		}
		s.invalidateInventory()
		result = r
	case "computer":
		r, e := s.computer(ctx, args)
		if e != nil {
			return nil, e
		}
		if img, ok := r["_image"].(string); ok {
			delete(r, "_image")
			return map[string]any{"content": []map[string]any{{"type": "image", "data": img, "mimeType": "image/png"}, {"type": "text", "text": mustJSON(r)}}, "structuredContent": r, "isError": false}, nil
		}
		result = r
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	return toolResult(result), nil
}

package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/niktoimiyazap/codexpc-connector/internal/appserver"
	"github.com/niktoimiyazap/codexpc-connector/internal/computer"
	logpkg "github.com/niktoimiyazap/codexpc-connector/internal/logging"
)

const ProtocolVersion = "2025-06-18"

var commandProcessSequence uint64

func nextCommandProcessID() string {
	return fmt.Sprintf("go-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&commandProcessSequence, 1))
}

type Server struct {
	app                 *appserver.Client
	streams             *appserver.StreamRegistry
	in                  *bufio.Scanner
	out                 io.Writer
	writeMu             sync.Mutex
	threadMu            sync.Mutex
	threadID            string
	started             time.Time
	workspace           string
	allowedRoots        []string
	logger              *logpkg.Logger
	inventoryMu         sync.Mutex
	inventory           []any
	inventoryAt         time.Time
	inventoryRefreshing bool
	commandsMu          sync.Mutex
	commands            map[string]*commandSession
	sessionsMu          sync.Mutex
	sessions            map[string]chatSession
	backendMu           sync.Mutex
	backendReady        chan struct{}
	backendErr          error
	backendOnce         sync.Once
}

type chatSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type commandSession struct {
	mu             sync.Mutex
	processID      string
	callID         string
	chatSessionID  string
	stdout         bytes.Buffer
	stderr         bytes.Buffer
	stdoutRead     int
	stderrRead     int
	capReached     bool
	yielded        bool
	started        time.Time
	lastOutput     time.Time
	outputBytes    int64
	outputCap      int64
	done           chan struct{}
	result         map[string]any
	err            error
	timedOut       bool
	timeoutMs      int64
	unregister     func()
	local          bool
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	approvalState  string
	approvalID     string
	approvalReason string
}

type commandSessionWriter struct {
	session *commandSession
	stderr  bool
	onWrite func(stream string, data []byte)
}

func (w commandSessionWriter) Write(p []byte) (int, error) {
	originalLen := len(p)
	w.session.mu.Lock()
	w.session.lastOutput = time.Now()
	w.session.outputBytes += int64(originalLen)
	stored := p
	if w.session.outputCap > 0 {
		storedBytes := int64(w.session.stdout.Len() + w.session.stderr.Len())
		remaining := w.session.outputCap - storedBytes
		if remaining <= 0 {
			stored = nil
			w.session.capReached = true
		} else if int64(len(stored)) > remaining {
			stored = stored[:remaining]
			w.session.capReached = true
		}
	}
	stream := "stdout"
	if w.stderr {
		stream = "stderr"
		if len(stored) > 0 {
			_, _ = w.session.stderr.Write(stored)
		}
	} else if len(stored) > 0 {
		_, _ = w.session.stdout.Write(stored)
	}
	w.session.mu.Unlock()
	if w.onWrite != nil && len(stored) > 0 {
		w.onWrite(stream, append([]byte(nil), stored...))
	}
	// Never propagate the UI/log retention cap back to the child process as a
	// short write. The process keeps running normally even after we stop retaining
	// additional output in memory.
	return originalLen, nil
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
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
	// MCP annotations describe tool semantics rather than generic mutation or
	// capability risk. Keep these explicit so benign local operations are not
	// mislabeled as destructive by clients performing pre-dispatch checks.
	readOnly := map[string]bool{
		"connector_status":  true,
		"wait":              true,
		"read_rules":        true,
		"fs_read_file":      true,
		"fs_read_directory": true,
		"fs_search":         true,
		"command_inspect":   true,
		"command_poll":      true,
		"secret_vault":      true,
		"credential_value":  true,
		"mcp_discover":      true,
		"mcp_resource_read": true,
	}
	destructive := map[string]bool{
		// These can directly delete or replace existing user data.
		"fs_remove":     true,
		"fs_write_file": true,
		// A batch may contain destructive operations, so the static annotation
		// must conservatively cover that possibility.
		"multi_tool": true,
	}
	idempotent := map[string]bool{
		"fs_create_directory": true,
		"command_resize":      true,
		"command_terminate":   true,
		"mcp_reload":          true,
	}
	openWorld := map[string]bool{
		// These can cross the local-machine trust boundary depending on arguments.
		// command_inspect is still read-only, but SSH/network inspection is an
		// open-world read. Marking it closed-world makes clients over-constrain
		// perfectly valid remote reads and encourages pointless local wrappers.
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

func NewServer(app *appserver.Client, streams *appserver.StreamRegistry, logger *logpkg.Logger, input io.Reader, output io.Writer) *Server {
	s := bufio.NewScanner(input)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	home, _ := os.UserHomeDir()
	workspace := os.Getenv("CODEXPC_WORKSPACE")
	if workspace == "" {
		workspace = home
	}
	workspace, _ = filepath.Abs(workspace)
	roots := []string{home}
	if raw := os.Getenv("CODEXPC_ALLOWED_ROOTS"); raw != "" {
		roots = filepath.SplitList(raw)
	}
	for i := range roots {
		roots[i], _ = filepath.Abs(roots[i])
	}
	srv := &Server{app: app, streams: streams, in: s, out: output, started: time.Now(), workspace: workspace, allowedRoots: roots, logger: logger, commands: make(map[string]*commandSession), sessions: make(map[string]chatSession), backendReady: make(chan struct{})}
	srv.loadInventoryCache()
	srv.loadSessions()
	return srv
}

func (s *Server) MarkBackendReady(err error) {
	if s == nil {
		return
	}
	s.backendOnce.Do(func() {
		s.backendMu.Lock()
		s.backendErr = err
		s.backendMu.Unlock()
		if s.backendReady != nil {
			close(s.backendReady)
		}
	})
}

func (s *Server) waitBackend(ctx context.Context) error {
	if s == nil || s.backendReady == nil {
		return nil
	}
	select {
	case <-s.backendReady:
		s.backendMu.Lock()
		err := s.backendErr
		s.backendMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func toolNeedsBackend(name string) bool {
	if strings.HasPrefix(name, "fs_") || strings.HasPrefix(name, "mcp_") {
		return true
	}
	if runtime.GOOS != "windows" {
		switch name {
		case "command_exec", "command_inspect", "shell_exec", "command_write", "command_resize", "command_terminate":
			return true
		}
	}
	return false
}

func stateDirectory() string {
	state := os.Getenv("CODEXPC_STATE_DIR")
	if state == "" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			state = filepath.Join(local, "CodexPCConnector")
		}
	}
	return state
}

func (s *Server) sessionsPath() string {
	state := stateDirectory()
	if state == "" {
		return ""
	}
	return filepath.Join(state, "sessions.json")
}

func (s *Server) deletedSessionPath(id string) string {
	state := stateDirectory()
	if state == "" || id == "" {
		return ""
	}
	return filepath.Join(state, "deleted-sessions", id)
}

func (s *Server) isSessionDeleted(id string) bool {
	path := s.deletedSessionPath(id)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (s *Server) loadSessions() {
	path := s.sessionsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var payload struct {
		Version  int           `json:"version"`
		Sessions []chatSession `json:"sessions"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Version != 1 {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	for _, item := range payload.Sessions {
		if item.ID != "" && item.Name != "" && !s.isSessionDeleted(item.ID) {
			s.sessions[item.ID] = item
		}
	}
}

func (s *Server) saveSessionsLocked() error {
	path := s.sessionsPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	items := make([]chatSession, 0, len(s.sessions))
	for id, item := range s.sessions {
		if s.isSessionDeleted(id) {
			delete(s.sessions, id)
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	payload, err := json.MarshalIndent(map[string]any{"version": 1, "sessions": items}, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	// Windows does not reliably replace an existing destination with Rename.
	// Remove the old metadata file while holding sessionsMu, then atomically
	// install the fully written replacement.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Server) createSession(name string) (chatSession, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return chatSession{}, fmt.Errorf("session name is required")
	}
	if len([]rune(name)) > 80 {
		return chatSession{}, fmt.Errorf("session name must be at most 80 characters")
	}
	now := time.Now()
	item := chatSession{
		ID:        fmt.Sprintf("session-%d", now.UnixNano()),
		Name:      name,
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.sessions[item.ID] = item
	if err := s.saveSessionsLocked(); err != nil {
		delete(s.sessions, item.ID)
		return chatSession{}, fmt.Errorf("persist session: %w", err)
	}
	return item, nil
}

func (s *Server) sessionByID(id string) (chatSession, bool) {
	if s.isSessionDeleted(id) {
		return chatSession{}, false
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	item, ok := s.sessions[id]
	return item, ok
}

func (s *Server) touchSession(id string) (chatSession, error) {
	if s.isSessionDeleted(id) {
		s.sessionsMu.Lock()
		delete(s.sessions, id)
		s.sessionsMu.Unlock()
		return chatSession{}, fmt.Errorf("UNKNOWN_SESSION: create a session with session_create before using connector tools")
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	item, ok := s.sessions[id]
	if !ok {
		return chatSession{}, fmt.Errorf("UNKNOWN_SESSION: create a session with session_create before using connector tools")
	}
	previous := item
	item.UpdatedAt = time.Now().Format(time.RFC3339)
	s.sessions[id] = item
	if err := s.saveSessionsLocked(); err != nil {
		s.sessions[id] = previous
		return chatSession{}, fmt.Errorf("persist session activity: %w", err)
	}
	return item, nil
}

func (s *Server) listSessions() []chatSession {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	items := make([]chatSession, 0, len(s.sessions))
	for id, item := range s.sessions {
		if s.isSessionDeleted(id) {
			delete(s.sessions, id)
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	return items
}

func (s *Server) inventoryPath() string {
	state := stateDirectory()
	if state == "" {
		return ""
	}
	return filepath.Join(state, "mcp_inventory.json")
}

func (s *Server) loadInventoryCache() {
	path := s.inventoryPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var payload struct {
		Version   int     `json:"version"`
		UpdatedAt float64 `json:"updated_at"`
		Servers   []any   `json:"servers"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Version != 1 || len(payload.Servers) == 0 {
		return
	}
	s.inventoryMu.Lock()
	s.inventory = payload.Servers
	s.inventoryAt = time.Unix(int64(payload.UpdatedAt), 0)
	s.inventoryMu.Unlock()
}

func (s *Server) saveInventoryCache(servers []any) {
	now := time.Now()
	s.inventoryMu.Lock()
	s.inventory = servers
	s.inventoryAt = now
	s.inventoryMu.Unlock()

	path := s.inventoryPath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	payload := map[string]any{"version": 1, "updated_at": float64(now.Unix()), "servers": servers}
	if data, err := json.Marshal(payload); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

func (s *Server) cachedInventory() ([]any, time.Time, bool) {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	if len(s.inventory) == 0 {
		return nil, time.Time{}, false
	}
	for _, item := range s.inventory {
		server, _ := item.(map[string]any)
		if server == nil {
			continue
		}
		if _, ok := server["tools"]; ok {
			return s.inventory, s.inventoryAt, true
		}
	}
	return nil, time.Time{}, false
}

func (s *Server) refreshInventoryInBackground() {
	s.inventoryMu.Lock()
	if s.inventoryRefreshing {
		s.inventoryMu.Unlock()
		return
	}
	s.inventoryRefreshing = true
	s.inventoryMu.Unlock()
	go func() {
		defer func() {
			s.inventoryMu.Lock()
			s.inventoryRefreshing = false
			s.inventoryMu.Unlock()
		}()
		_, _ = s.discover(context.Background(), map[string]any{"refresh": true, "_force_inventory_refresh": true})
	}()
}

func (s *Server) Serve(ctx context.Context) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for s.in.Scan() {
		select {
		case <-serveCtx.Done():
			return serveCtx.Err()
		default:
		}
		line := bytes.TrimSpace(bytes.TrimPrefix(append([]byte(nil), s.in.Bytes()...), []byte{0xEF, 0xBB, 0xBF}))
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if len(msg.ID) == 0 {
			continue
		}

		// MCP requests must not block the stdio reader. A long-running tools/call
		// used to prevent initialize/ping from being read at all, which made the
		// tunnel time out and tear down the pipe. Handle each request independently
		// while serializing only response writes through writeMu.
		request := msg
		go func() {
			result, err := s.handle(serveCtx, request.Method, request.Params)
			resp := rpcResponse{JSONRPC: "2.0", ID: request.ID}
			if err != nil {
				resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			} else {
				resp.Result = result
			}
			if err := s.write(resp); err != nil && serveCtx.Err() == nil && s.logger != nil {
				s.logger.Event("WARN", "mcp_response_write_failed", map[string]any{
					"method": request.Method,
					"error":  err.Error(),
				})
			}
		}()
	}
	cancel()
	return s.in.Err()
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
	case "connector_status", "session_create", "session_list":
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
		{"command_terminate", "Terminates a running command_exec session and returns its latest output/status.", objSchema(map[string]any{"process_id": map[string]any{"type": "string"}}, "process_id")},
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

	// On Windows, general command_exec uses the connector's native process
	// session runner so long-lived commands can outlive the MCP request deadline.
	// Poll/write/terminate are therefore real capabilities. PTY resize remains
	// unavailable until the native runner gets ConPTY support.
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

func (s *Server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "codexpc-go", "title": "CodexPC Go", "version": "0.4.0-dev"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var req struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		started := time.Now()
		callID := fmt.Sprintf("go-call-%d", started.UnixNano())
		var activeSession chatSession
		if toolRequiresSession(req.Name) {
			sessionID := strings.TrimSpace(stringValue(req.Arguments["session_id"]))
			if sessionID == "" {
				return nil, fmt.Errorf("SESSION_REQUIRED: call session_create with a short name before using %s", req.Name)
			}
			var sessionErr error
			activeSession, sessionErr = s.touchSession(sessionID)
			if sessionErr != nil {
				return nil, sessionErr
			}
		}
		if s.logger != nil {
			data := map[string]any{
				"tool": req.Name, "call_id": callID,
				"argument_count": len(req.Arguments),
				"argument_keys":  mapKeys(req.Arguments),
				"input_preview":  logJSON(redactSensitive(req.Arguments), 20000),
			}
			if activeSession.ID != "" {
				data["session_id"] = activeSession.ID
				data["session_name"] = activeSession.Name
			}
			copyTargetFields(data, req.Arguments)
			s.logger.Event("INFO", "chatgpt_tool_call_started", data)
		}
		var result any
		var err error
		if req.Name == "multi_tool" {
			result, err = s.multiTool(ctx, req.Arguments, callID)
		} else {
			result, err = s.callTool(ctx, req.Name, req.Arguments, callID)
		}
		if s.logger != nil {
			level, message := "INFO", "chatgpt_tool_call_succeeded"
			data := map[string]any{"tool": req.Name, "call_id": callID, "duration_ms": float64(time.Since(started).Microseconds()) / 1000}
			if activeSession.ID != "" {
				data["session_id"] = activeSession.ID
				data["session_name"] = activeSession.Name
			}
			if err != nil {
				level, message = "ERROR", "chatgpt_tool_call_failed"
				data["error_preview"] = err.Error()
			} else {
				payload := logResultPayload(result)
				if req.Name == "secret_vault" || req.Name == "credential_value" {
					payload = redactSecretVaultResult(payload)
				}
				previewLimit := 20000
				if req.Name == "read_rules" {
					previewLimit = 100000
				}
				data["output_preview"] = logJSON(redactSensitive(payload), previewLimit)
				copyResultFields(data, payload)
				if m, ok := payload.(map[string]any); ok && stringValue(m["status"]) == "running" {
					message = "chatgpt_tool_call_yielded"
					data["process_id"] = m["process_id"]
				}
			}
			s.logger.Event(level, message, data)
		}
		return result, err
	default:
		return nil, fmt.Errorf("unsupported method: %s", method)
	}
}

func (s *Server) resolvePath(v any) (string, error) {
	raw, ok := v.(string)
	if !ok || raw == "" {
		return "", fmt.Errorf("path required")
	}
	if strings.HasPrefix(raw, "~") {
		home, _ := os.UserHomeDir()
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~\\/"))
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(s.workspace, raw)
	}
	p, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	for _, r := range s.allowedRoots {
		rel, e := filepath.Rel(r, p)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return p, nil
		}
	}
	return "", fmt.Errorf("path outside allowed roots: %s", p)
}

var sensitiveKeyRE = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|secret|authorization|private[_-]?key|client[_-]?secret|access[_-]?key|cookie)`)
var sensitiveValueRE = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{12,}|Bearer\s+[A-Za-z0-9._~+/-]{12,})`)
var inlineSecretAssignRE = regexp.MustCompile(`(?i)((?:api[_-]?key|access[_-]?token|auth[_-]?token|token|password|passwd|secret|authorization|private[_-]?key|client[_-]?secret)\s*[=:]\s*)([^\s;,&|]+)`)

func containsSensitive(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if sensitiveKeyRE.MatchString(k) && fmt.Sprint(value) != "" {
				return true
			}
			if containsSensitive(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if containsSensitive(value) {
				return true
			}
		}
	case string:
		return sensitiveValueRE.MatchString(x) || inlineSecretAssignRE.MatchString(x)
	}
	return false
}

func redactString(s string) string {
	s = sensitiveValueRE.ReplaceAllString(s, "[REDACTED]")
	s = inlineSecretAssignRE.ReplaceAllString(s, `${1}[REDACTED]`)
	return s
}

func commandNeedsApproval(args map[string]any) bool {
	if boolValue(args["_approval_granted"], false) {
		return false
	}
	// read_only describes filesystem/process side effects; it is not a privacy
	// bypass. Explicit approval and sensitive-data detection must still pause
	// read-only commands before credentials or private data are revealed.
	return boolValue(args["require_approval"], false) || len(credentialRefMap(args)) > 0 || containsSensitive(args)
}

func redactSensitive(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if sensitiveKeyRE.MatchString(k) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redactSensitive(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redactSensitive(value)
		}
		return out
	case string:
		return redactString(x)
	default:
		return v
	}
}

func connectorStateDir() string {
	if state := os.Getenv("CODEXPC_STATE_DIR"); state != "" {
		return state
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "CodexPCConnector")
	}
	return filepath.Join(os.TempDir(), "CodexPCConnector")
}

func secretVaultDir() string {
	return filepath.Join(connectorStateDir(), "secrets")
}

func secretVaultFile() string {
	return filepath.Join(secretVaultDir(), "vault.json")
}

func secretRequestPath(id string) string {
	return filepath.Join(secretVaultDir(), "requests", id+".json")
}

func secretResponsePath(id string) string {
	return filepath.Join(secretVaultDir(), "responses", id+".json")
}

type secretVaultRecord struct {
	ID          string `json:"id,omitempty"`
	Title       string `json:"title,omitempty"`
	Name        string `json:"name,omitempty"`
	Label       string `json:"label,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Hint        string `json:"hint,omitempty"`
	LastPurpose string `json:"last_purpose,omitempty"`
	Ciphertext  string `json:"ciphertext,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	LastUsedAt  string `json:"last_used_at,omitempty"`
	UseCount    int    `json:"use_count,omitempty"`
}

func loadSecretVaultMetadata() ([]secretVaultRecord, error) {
	data, err := os.ReadFile(secretVaultFile())
	if errors.Is(err, os.ErrNotExist) {
		return []secretVaultRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		Secrets []secretVaultRecord `json:"secrets"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("invalid secret vault metadata: %w", err)
	}
	return payload.Secrets, nil
}

func saveSecretVaultMetadata(records []secretVaultRecord) error {
	payload := struct {
		Version int                 `json:"version"`
		Secrets []secretVaultRecord `json:"secrets"`
	}{Version: 1, Secrets: records}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(secretVaultDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(secretVaultFile(), data, 0o600)
}

func credentialRefMap(args map[string]any) map[string]any {
	refs, _ := args["credential_refs"].(map[string]any)
	return refs
}

func secretRefTitles(args map[string]any) []string {
	refs := credentialRefMap(args)
	if len(refs) == 0 {
		return nil
	}
	records, err := loadSecretVaultMetadata()
	if err != nil {
		return nil
	}
	byID := make(map[string]secretVaultRecord, len(records))
	for _, record := range records {
		id := record.ID
		if id == "" {
			id = record.Name
		}
		byID[strings.ToLower(id)] = record
	}
	seen := map[string]bool{}
	var titles []string
	for _, raw := range refs {
		id := strings.TrimSpace(fmt.Sprint(raw))
		record, ok := byID[strings.ToLower(id)]
		if !ok {
			continue
		}
		title := strings.TrimSpace(record.Title)
		if title == "" {
			title = strings.TrimSpace(record.Kind)
		}
		if title == "" {
			title = "Saved secret"
		}
		if !seen[strings.ToLower(title)] {
			seen[strings.ToLower(title)] = true
			titles = append(titles, title)
		}
	}
	return titles
}

func (s *Server) injectSecretRefs(args map[string]any) error {
	refs := credentialRefMap(args)
	if len(refs) == 0 {
		return nil
	}
	records, err := loadSecretVaultMetadata()
	if err != nil {
		return err
	}
	byID := make(map[string]int, len(records))
	for i, record := range records {
		id := record.ID
		if id == "" {
			id = record.Name
		}
		if id != "" {
			byID[strings.ToLower(id)] = i
		}
	}
	env, _ := args["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	purpose := strings.TrimSpace(stringValue(args["intent"]))
	if purpose == "" {
		purpose = strings.TrimSpace(stringValue(args["approval_reason"]))
	}
	if purpose == "" {
		purpose = "Used by command_exec"
	}
	now := time.Now().Format(time.RFC3339)
	for envName, rawID := range refs {
		envName = strings.TrimSpace(envName)
		if envName == "" {
			return fmt.Errorf("credential_refs contains an empty environment variable name")
		}
		id := strings.TrimSpace(fmt.Sprint(rawID))
		idx, ok := byID[strings.ToLower(id)]
		if !ok {
			return fmt.Errorf("secret id %q is not saved in CodexPC Secret Vault", id)
		}
		plain, err := unprotectSecret(records[idx].Ciphertext)
		if err != nil {
			return fmt.Errorf("decrypt saved secret %q: %w", id, err)
		}
		env[envName] = plain
		records[idx].LastUsedAt = now
		records[idx].LastPurpose = purpose
		records[idx].UseCount++
	}
	args["env"] = env
	if err := saveSecretVaultMetadata(records); err != nil {
		return fmt.Errorf("update Secret Vault usage metadata: %w", err)
	}
	return nil
}

func redactSecretVaultResult(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, value := range m {
		if k == "secret" || k == "value" {
			out[k] = "[REDACTED]"
		} else {
			out[k] = value
		}
	}
	return out
}

func (s *Server) secretVault(args map[string]any, callID string) (map[string]any, error) {
	action := strings.ToLower(strings.TrimSpace(stringValue(args["action"])))
	if action != "list" {
		return nil, fmt.Errorf("secret_vault action must be: list")
	}
	records, err := loadSecretVaultMetadata()
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		id := record.ID
		if id == "" {
			id = record.Name
		}
		items = append(items, map[string]any{"id": id, "title": record.Title, "kind": record.Kind, "hint": record.Hint, "last_purpose": record.LastPurpose, "created_at": record.CreatedAt, "last_used_at": record.LastUsedAt, "use_count": record.UseCount})
	}
	return map[string]any{"status": "ok", "count": len(items), "secrets": items}, nil
}

func (s *Server) credentialValue(args map[string]any, callID string) (map[string]any, error) {
	action := strings.ToLower(strings.TrimSpace(stringValue(args["action"])))
	switch action {
	case "request":
		if !boolValue(args["user_requested_exact_value"], false) {
			return nil, fmt.Errorf("direct value access requires user_requested_exact_value=true after an explicit user request")
		}
		id := strings.TrimSpace(stringValue(args["id"]))
		purpose := strings.TrimSpace(stringValue(args["purpose"]))
		if id == "" || purpose == "" {
			return nil, fmt.Errorf("id and purpose are required")
		}
		records, err := loadSecretVaultMetadata()
		if err != nil {
			return nil, err
		}
		var title string
		found := false
		for _, record := range records {
			rid := record.ID
			if rid == "" {
				rid = record.Name
			}
			if strings.EqualFold(rid, id) {
				title = record.Title
				if title == "" {
					title = record.Kind
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("credential id %q is not saved in CodexPC vault", id)
		}
		requestID := fmt.Sprintf("secret-%d", time.Now().UnixNano())
		request := map[string]any{"request_id": requestID, "id": id, "title": title, "purpose": purpose, "mode": "reveal_to_model", "call_id": callID, "created_at": time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(request)
		if err := os.MkdirAll(filepath.Dir(secretRequestPath(requestID)), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretRequestPath(requestID), data, 0o600); err != nil {
			return nil, err
		}
		_ = os.Remove(secretResponsePath(requestID))
		return map[string]any{"status": "awaiting_user", "request_id": requestID, "id": id, "title": title, "purpose": purpose, "next_action": "Keep this assistant turn alive: call wait, then credential_value action=poll with request_id; repeat while awaiting_user."}, nil
	case "poll":
		requestID := strings.TrimSpace(stringValue(args["request_id"]))
		if !strings.HasPrefix(requestID, "secret-") || len(requestID) > 128 {
			return nil, fmt.Errorf("valid request_id is required")
		}
		data, err := os.ReadFile(secretResponsePath(requestID))
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"status": "awaiting_user", "request_id": requestID}, nil
		}
		if err != nil {
			return nil, err
		}
		var response struct {
			Approved bool   `json:"approved"`
			ID       string `json:"id"`
			Secret   string `json:"secret"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, fmt.Errorf("invalid credential response: %w", err)
		}
		_ = os.Remove(secretResponsePath(requestID))
		_ = os.Remove(secretRequestPath(requestID))
		if !response.Approved {
			return map[string]any{"status": "denied", "request_id": requestID, "id": response.ID, "reason": response.Reason}, nil
		}
		if response.Secret == "" {
			return nil, fmt.Errorf("approved response contained no value")
		}
		return map[string]any{"status": "completed", "request_id": requestID, "id": response.ID, "value": response.Secret}, nil
	default:
		return nil, fmt.Errorf("credential_value action must be one of: request, poll")
	}
}

func approvalDecisionPath(id string) string {
	return filepath.Join(connectorStateDir(), "approvals", id+".json")
}

func (s *Server) requestCommandApproval(args map[string]any, cmd []string, callID string) (map[string]any, error) {
	pid := nextCommandProcessID()
	approvalID := fmt.Sprintf("approval-%d", time.Now().UnixNano())
	reason := stringValue(args["approval_reason"])
	if reason == "" {
		if titles := secretRefTitles(args); len(titles) > 0 {
			reason = "Use saved credential: " + strings.Join(titles, ", ")
		} else {
			reason = "Command contains or may transmit sensitive credentials"
		}
	}
	session := &commandSession{processID: pid, callID: callID, chatSessionID: stringValue(args["session_id"]), started: time.Now(), lastOutput: time.Now(), done: make(chan struct{}), approvalState: "pending", approvalID: approvalID, approvalReason: reason}
	s.commandsMu.Lock()
	s.commands[pid] = session
	s.commandsMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(approvalDecisionPath(approvalID)), 0o700)
	_ = os.Remove(approvalDecisionPath(approvalID))
	if s.logger != nil {
		s.logger.Event("WARN", "chatgpt_tool_call_approval_required", map[string]any{"tool": "command_exec", "call_id": callID, "process_id": pid, "approval_id": approvalID, "approval_reason": reason, "input_preview": logJSON(redactSensitive(args), 20000)})
	}
	go func() {
		deadline := time.Now().Add(30 * time.Minute)
		for time.Now().Before(deadline) {
			data, err := os.ReadFile(approvalDecisionPath(approvalID))
			if err == nil {
				_ = os.Remove(approvalDecisionPath(approvalID))
				var decision struct {
					Approve bool   `json:"approve"`
					Reason  string `json:"reason"`
				}
				if json.Unmarshal(data, &decision) == nil {
					if !decision.Approve {
						session.mu.Lock()
						session.approvalState = "denied"
						session.result = map[string]any{"status": "denied", "process_id": pid, "approval_id": approvalID, "reason": decision.Reason}
						session.mu.Unlock()
						close(session.done)
						if s.logger != nil {
							s.logger.Event("WARN", "chatgpt_tool_call_approval_resolved", map[string]any{"tool": "command_exec", "call_id": callID, "process_id": pid, "approval_id": approvalID, "status": "denied"})
						}
						return
					}
					approvedArgs := make(map[string]any, len(args)+1)
					for k, v := range args {
						approvedArgs[k] = v
					}
					approvedArgs["require_approval"] = false
					approvedArgs["_approval_granted"] = true
					result, runErr := s.command(context.Background(), approvedArgs, callID)
					adoptedChild := false
					if result != nil {
						m := result
						childID := stringValue(m["process_id"])
						if childID != "" && childID != pid {
							s.commandsMu.Lock()
							if child := s.commands[childID]; child != nil {
								child.mu.Lock()
								child.processID = pid
								child.approvalState = "approved"
								child.approvalID = approvalID
								child.approvalReason = reason
								child.mu.Unlock()
								s.commands[pid] = child
								delete(s.commands, childID)
								adoptedChild = true
							}
							s.commandsMu.Unlock()
							m["process_id"] = pid
						}
					}
					if !adoptedChild {
						session.mu.Lock()
						session.approvalState = "approved"
						session.result = result
						session.err = runErr
						session.mu.Unlock()
						close(session.done)
					}
					if s.logger != nil {
						s.logger.Event("INFO", "chatgpt_tool_call_approval_resolved", map[string]any{"tool": "command_exec", "call_id": callID, "process_id": pid, "approval_id": approvalID, "status": "approved", "output_preview": logJSON(redactSensitive(result), 20000)})
					}
					return
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		session.mu.Lock()
		session.approvalState = "expired"
		session.result = map[string]any{"status": "expired", "process_id": pid, "approval_id": approvalID}
		session.mu.Unlock()
		close(session.done)
	}()
	return map[string]any{"status": "awaiting_approval", "process_id": pid, "approval_id": approvalID, "approval_reason": reason, "command_preview": redactString(strings.Join(cmd, " ")), "next_action": "Keep this assistant turn alive: call wait, then command_poll(process_id); repeat while awaiting_approval. Do not finalize the response before approval resolves."}, nil
}

var shellWriteTargetRE = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:1\s*)?>>?\s*["']?([^\s;&|"']+)["']?`)
var psWriteTargetRE = regexp.MustCompile(`(?i)(?:set-content|add-content|out-file).*?(?:-literalpath|-path)\s+["']([^"']+)["']`)
var pyWriteTargetRE = regexp.MustCompile(`(?i)(?:open\(|write_text\()["']([^"']+)["']`)
var jsWriteTargetRE = regexp.MustCompile(`(?i)(?:writefile(?:sync)?|appendfile(?:sync)?|bun\.write)\s*\(\s*["']([^"']+)["']`)
var teeWriteTargetRE = regexp.MustCompile(`(?i)(?:^|[\s|;&])tee(?:\.exe)?\s+["']?([^\s;&|"']+)`)

type fileSnapshot struct {
	path    string
	existed bool
	data    []byte
}

func isTextPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".txt", ".md", ".json", ".jsonl", ".js", ".jsx", ".ts", ".tsx", ".html", ".htm", ".css", ".scss", ".xml", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".env", ".py", ".pyw", ".go", ".rs", ".java", ".cs", ".c", ".cc", ".cpp", ".h", ".hpp", ".ps1", ".cmd", ".bat", ".sh", ".sql", ".csv":
		return true
	}
	return false
}

func detectTextWriteTargets(cmd []string, cwd string) []string {
	joined := strings.Join(cmd, " ")
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(strings.Trim(raw, `"'`))
		low := strings.ToLower(raw)
		if raw == "" || low == "nul" || low == "$null" || low == "/dev/null" {
			return
		}
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(cwd, raw)
		}
		raw = filepath.Clean(raw)
		if !isTextPath(raw) || seen[strings.ToLower(raw)] {
			return
		}
		seen[strings.ToLower(raw)] = true
		out = append(out, raw)
	}
	for _, re := range []*regexp.Regexp{shellWriteTargetRE, psWriteTargetRE, pyWriteTargetRE, jsWriteTargetRE, teeWriteTargetRE} {
		for _, m := range re.FindAllStringSubmatch(joined, -1) {
			if len(m) > 1 {
				add(m[1])
			}
		}
	}
	return out
}

func snapshotTargets(paths []string) []fileSnapshot {
	out := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		out = append(out, fileSnapshot{path: path, existed: err == nil, data: data})
	}
	return out
}

func looksMojibake(text string) bool {
	markers := []string{"Р°", "Рµ", "Рё", "Рѕ", "РЅ", "Рї", "СЂ", "С‚", "СЃ", "Р»", "Рє", "Рґ", "РІ", "Рј", "С‹", "СЏ", "Р¶", "С‡", "С€"}
	count := 0
	for _, marker := range markers {
		count += strings.Count(text, marker)
	}
	return count >= 4
}

func validateAndRestoreTextTargets(snaps []fileSnapshot) error {
	for _, snap := range snaps {
		data, err := os.ReadFile(snap.path)
		if err != nil {
			continue
		}
		if utf8.Valid(data) && !looksMojibake(string(data)) {
			continue
		}
		if snap.existed {
			_ = os.WriteFile(snap.path, snap.data, 0o644)
		} else {
			_ = os.Remove(snap.path)
		}
		return fmt.Errorf("encoding safety check failed for %s: terminal write produced non-UTF-8 or mojibake text; original file was restored", snap.path)
	}
	return nil
}

func shellExecCommand(args map[string]any) ([]any, error) {
	script := strings.TrimSpace(stringValue(args["script"]))
	if script == "" {
		return nil, fmt.Errorf("script must be non-empty string")
	}
	shell := strings.ToLower(strings.TrimSpace(stringValue(args["shell"])))
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "powershell"
		} else {
			shell = "sh"
		}
	}
	var argv []any
	switch shell {
	case "powershell":
		argv = []any{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}
	case "pwsh":
		argv = []any{"pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script}
	case "cmd":
		argv = []any{"cmd.exe", "/d", "/s", "/c", script}
	case "bash":
		argv = []any{"bash", "-lc", script}
	case "sh":
		argv = []any{"sh", "-lc", script}
	default:
		return nil, fmt.Errorf("unsupported shell %q", shell)
	}
	return argv, nil
}

func (s *Server) shellExec(ctx context.Context, args map[string]any, callID string) (map[string]any, error) {
	argv, err := shellExecCommand(args)
	if err != nil {
		return nil, err
	}
	forwarded := make(map[string]any, len(args)+1)
	for k, v := range args {
		if k != "script" && k != "shell" {
			forwarded[k] = v
		}
	}
	forwarded["command"] = argv
	return s.command(ctx, forwarded, callID)
}

func (s *Server) command(ctx context.Context, args map[string]any, callID string) (map[string]any, error) {
	arr, ok := args["command"].([]any)
	if !ok || len(arr) == 0 {
		return nil, fmt.Errorf("command must be non-empty array")
	}
	cmd := make([]string, 0, len(arr))
	for _, v := range arr {
		x, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("command entries must be strings")
		}
		cmd = append(cmd, x)
	}
	if commandNeedsApproval(args) {
		return s.requestCommandApproval(args, cmd, callID)
	}
	if boolValue(args["_approval_granted"], false) {
		if err := s.injectSecretRefs(args); err != nil {
			return nil, err
		}
	}
	if runtime.GOOS == "windows" {
		cmd = normalizeWindowsCommand(cmd)
		return s.commandWindowsBuffered(ctx, args, cmd, callID)
	}
	pid := nextCommandProcessID()
	session := &commandSession{processID: pid, callID: callID, chatSessionID: stringValue(args["session_id"]), started: time.Now(), done: make(chan struct{})}
	session.unregister = s.streams.Register(pid, func(stream string, data []byte, capReached bool) {
		session.mu.Lock()
		if stream == "stderr" {
			session.stderr.Write(data)
		} else {
			session.stdout.Write(data)
		}
		session.capReached = session.capReached || capReached
		session.mu.Unlock()
		if s.logger != nil && callID != "" {
			logData := map[string]any{"tool": "command_exec", "call_id": callID, "process_id": pid, "stream": stream, "delta": redactString(string(data)), "cap_reached": capReached}
			if sid := stringValue(args["session_id"]); sid != "" {
				logData["session_id"] = sid
				if item, ok := s.sessionByID(sid); ok {
					logData["session_name"] = item.Name
				}
			}
			s.logger.Event("INFO", "chatgpt_tool_call_stream", logData)
		}
	})
	s.commandsMu.Lock()
	s.commands[pid] = session
	s.commandsMu.Unlock()

	p := map[string]any{"command": cmd, "processId": pid, "streamStdoutStderr": true, "permissionProfile": ":danger-full-access"}
	if v, ok := args["cwd"]; ok {
		q, e := s.resolvePath(v)
		if e != nil {
			s.deleteCommand(pid)
			return nil, e
		}
		p["cwd"] = q
	}
	// Never delegate timeout killing to app-server. timeout_ms is a connector-side
	// soft notification threshold only; explicit command_terminate is the sole
	// timeout-related path that is allowed to stop a healthy process.
	p["disableTimeout"] = true
	if v, ok := numberAsInt(args["timeout_ms"]); ok {
		if boolValue(args["disable_timeout"], false) {
			s.deleteCommand(pid)
			return nil, fmt.Errorf("timeout_ms cannot be combined with disable_timeout")
		}
		session.timeoutMs = v
		armSoftCommandTimeout(session, time.Duration(v)*time.Millisecond)
	}
	if v, ok := numberAsInt(args["output_bytes_cap"]); ok {
		if boolValue(args["disable_output_cap"], false) {
			s.deleteCommand(pid)
			return nil, fmt.Errorf("output_bytes_cap cannot be combined with disable_output_cap")
		}
		p["outputBytesCap"] = v
	} else if boolValue(args["disable_output_cap"], false) {
		p["disableOutputCap"] = true
	}
	tty := boolValue(args["tty"], false)
	streamStdin := boolValue(args["stream_stdin"], false) || tty
	if tty {
		p["tty"] = true
		rows := int64(24)
		cols := int64(100)
		if n, ok := numberAsInt(args["rows"]); ok {
			rows = n
		}
		if n, ok := numberAsInt(args["cols"]); ok {
			cols = n
		}
		p["size"] = map[string]any{"rows": rows, "cols": cols}
	}
	if streamStdin {
		p["streamStdin"] = true
	}
	if env := commandEnvironment(args); len(env) > 0 {
		p["env"] = env
	}

	go func() {
		var final map[string]any
		err := s.app.Request(context.Background(), "command/exec", p, &final)
		session.mu.Lock()
		session.result = final
		session.err = err
		if session.unregister != nil {
			session.unregister()
			session.unregister = nil
		}
		session.mu.Unlock()
		close(session.done)
		session.mu.Lock()
		yielded := session.yielded
		session.mu.Unlock()
		if yielded && s.logger != nil && callID != "" {
			payload := sessionSnapshot(session, false)
			data := map[string]any{"tool": "command_exec", "call_id": callID, "process_id": pid, "duration_ms": float64(time.Since(session.started).Microseconds()) / 1000, "output_preview": logJSON(redactSensitive(payload), 20000)}
			if err != nil {
				data["error_preview"] = err.Error()
				s.logger.Event("ERROR", "chatgpt_tool_call_failed", data)
			} else {
				copyResultFields(data, payload)
				s.logger.Event("INFO", "chatgpt_tool_call_succeeded", data)
			}
		}
	}()

	yield := int64(10000)
	if n, ok := numberAsInt(args["yield_time_ms"]); ok {
		yield = n
	}
	if boolValue(args["background"], false) && yield > 100 {
		yield = 100
	}
	if yield < 0 {
		yield = 0
	}
	markYielded := func() {
		session.mu.Lock()
		session.yielded = true
		session.mu.Unlock()
	}
	if yield == 0 {
		select {
		case <-session.done:
			return sessionSnapshot(session, false), sessionError(session)
		default:
			markYielded()
			return sessionSnapshot(session, false), nil
		}
	}
	select {
	case <-session.done:
		return sessionSnapshot(session, false), sessionError(session)
	case <-time.After(time.Duration(yield) * time.Millisecond):
		markYielded()
		return sessionSnapshot(session, false), nil
	case <-ctx.Done():
		// The app-server command request intentionally outlives this handler so
		// yielded/background commands can be polled later. A cancelled MCP call,
		// however, never hands the process id back to the caller reliably. Kill
		// that command explicitly instead of leaking it in the app-server.
		terminateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.terminateCommand(terminateCtx, pid, session, 3*time.Second)
		cancel()
		return sessionSnapshot(session, false), ctx.Err()
	}
}

func normalizeWindowsCommand(cmd []string) []string {
	if len(cmd) == 0 {
		return cmd
	}

	resolved := resolveWindowsCommand(cmd[0])
	if resolved == "" {
		return cmd
	}
	resolved, _ = filepath.Abs(resolved)

	ext := strings.ToLower(filepath.Ext(resolved))
	if ext != ".cmd" && ext != ".bat" {
		out := append([]string(nil), cmd...)
		out[0] = resolved
		return normalizePowerShellCommand(out)
	}

	// Do not rebuild argv into a single shell string here. Passing a joined and
	// manually quoted command through `cmd /c` causes a second parsing pass and
	// is the source of a large class of Windows escaping bugs (npm/pnpm/npx,
	// paths with spaces, &, %, quotes, etc.). Keep each original argument as an
	// argv element and let Go's Windows process launcher perform the native
	// command-line quoting exactly once.
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	out := make([]string, 0, len(cmd)+3)
	out = append(out, comspec, "/d", "/c", resolved)
	out = append(out, cmd[1:]...)
	return out
}

func normalizePowerShellCommand(cmd []string) []string {
	if len(cmd) < 3 {
		return cmd
	}
	base := strings.ToLower(filepath.Base(cmd[0]))
	if base != "powershell" && base != "powershell.exe" && base != "pwsh" && base != "pwsh.exe" {
		return cmd
	}
	for i := 1; i+1 < len(cmd); i++ {
		flag := strings.ToLower(cmd[i])
		if flag != "-command" && flag != "-c" {
			continue
		}
		script := cmd[i+1]
		wrapped := `$global:LASTEXITCODE = 0; $ErrorActionPreference = 'Stop'; try { & { ` + script + ` }; $__codexOk = $?; $__codexExit = $global:LASTEXITCODE; if ($__codexExit -ne 0) { exit $__codexExit }; if (-not $__codexOk) { exit 1 } } catch { [Console]::Error.WriteLine($_.ToString()); exit 1 }`
		out := append([]string(nil), cmd...)
		out[i+1] = wrapped
		return out
	}
	return cmd
}

func resolveWindowsCommand(name string) string {
	if name == "" {
		return ""
	}

	// Windows command resolution is PATHEXT-based. exec.LookPath("npm") may
	// choose an extensionless POSIX shim before npm.cmd when both live in the
	// same directory. That file is valid for bash but cannot be executed by
	// CreateProcess, so explicitly prefer native Windows executable extensions.
	if filepath.Ext(name) == "" {
		pathext := os.Getenv("PATHEXT")
		if strings.TrimSpace(pathext) == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, ext := range strings.Split(pathext, ";") {
			ext = strings.TrimSpace(ext)
			if ext == "" {
				continue
			}
			candidate := name + ext
			if resolved, err := exec.LookPath(candidate); err == nil && resolved != "" {
				return resolved
			}
		}
	}

	if resolved, err := exec.LookPath(name); err == nil && resolved != "" {
		return resolved
	}
	return ""
}

func localSearchShouldUseFS(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	joined := " " + strings.ToLower(strings.Join(argv, " ")) + " "
	// Remote inspection is intentionally allowed: grep/find on a remote host is
	// not a search over the local CodexPC filesystem.
	first := strings.ToLower(strings.TrimSpace(argv[0]))
	if first == "ssh" || strings.HasSuffix(first, "\\ssh.exe") || strings.HasSuffix(first, "/ssh") {
		return false
	}
	markers := []string{
		" get-childitem ", " gci ", " select-string ",
		" rg ", " ripgrep ", " grep -r", " grep --recursive", " findstr /s", " dir /s",
		"os.walk(", ".rglob(", "rglob(", "glob('**", "glob(\"**",
	}
	for _, marker := range markers {
		if strings.Contains(joined, marker) {
			return true
		}
	}
	return false
}

func armSoftCommandTimeout(session *commandSession, limit time.Duration) {
	if session == nil || limit <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case <-session.done:
			return
		case <-timer.C:
			session.mu.Lock()
			session.timedOut = true
			session.mu.Unlock()
		}
	}()
}

func killWindowsProcessTree(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	killer := exec.Command("taskkill.exe", "/PID", fmt.Sprintf("%d", command.Process.Pid), "/T", "/F")
	if err := killer.Run(); err != nil {
		_ = command.Process.Kill()
	}
}

func commandEnvironment(args map[string]any) map[string]any {
	env := map[string]any{
		"PYTHONUTF8":       "1",
		"PYTHONIOENCODING": "utf-8",
		"LANG":             "C.UTF-8",
		"LC_ALL":           "C.UTF-8",
	}
	if supplied, ok := args["env"].(map[string]any); ok {
		for key, value := range supplied {
			env[key] = value
		}
	}
	return env
}

func (s *Server) commandWindowsBuffered(ctx context.Context, args map[string]any, argv []string, callID string) (map[string]any, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("command must be non-empty array")
	}
	if boolValue(args["tty"], false) {
		return nil, fmt.Errorf("tty is not supported by the native Windows command runner")
	}
	if _, ok := numberAsInt(args["timeout_ms"]); ok && boolValue(args["disable_timeout"], false) {
		return nil, fmt.Errorf("timeout_ms cannot be combined with disable_timeout")
	}

	pid := nextCommandProcessID()
	now := time.Now()
	session := &commandSession{processID: pid, callID: callID, chatSessionID: stringValue(args["session_id"]), started: now, done: make(chan struct{}), local: true}
	if !boolValue(args["disable_output_cap"], false) {
		// Keep long/noisy processes bounded by default. This only limits retained
		// output for snapshots/UI/log streaming; it never stops the child process.
		session.outputCap = 8 * 1024 * 1024
		if capBytes, ok := numberAsInt(args["output_bytes_cap"]); ok {
			session.outputCap = capBytes
		}
	}
	command := exec.Command(argv[0], argv[1:]...)
	session.cmd = command
	if v, ok := args["cwd"]; ok {
		q, e := s.resolvePath(v)
		if e != nil {
			return nil, e
		}
		command.Dir = q
	}
	effectiveCwd := command.Dir
	if effectiveCwd == "" {
		effectiveCwd, _ = os.Getwd()
	}
	writeSnapshots := snapshotTargets(detectTextWriteTargets(argv, effectiveCwd))
	env := os.Environ()
	for key, value := range commandEnvironment(args) {
		env = append(env, fmt.Sprintf("%s=%v", key, value))
	}
	command.Env = env
	streamLog := func(stream string, data []byte) {
		if s.logger != nil && callID != "" {
			logData := map[string]any{
				"tool": "command_exec", "call_id": callID, "process_id": pid,
				"stream": stream, "delta": redactString(string(data)), "cap_reached": false,
			}
			if sid := stringValue(args["session_id"]); sid != "" {
				logData["session_id"] = sid
				if item, ok := s.sessionByID(sid); ok {
					logData["session_name"] = item.Name
				}
			}
			s.logger.Event("INFO", "chatgpt_tool_call_stream", logData)
		}
	}
	command.Stdout = commandSessionWriter{session: session, onWrite: streamLog}
	command.Stderr = commandSessionWriter{session: session, stderr: true, onWrite: streamLog}
	if boolValue(args["stream_stdin"], false) {
		stdin, err := command.StdinPipe()
		if err != nil {
			return nil, err
		}
		session.stdin = stdin
	}

	if err := command.Start(); err != nil {
		if session.stdin != nil {
			_ = session.stdin.Close()
		}
		return nil, err
	}

	s.commandsMu.Lock()
	s.commands[pid] = session
	s.commandsMu.Unlock()

	if timeout, ok := numberAsInt(args["timeout_ms"]); ok && timeout > 0 {
		session.timeoutMs = timeout
		armSoftCommandTimeout(session, time.Duration(timeout)*time.Millisecond)
	}

	go func() {
		err := command.Wait()
		encodingErr := validateAndRestoreTextTargets(writeSnapshots)
		session.mu.Lock()
		exitCode := 0
		if command.ProcessState != nil {
			exitCode = command.ProcessState.ExitCode()
		}
		session.result = map[string]any{"exitCode": exitCode}
		if encodingErr != nil {
			session.err = encodingErr
		} else if err != nil {
			if exitCode != 0 {
				session.err = fmt.Errorf("process exited with code %d", exitCode)
			} else {
				session.err = err
			}
		}
		if session.stdin != nil {
			_ = session.stdin.Close()
			session.stdin = nil
		}
		session.mu.Unlock()
		close(session.done)
	}()

	yield := int64(10000)
	if n, ok := numberAsInt(args["yield_time_ms"]); ok {
		yield = n
	}
	if boolValue(args["background"], false) && yield > 100 {
		yield = 100
	}
	if yield < 0 {
		yield = 0
	}
	if yield == 0 {
		return sessionSnapshot(session, false), nil
	}
	select {
	case <-session.done:
		// A child process exiting non-zero is an execution result, not an MCP
		// transport failure. Keep stdout/stderr/exitCode visible to the model.
		return sessionSnapshot(session, false), nil
	case <-time.After(time.Duration(yield) * time.Millisecond):
		session.mu.Lock()
		session.yielded = true
		session.mu.Unlock()
		return sessionSnapshot(session, false), nil
	case <-ctx.Done():
		// The process deliberately survives MCP request cancellation. The caller
		// can reconnect and continue through command_poll/command_terminate.
		session.mu.Lock()
		session.yielded = true
		session.mu.Unlock()
		return sessionSnapshot(session, false), nil
	}
}

func (s *Server) commandSession(processID string) (*commandSession, error) {
	s.commandsMu.Lock()
	defer s.commandsMu.Unlock()
	session := s.commands[processID]
	if session == nil {
		return nil, fmt.Errorf("unknown process_id: %s", processID)
	}
	return session, nil
}

func (s *Server) deleteCommand(processID string) {
	s.commandsMu.Lock()
	session := s.commands[processID]
	delete(s.commands, processID)
	s.commandsMu.Unlock()
	if session != nil {
		session.mu.Lock()
		if session.unregister != nil {
			session.unregister()
			session.unregister = nil
		}
		session.mu.Unlock()
	}
}

func sessionError(session *commandSession) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.err
}

func sessionSnapshot(session *commandSession, deltaOnly bool) map[string]any {
	session.mu.Lock()
	defer session.mu.Unlock()
	stdout := session.stdout.String()
	stderr := session.stderr.String()
	if deltaOnly {
		if session.stdoutRead < len(stdout) {
			stdout = stdout[session.stdoutRead:]
		} else {
			stdout = ""
		}
		if session.stderrRead < len(stderr) {
			stderr = stderr[session.stderrRead:]
		} else {
			stderr = ""
		}
		session.stdoutRead = session.stdout.Len()
		session.stderrRead = session.stderr.Len()
	}
	status := "running"
	if session.approvalState == "pending" {
		status = "awaiting_approval"
	} else {
		select {
		case <-session.done:
			status = "completed"
		default:
		}
	}
	lastOutputAgo := time.Since(session.lastOutput).Seconds()
	if session.lastOutput.IsZero() {
		lastOutputAgo = time.Since(session.started).Seconds()
	}
	out := map[string]any{"process_id": session.processID, "status": status, "stdout": stdout, "stderr": stderr, "elapsed_sec": time.Since(session.started).Seconds(), "last_output_sec_ago": lastOutputAgo, "output_bytes": session.outputBytes, "stdin_open": session.stdin != nil, "cap_reached": session.capReached}
	if session.timeoutMs > 0 {
		out["timeout_ms"] = session.timeoutMs
	}
	if session.timedOut {
		out["timeout_reached"] = true
		out["timeout_notice"] = "Soft timeout reached; process was not terminated and may still be running. Choose whether to keep polling, terminate explicitly, or use a different timeout_ms for future commands."
	}
	if session.approvalID != "" {
		out["approval_id"] = session.approvalID
		out["approval_reason"] = session.approvalReason
	}
	if session.local && session.cmd != nil && session.cmd.Process != nil {
		out["pid"] = session.cmd.Process.Pid
	}
	if session.result != nil {
		for k, v := range session.result {
			switch k {
			case "stdout":
				if stdout == "" {
					out[k] = v
				}
			case "stderr":
				if stderr == "" {
					out[k] = v
				}
			default:
				out[k] = v
			}
		}
	}
	if session.err != nil {
		out["error"] = session.err.Error()
		out["status"] = "failed"
	}
	return out
}

func ensureCommandSessionOwner(session *commandSession, args map[string]any) error {
	if session == nil || session.chatSessionID == "" {
		return nil
	}
	if session.chatSessionID != stringValue(args["session_id"]) {
		return fmt.Errorf("SESSION_PROCESS_MISMATCH: process belongs to another CodexPC chat session")
	}
	return nil
}

func (s *Server) commandPoll(ctx context.Context, args map[string]any) (map[string]any, error) {
	pid := stringValue(args["process_id"])
	session, err := s.commandSession(pid)
	if err != nil {
		return nil, err
	}
	if err := ensureCommandSessionOwner(session, args); err != nil {
		return nil, err
	}
	yield := int64(250)
	if n, ok := numberAsInt(args["yield_time_ms"]); ok {
		yield = n
	}
	if yield > 0 {
		select {
		case <-session.done:
		case <-time.After(time.Duration(yield) * time.Millisecond):
		case <-ctx.Done():
		}
	}
	return sessionSnapshot(session, true), nil
}

func (s *Server) commandWrite(ctx context.Context, args map[string]any) (map[string]any, error) {
	pid := stringValue(args["process_id"])
	session, err := s.commandSession(pid)
	if err != nil {
		return nil, err
	}
	if err := ensureCommandSessionOwner(session, args); err != nil {
		return nil, err
	}
	input, hasInput := args["input"].(string)
	if boolValue(args["append_newline"], false) {
		input += "\n"
		hasInput = true
	}
	closeStdin := boolValue(args["close_stdin"], false)
	if !hasInput && !closeStdin {
		return nil, fmt.Errorf("command_write requires input, append_newline, or close_stdin; use command_poll for read-only polling")
	}
	if session.local {
		session.mu.Lock()
		stdin := session.stdin
		session.mu.Unlock()
		if stdin == nil {
			return nil, fmt.Errorf("stdin is closed for process %s", pid)
		}
		if hasInput {
			if _, err := io.WriteString(stdin, input); err != nil {
				return nil, err
			}
		}
		if closeStdin {
			if err := stdin.Close(); err != nil {
				return nil, err
			}
			session.mu.Lock()
			if session.stdin == stdin {
				session.stdin = nil
			}
			session.mu.Unlock()
		}
		return sessionSnapshot(session, true), nil
	}
	p := map[string]any{"processId": pid, "closeStdin": closeStdin}
	if hasInput {
		p["deltaBase64"] = base64.StdEncoding.EncodeToString([]byte(input))
	}
	var response any
	if err := s.app.Request(ctx, "command/exec/write", p, &response); err != nil {
		return nil, err
	}
	return sessionSnapshot(session, true), nil
}

func (s *Server) commandResize(ctx context.Context, args map[string]any) (map[string]any, error) {
	pid := stringValue(args["process_id"])
	session, err := s.commandSession(pid)
	if err != nil {
		return nil, err
	}
	if err := ensureCommandSessionOwner(session, args); err != nil {
		return nil, err
	}
	rows, rok := numberAsInt(args["rows"])
	cols, cok := numberAsInt(args["cols"])
	if !rok || !cok || rows < 1 || cols < 1 {
		return nil, fmt.Errorf("rows and cols must be positive")
	}
	var response any
	if err := s.app.Request(ctx, "command/exec/resize", map[string]any{"processId": pid, "size": map[string]any{"rows": rows, "cols": cols}}, &response); err != nil {
		return nil, err
	}
	return map[string]any{"process_id": pid, "status": "resized", "rows": rows, "cols": cols}, nil
}

func (s *Server) terminateCommand(ctx context.Context, pid string, session *commandSession, wait time.Duration) error {
	if session.local {
		session.mu.Lock()
		cmd := session.cmd
		session.mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			// Kill the whole Windows process tree so child processes do not survive
			// after explicit termination or a timeout.
			killWindowsProcessTree(cmd)
		}
	} else {
		var response any
		if err := s.app.Request(ctx, "command/exec/terminate", map[string]any{"processId": pid}, &response); err != nil {
			return err
		}
	}
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-session.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("process %s did not exit within %s after termination request", pid, wait)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) commandTerminate(ctx context.Context, args map[string]any) (map[string]any, error) {
	pid := stringValue(args["process_id"])
	session, err := s.commandSession(pid)
	if err != nil {
		return nil, err
	}
	if err := ensureCommandSessionOwner(session, args); err != nil {
		return nil, err
	}
	if err := s.terminateCommand(ctx, pid, session, 5*time.Second); err != nil {
		return sessionSnapshot(session, true), err
	}
	return sessionSnapshot(session, true), nil
}

func (s *Server) ensureThread(ctx context.Context) (string, error) {
	s.threadMu.Lock()
	defer s.threadMu.Unlock()
	if s.threadID != "" {
		return s.threadID, nil
	}
	var r map[string]any
	if err := s.app.Request(ctx, "thread/start", map[string]any{"cwd": s.workspace, "ephemeral": true, "sandbox": "danger-full-access", "approvalPolicy": "never"}, &r); err != nil {
		return "", err
	}
	t, _ := r["thread"].(map[string]any)
	id, _ := t["id"].(string)
	if id == "" {
		return "", fmt.Errorf("no thread id")
	}
	s.threadID = id
	return id, nil
}

func (s *Server) multiTool(ctx context.Context, args map[string]any, callID string) (any, error) {
	ops, ok := args["operations"].([]any)
	if !ok || len(ops) == 0 {
		return nil, fmt.Errorf("operations must be a non-empty array")
	}
	if len(ops) > 16 {
		return nil, fmt.Errorf("too many operations: max 16")
	}
	type batchOp struct {
		Tool string
		Args map[string]any
	}
	parsed := make([]batchOp, len(ops))
	mutating := map[string]bool{"fs_write_file": true, "fs_edit_file": true, "fs_remove": true, "fs_copy": true, "fs_create_directory": true}
	seenTargets := map[string]int{}
	sessionID := stringValue(args["session_id"])
	sessionName := ""
	if item, ok := s.sessionByID(sessionID); ok {
		sessionName = item.Name
	}
	for i, raw := range ops {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("operation %d must be an object", i)
		}
		name := stringValue(m["tool"])
		if name == "" || name == "multi_tool" {
			return nil, fmt.Errorf("operation %d has invalid tool %q", i, name)
		}
		opArgs := mapValue(m["arguments"])
		if sessionID != "" {
			opArgs["session_id"] = sessionID
		}
		parsed[i] = batchOp{Tool: name, Args: opArgs}
		if mutating[name] {
			target := stringValue(opArgs["path"])
			if name == "fs_copy" {
				target = stringValue(opArgs["destination_path"])
			}
			if target != "" {
				resolved, err := s.resolvePath(target)
				if err != nil {
					return nil, fmt.Errorf("operation %d: %w", i, err)
				}
				key := strings.ToLower(filepath.Clean(resolved))
				if prev, exists := seenTargets[key]; exists {
					return nil, fmt.Errorf("operations %d and %d both mutate the same target: %s", prev, i, resolved)
				}
				seenTargets[key] = i
			}
		}
	}

	states := make([]map[string]any, len(parsed))
	for i, op := range parsed {
		states[i] = map[string]any{"index": i, "tool": op.Tool, "status": "queued", "input_preview": logJSON(redactSensitive(op.Args), 4000)}
		copyTargetFields(states[i], op.Args)
	}
	var stateMu sync.Mutex
	emit := func() {
		if s.logger == nil {
			return
		}
		stateMu.Lock()
		snapshot := make([]any, len(states))
		for i, item := range states {
			cp := make(map[string]any, len(item))
			for k, v := range item {
				cp[k] = v
			}
			snapshot[i] = cp
		}
		stateMu.Unlock()
		data := map[string]any{"tool": "multi_tool", "call_id": callID, "batch_items": snapshot}
		if sessionID != "" {
			data["session_id"] = sessionID
			data["session_name"] = sessionName
		}
		s.logger.Event("INFO", "chatgpt_tool_call_batch_progress", data)
	}
	emit()

	results := make([]any, len(parsed))
	errs := make([]error, len(parsed))
	runOne := func(i int) {
		op := parsed[i]
		started := time.Now()
		stateMu.Lock()
		states[i]["status"] = "running"
		stateMu.Unlock()
		emit()
		res, err := s.callTool(ctx, op.Tool, op.Args, "")
		results[i], errs[i] = res, err
		stateMu.Lock()
		states[i]["duration_ms"] = float64(time.Since(started).Microseconds()) / 1000
		if err != nil {
			states[i]["status"] = "failed"
			states[i]["error_preview"] = err.Error()
		} else {
			states[i]["status"] = "succeeded"
			states[i]["output_preview"] = logJSON(res, 4000)
		}
		stateMu.Unlock()
		emit()
	}
	if boolValue(args["parallel"], true) {
		var wg sync.WaitGroup
		wg.Add(len(parsed))
		for i := range parsed {
			go func(i int) { defer wg.Done(); runOne(i) }(i)
		}
		wg.Wait()
	} else {
		for i := range parsed {
			runOne(i)
		}
	}

	items := make([]any, len(parsed))
	failed := 0
	for i, op := range parsed {
		item := map[string]any{"index": i, "tool": op.Tool, "ok": errs[i] == nil}
		if errs[i] != nil {
			item["error"] = errs[i].Error()
			failed++
		} else {
			item["result"] = results[i]
		}
		items[i] = item
	}
	return toolResult(map[string]any{"count": len(items), "failed": failed, "succeeded": len(items) - failed, "items": items}), nil
}

func (s *Server) fsSearch(ctx context.Context, args map[string]any) (map[string]any, error) {
	root, err := s.resolvePath(args["path"])
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fs_search path must be a directory")
	}
	query := stringValue(args["query"])
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	mode := strings.ToLower(stringValue(args["mode"]))
	if mode == "" {
		mode = "name"
	}
	if mode != "name" && mode != "content" && mode != "both" {
		return nil, fmt.Errorf("mode must be name, content, or both")
	}
	caseSensitive := boolValue(args["case_sensitive"], false)
	useRegex := boolValue(args["regex"], false)
	includeHidden := boolValue(args["include_hidden"], false)
	glob := stringValue(args["glob"])
	limit := int64(100)
	if n, ok := numberAsInt(args["max_results"]); ok {
		limit = n
	}
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("max_results must be between 1 and 500")
	}

	var re *regexp.Regexp
	if useRegex {
		pattern := query
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
	}
	needle := query
	if !caseSensitive && !useRegex {
		needle = strings.ToLower(needle)
	}
	matches := func(value string) bool {
		if useRegex {
			return re.MatchString(value)
		}
		if !caseSensitive {
			value = strings.ToLower(value)
		}
		return strings.Contains(value, needle)
	}
	globMatches := func(name string) bool {
		if glob == "" {
			return true
		}
		ok, matchErr := filepath.Match(glob, name)
		return matchErr == nil && ok
	}

	results := make([]any, 0, min(int(limit), 100))
	visitedFiles, visitedDirs, skippedLarge, skippedBinary := 0, 0, 0, 0
	truncated := false
	stopReason := ""
	searchStarted := time.Now()
	const searchBudget = 10 * time.Second
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, ".next": true, "__pycache__": true, ".venv": true, "venv": true,
		"env": true, ".tox": true, ".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
		"appdata": true, ".cache": true, "cache": true, "caches": true, "temp": true, "tmp": true,
		"vendor": true, "dist": true, "build": true, "target": true, ".gradle": true, ".idea": true,
	}
	textExt := map[string]bool{
		"": true, ".txt": true, ".md": true, ".go": true, ".py": true, ".js": true, ".jsx": true,
		".ts": true, ".tsx": true, ".json": true, ".jsonl": true, ".yaml": true, ".yml": true, ".toml": true,
		".ini": true, ".cfg": true, ".conf": true, ".xml": true, ".html": true, ".htm": true, ".css": true,
		".scss": true, ".less": true, ".sql": true, ".sh": true, ".bash": true, ".ps1": true, ".cmd": true,
		".bat": true, ".rs": true, ".java": true, ".kt": true, ".kts": true, ".c": true, ".h": true, ".cpp": true,
		".hpp": true, ".cs": true, ".php": true, ".rb": true, ".swift": true, ".vue": true, ".svelte": true,
		".env": true, ".gitignore": true, ".dockerignore": true, ".csv": true, ".tsv": true, ".log": true,
	}
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if time.Since(searchStarted) >= searchBudget {
			truncated = true
			stopReason = "time_budget"
			return io.EOF
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if path == root {
			visitedDirs++
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			visitedDirs++
			if skipDirs[strings.ToLower(name)] || (!includeHidden && strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if (mode == "name" || mode == "both") && globMatches(name) && matches(name) {
				results = append(results, map[string]any{"kind": "directory", "path": path, "name": name})
				if int64(len(results)) >= limit {
					truncated = true
					return io.EOF
				}
			}
			return nil
		}
		if !includeHidden && strings.HasPrefix(name, ".") {
			return nil
		}
		visitedFiles++
		if !globMatches(name) {
			return nil
		}
		if (mode == "name" || mode == "both") && matches(name) {
			results = append(results, map[string]any{"kind": "file", "path": path, "name": name})
			if int64(len(results)) >= limit {
				truncated = true
				return io.EOF
			}
		}
		if mode == "name" {
			return nil
		}
		// Content search is the expensive path. Avoid opening obvious binary/media/archive files;
		// callers can still target unusual text files explicitly with a glob.
		if glob == "" && !textExt[strings.ToLower(filepath.Ext(name))] {
			skippedBinary++
			return nil
		}
		stat, statErr := entry.Info()
		if statErr != nil || stat.Size() > 2*1024*1024 {
			if statErr == nil && stat.Size() > 2*1024*1024 {
				skippedLarge++
			}
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			skippedBinary++
			return nil
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			if !matches(line) {
				continue
			}
			snippet := strings.TrimSpace(line)
			if len(snippet) > 320 {
				snippet = snippet[:320] + "…"
			}
			results = append(results, map[string]any{"kind": "content", "path": path, "name": name, "line": i + 1, "text": snippet})
			if int64(len(results)) >= limit {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, io.EOF) {
		return nil, walkErr
	}
	return map[string]any{
		"path": root, "query": query, "mode": mode, "glob": glob,
		"case_sensitive": caseSensitive, "regex": useRegex,
		"count": len(results), "results": results, "truncated": truncated, "stop_reason": stopReason,
		"visited_files": visitedFiles, "visited_directories": visitedDirs,
		"skipped_large_files": skippedLarge, "skipped_binary_files": skippedBinary,
	}, nil
}

func (s *Server) readRules(args map[string]any) (map[string]any, error) {
	target := strings.TrimSpace(stringValue(args["path"]))
	if target == "" {
		target = s.workspace
	} else {
		resolved, err := s.resolvePath(target)
		if err != nil {
			return nil, err
		}
		target = resolved
	}
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		target = filepath.Dir(target)
	}
	target, _ = filepath.Abs(target)

	home, _ := os.UserHomeDir()
	candidates := make([]string, 0, 16)
	if home != "" {
		candidates = append(candidates, filepath.Join(home, "Desktop", "AGENTS.md"))
	}

	dirs := make([]string, 0, 12)
	for dir := target; dir != ""; {
		dirs = append(dirs, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		candidates = append(candidates, filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, ".agents", "AGENTS.md"))
	}

	seen := make(map[string]bool)
	rules := make([]map[string]any, 0, 8)
	combined := strings.Builder{}
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		key := strings.ToLower(filepath.Clean(abs))
		if seen[key] {
			continue
		}
		seen[key] = true
		data, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read rules %s: %w", abs, err)
		}
		content := string(data)
		rules = append(rules, map[string]any{"path": abs, "content": content, "size_bytes": len(data)})
		if combined.Len() > 0 {
			combined.WriteString("\n\n")
		}
		combined.WriteString("# Rules from ")
		combined.WriteString(abs)
		combined.WriteString("\n\n")
		combined.WriteString(content)
	}

	return map[string]any{
		"target":      target,
		"count":       len(rules),
		"rules":       rules,
		"combined":    combined.String(),
		"instruction": "Apply these rules to the current project work. More specific project rules supplement or override broader rules where they conflict.",
	}, nil
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

func (s *Server) editFile(args map[string]any) (map[string]any, error) {
	p, e := s.resolvePath(args["path"])
	if e != nil {
		return nil, e
	}
	data, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	oldHash := hex.EncodeToString(sum(data))
	expectedHash := strings.TrimSpace(stringValue(args["expected_sha256"]))
	baseStale := expectedHash != "" && !strings.EqualFold(oldHash, expectedHash)
	text := string(data)
	edits, ok := args["edits"].([]any)
	if !ok {
		return nil, fmt.Errorf("edits required")
	}
	count := 0
	var diff strings.Builder
	for _, v := range edits {
		m, _ := v.(map[string]any)
		old := stringValue(m["old_text"])
		nw := stringValue(m["new_text"])
		want := int64(1)
		if n, ok := numberAsInt(m["expected_count"]); ok {
			want = n
		}

		// fs_read_file pagination returns normalized LF text so line-based reads
		// are stable across platforms. Windows files commonly remain CRLF on disk,
		// though, which used to make a verbatim read -> edit round trip fail with
		// "expected 1 matches, got 0". If exact matching fails, retry with the
		// file's newline convention and apply the same convention to replacement
		// text. This changes only the requested fragment, not the whole file.
		matchOld := old
		matchNew := nw
		got := strings.Count(text, matchOld)
		if got == 0 && strings.Contains(old, "\n") {
			if strings.Contains(text, "\r\n") && !strings.Contains(old, "\r\n") {
				candidateOld := strings.ReplaceAll(old, "\n", "\r\n")
				candidateGot := strings.Count(text, candidateOld)
				if candidateGot > 0 {
					matchOld = candidateOld
					matchNew = strings.ReplaceAll(strings.ReplaceAll(nw, "\r\n", "\n"), "\n", "\r\n")
					got = candidateGot
				}
			} else if !strings.Contains(text, "\r\n") && strings.Contains(old, "\r\n") {
				candidateOld := strings.ReplaceAll(old, "\r\n", "\n")
				candidateGot := strings.Count(text, candidateOld)
				if candidateGot > 0 {
					matchOld = candidateOld
					matchNew = strings.ReplaceAll(nw, "\r\n", "\n")
					got = candidateGot
				}
			}
		}

		replaceAll := boolValue(m["replace_all"], false)
		// A stale base hash no longer kills a safe targeted edit up front. The
		// current file is authoritative: if the requested fragment still resolves
		// to exactly the expected number of matches, applying it is deterministic.
		// replace_all is intentionally stricter because concurrent changes could
		// silently broaden the mutation set.
		if baseStale && replaceAll {
			return nil, fmt.Errorf("STALE_FILE: hash mismatch; replace_all requires a fresh read")
		}
		// Keep edits deterministic, but tolerate the common case where the model
		// copied the right fragment with different indentation or horizontal
		// spacing. We only use this fallback for a single expected match; if the
		// relaxed pattern is ambiguous, the edit still fails instead of guessing.
		if got == 0 && want == 1 && !replaceAll {
			if actual, matches := whitespaceEquivalentMatch(text, old); matches == 1 {
				matchOld = actual
				got = 1
				if strings.Contains(actual, "\r\n") {
					matchNew = strings.ReplaceAll(strings.ReplaceAll(nw, "\r\n", "\n"), "\n", "\r\n")
				} else {
					matchNew = strings.ReplaceAll(nw, "\r\n", "\n")
				}
			}
		}
		if got != int(want) && !replaceAll {
			return nil, fmt.Errorf("expected %d matches, got %d (file may have changed since read, or the requested fragment is no longer unique)", want, got)
		}
		diffCount := int(want)
		if replaceAll {
			diffCount = got
		}
		appendEditDiff(&diff, text, matchOld, matchNew, diffCount)
		if replaceAll {
			text = strings.ReplaceAll(text, matchOld, matchNew)
			count += got
		} else {
			text = strings.Replace(text, matchOld, matchNew, int(want))
			count += int(want)
		}
	}
	newData := []byte(text)
	dry := boolValue(args["dry_run"], false)
	if !dry && !bytes.Equal(data, newData) {
		if e = os.WriteFile(p, newData, 0o666); e != nil {
			return nil, e
		}
	}
	return map[string]any{"path": p, "changed": !bytes.Equal(data, newData), "dry_run": dry, "replacements": count, "base_stale": baseStale, "expected_sha256": expectedHash, "old_sha256": oldHash, "new_sha256": hex.EncodeToString(sum(newData)), "encoding": "utf-8", "diff": strings.TrimSuffix(diff.String(), "\n")}, nil
}

func whitespaceEquivalentMatch(text, pattern string) (string, int) {
	if pattern == "" {
		return "", 0
	}
	var expr strings.Builder
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '\r':
			if i+1 < len(pattern) && pattern[i+1] == '\n' {
				expr.WriteString(`\r?\n`)
				i += 2
				continue
			}
			expr.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		case '\n':
			expr.WriteString(`\r?\n`)
			i++
		case ' ', '\t':
			for i < len(pattern) && (pattern[i] == ' ' || pattern[i] == '\t') {
				i++
			}
			expr.WriteString(`[ \t]+`)
		default:
			start := i
			for i < len(pattern) && pattern[i] != '\r' && pattern[i] != '\n' && pattern[i] != ' ' && pattern[i] != '\t' {
				i++
			}
			expr.WriteString(regexp.QuoteMeta(pattern[start:i]))
		}
	}
	re, err := regexp.Compile(expr.String())
	if err != nil {
		return "", 0
	}
	matches := re.FindAllStringIndex(text, 2)
	if len(matches) != 1 {
		return "", len(matches)
	}
	return text[matches[0][0]:matches[0][1]], 1
}

func appendEditDiff(out *strings.Builder, text, old, nw string, count int) {
	if count <= 0 || old == "" {
		return
	}
	from := 0
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(nw, "\n")
	for i := 0; i < count; i++ {
		rel := strings.Index(text[from:], old)
		if rel < 0 {
			break
		}
		pos := from + rel
		line := 1 + strings.Count(text[:pos], "\n")
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", line, len(oldLines), line, len(newLines))
		for _, value := range oldLines {
			out.WriteByte('-')
			out.WriteString(value)
			out.WriteByte('\n')
		}
		for _, value := range newLines {
			out.WriteByte('+')
			out.WriteString(value)
			out.WriteByte('\n')
		}
		from = pos + len(old)
	}
}

func inventoryResponseFromCache(cached []any, q, sn string, limit int64, configPath string, stale bool) map[string]any {
	out := make([]any, 0, len(cached))
	toolsOut := make([]any, 0)
	for _, item := range cached {
		server, _ := item.(map[string]any)
		if server == nil {
			continue
		}
		name := stringValue(server["name"])
		if sn != "" && name != sn {
			continue
		}
		matchedServer := q == "" || strings.Contains(strings.ToLower(name+" "+stringValue(server["command"])+" "+stringValue(server["url"])), q)
		matchedTool := false
		if rawTools, ok := server["tools"].([]any); ok {
			for _, rawTool := range rawTools {
				tool, _ := rawTool.(map[string]any)
				if tool == nil {
					continue
				}
				toolName := stringValue(tool["name"])
				desc := stringValue(tool["description"])
				if q != "" && !strings.Contains(strings.ToLower(toolName+" "+desc+" "+name), q) {
					continue
				}
				matchedTool = true
				copyTool := make(map[string]any, len(tool)+1)
				for k, v := range tool {
					copyTool[k] = v
				}
				copyTool["server"] = name
				toolsOut = append(toolsOut, copyTool)
			}
		}
		if matchedServer || matchedTool {
			copyServer := make(map[string]any, len(server))
			for k, v := range server {
				if k != "tools" {
					copyServer[k] = v
				}
			}
			out = append(out, copyServer)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)
		b, _ := out[j].(map[string]any)
		return stringValue(a["name"]) < stringValue(b["name"])
	})
	sort.Slice(toolsOut, func(i, j int) bool {
		a, _ := toolsOut[i].(map[string]any)
		b, _ := toolsOut[j].(map[string]any)
		return stringValue(a["server"])+"."+stringValue(a["name"]) < stringValue(b["server"])+"."+stringValue(b["name"])
	})
	return map[string]any{
		"servers":     out,
		"apps":        []any{},
		"tools":       toolsOut,
		"query":       q,
		"server_name": nilIfEmpty(sn),
		"limit":       limit,
		"tool_count":  len(toolsOut),
		"truncated":   false,
		"refreshed":   false,
		"stale":       stale,
		"source":      "inventory_cache",
		"config_path": configPath,
	}
}

func (s *Server) discover(ctx context.Context, args map[string]any) (map[string]any, error) {
	servers, configPath, err := readCodexMCPConfig()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(stringValue(args["query"])))
	sn := strings.TrimSpace(stringValue(args["server_name"]))
	limit := int64(50)
	if n, ok := numberAsInt(args["limit"]); ok && n > 0 {
		limit = n
	}
	refresh := boolValue(args["refresh"], false) || sn != ""
	forceInventoryRefresh := boolValue(args["_force_inventory_refresh"], false)

	if refresh && !forceInventoryRefresh {
		if cached, cachedAt, ok := s.cachedInventory(); ok {
			stale := time.Since(cachedAt) > 5*time.Minute
			if stale {
				s.refreshInventoryInBackground()
			}
			return inventoryResponseFromCache(cached, q, sn, limit, configPath, stale), nil
		}
	}

	// The common path is intentionally cheap: listing configured MCP servers must
	// not start every stdio server. The old implementation probed every server
	// with initialize + tools/list on every call, so one slow MCP could make the
	// whole list take ~12s. Deep tool inventory is now explicit via refresh=true.
	if !refresh {
		out := make([]any, 0, len(servers))
		for _, server := range servers {
			name := stringValue(server["name"])
			if sn != "" && name != sn {
				continue
			}
			haystack := strings.ToLower(name + " " + stringValue(server["command"]) + " " + stringValue(server["url"]))
			if q != "" && !strings.Contains(haystack, q) {
				continue
			}
			out = append(out, server)
		}
		sort.Slice(out, func(i, j int) bool {
			a, _ := out[i].(map[string]any)
			b, _ := out[j].(map[string]any)
			return stringValue(a["name"]) < stringValue(b["name"])
		})
		return map[string]any{
			"servers":     out,
			"apps":        []any{},
			"tools":       []any{},
			"query":       stringValue(args["query"]),
			"server_name": nilIfEmpty(sn),
			"limit":       limit,
			"truncated":   false,
			"refreshed":   false,
			"source":      "codex_effective_config",
			"config_path": configPath,
		}, nil
	}

	type probeResult struct {
		server map[string]any
		tools  []any
		err    error
	}
	candidates := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		name := stringValue(server["name"])
		if sn != "" && name != sn {
			continue
		}
		candidates = append(candidates, server)
	}
	results := make(chan probeResult, len(candidates))
	var wg sync.WaitGroup
	for _, server := range candidates {
		server := server
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools, err := probeCodexMCPTools(ctx, stringValue(server["name"]))
			results <- probeResult{server: server, tools: tools, err: err}
		}()
	}
	wg.Wait()
	close(results)
	out := make([]any, 0, len(candidates))
	toolsOut := make([]any, 0)
	cacheServers := make([]any, 0, len(candidates))
	for pr := range results {
		server := pr.server
		name := stringValue(server["name"])
		server["toolCount"] = len(pr.tools)
		if pr.err != nil {
			server["inventory_error"] = pr.err.Error()
		}
		cachedServer := make(map[string]any, len(server)+1)
		for k, v := range server {
			cachedServer[k] = v
		}
		cachedServer["tools"] = pr.tools
		cacheServers = append(cacheServers, cachedServer)
		matchedServer := q == "" || strings.Contains(strings.ToLower(name+" "+stringValue(server["command"])+" "+stringValue(server["url"])), q)
		matchedTool := false
		for _, item := range pr.tools {
			tm, _ := item.(map[string]any)
			if tm == nil {
				continue
			}
			toolName := stringValue(tm["name"])
			desc := stringValue(tm["description"])
			if q != "" && !strings.Contains(strings.ToLower(toolName+" "+desc+" "+name), q) {
				continue
			}
			matchedTool = true
			tm["server"] = name
			toolsOut = append(toolsOut, tm)
		}
		if matchedServer || matchedTool {
			out = append(out, server)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)
		b, _ := out[j].(map[string]any)
		return stringValue(a["name"]) < stringValue(b["name"])
	})
	sort.Slice(toolsOut, func(i, j int) bool {
		a, _ := toolsOut[i].(map[string]any)
		b, _ := toolsOut[j].(map[string]any)
		return stringValue(a["server"])+"."+stringValue(a["name"]) < stringValue(b["server"])+"."+stringValue(b["name"])
	})
	allToolCount := len(toolsOut)
	if sn == "" {
		s.saveInventoryCache(cacheServers)
	}
	apps := []any{}
	var installed map[string]any
	if err := s.app.Request(ctx, "app/installed", map[string]any{"forceRefresh": false}, &installed); err == nil {
		if raw, ok := installed["apps"].([]any); ok {
			for _, item := range raw {
				m, _ := item.(map[string]any)
				if m == nil {
					continue
				}
				apps = append(apps, map[string]any{
					"id": m["id"], "name": m["runtimeName"],
					"enabled": m["enabled"], "callable": m["callable"],
					"source": "codex_apps",
				})
			}
		}
	}
	return map[string]any{
		"servers":     out,
		"apps":        apps,
		"tools":       toolsOut,
		"query":       stringValue(args["query"]),
		"server_name": nilIfEmpty(sn),
		"limit":       limit,
		"tool_count":  allToolCount,
		"truncated":   false,
		"refreshed":   true,
		"source":      "codex_effective_config+probed_tools+app_installed",
		"config_path": configPath,
	}, nil
}

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

func (s *Server) invalidateInventory() {
	s.inventoryMu.Lock()
	s.inventory = nil
	s.inventoryAt = time.Time{}
	s.inventoryMu.Unlock()
}

func (s *Server) computer(ctx context.Context, args map[string]any) (map[string]any, error) {
	_ = ctx
	switch stringValue(args["action"]) {
	case "screen_info":
		return computer.ScreenInfo(), nil
	case "screenshot":
		pngData, info, err := computer.ScreenshotPNG()
		if err != nil {
			return nil, err
		}
		info["_image"] = base64.StdEncoding.EncodeToString(pngData)
		return info, nil
	case "move":
		return computer.Move(int(intValue(args["x"])), int(intValue(args["y"])), int(intValue(args["duration_ms"])))
	case "click":
		var x, y *int
		if _, ok := args["x"]; ok {
			xv := int(intValue(args["x"]))
			x = &xv
		}
		if _, ok := args["y"]; ok {
			yv := int(intValue(args["y"]))
			y = &yv
		}
		button := stringValue(args["button"])
		if button == "" {
			button = "left"
		}
		clicks := int(intValue(args["clicks"]))
		if clicks == 0 {
			clicks = 1
		}
		return computer.Click(x, y, button, clicks)
	case "scroll":
		return computer.Scroll(int(intValue(args["delta_x"])), int(intValue(args["delta_y"]))), nil
	case "type":
		return computer.TypeText(stringValue(args["text"]), int(intValue(args["interval_ms"])))
	case "keypress":
		var keys []string
		switch raw := args["keys"].(type) {
		case string:
			keys = strings.Fields(strings.ReplaceAll(raw, "+", " "))
		case []any:
			for _, item := range raw {
				keys = append(keys, stringValue(item))
			}
		}
		return computer.Keypress(keys)
	default:
		return nil, fmt.Errorf("unsupported computer action: %s", stringValue(args["action"]))
	}
}

func toolResult(v any) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": mustJSON(v)}}, "structuredContent": v, "isError": false}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func logJSON(v any, limit int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	if limit > 0 && len(b) > limit {
		return string(b[:limit-1]) + "ÃÂ Ã‚Â ÃÂ Ã¢â‚¬Â ÃÂ Ã‚Â ÃÂ²Ãâ€šÃ‘â„¢ÃÂ Ã¢â‚¬â„¢Ãâ€™Ã‚Â¦"
	}
	return string(b)
}

func logResultPayload(result any) any {
	if m, ok := result.(map[string]any); ok {
		if v, exists := m["structuredContent"]; exists {
			return v
		}
	}
	return result
}

func copyTargetFields(dst map[string]any, src map[string]any) {
	for _, key := range []string{"path", "filepath", "source_path", "destination_path", "output", "destination"} {
		if v, ok := src[key].(string); ok && v != "" {
			if key == "path" || key == "filepath" {
				dst["target_path"] = v
			}
			dst[key] = v
		}
	}
}

func copyResultFields(dst map[string]any, payload any) {
	m, ok := payload.(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"path", "filepath", "size_bytes", "encoding", "newline", "status", "job_id", "written", "changed", "replacements", "dry_run", "diff", "media_path", "exitCode", "exit_code"} {
		if v, exists := m[key]; exists {
			dst[key] = v
		}
	}
	if v, ok := m["path"].(string); ok && v != "" {
		dst["target_path"] = v
	}
}
func mustJSON(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
func sum(b []byte) []byte   { h := sha256.Sum256(b); return h[:] }
func numberAsInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}
func intValue(v any) int64 { n, _ := numberAsInt(v); return n }
func boolValue(v any, d bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return d
}
func stringValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
func mapValue(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (s *Server) write(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, e = fmt.Fprintf(s.out, "%s\n", b)
	return e
}

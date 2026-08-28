package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCommandProcessIDsAreUniqueConcurrently(t *testing.T) {
	const count = 1000
	ids := make(chan string, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			ids <- nextCommandProcessID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, count)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate process id: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestTerminalEncodingGuardRestoresBrokenText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.txt")
	original := []byte("привет\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	snaps := snapshotTargets([]string{path})
	if err := os.WriteFile(path, []byte{0xcf, 0xf0, 0xe8, 0xe2, 0xe5, 0xf2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateAndRestoreTextTargets(snaps); err == nil {
		t.Fatal("expected non-UTF-8 terminal write to fail validation")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("original text was not restored: %q", got)
	}
}

func TestTerminalWriteTargetDetection(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		cmd  []string
		want string
	}{
		{[]string{"cmd", "/c", `echo hello > x.txt`}, filepath.Join(dir, "x.txt")},
		{[]string{"powershell", "-Command", `Set-Content -LiteralPath 'a.json' -Value '{}'`}, filepath.Join(dir, "a.json")},
		{[]string{"python", "-c", `open('b.py','w').write('x')`}, filepath.Join(dir, "b.py")},
		{[]string{"node", "-e", `require('fs').writeFileSync('c.ts','x')`}, filepath.Join(dir, "c.ts")},
	}
	for _, tc := range cases {
		got := detectTextWriteTargets(tc.cmd, dir)
		if len(got) == 0 || filepath.Clean(got[0]) != filepath.Clean(tc.want) {
			t.Fatalf("target detection failed for %#v: got %#v want %s", tc.cmd, got, tc.want)
		}
	}
}

func TestSensitiveCommandDetectionAndRedaction(t *testing.T) {
	args := map[string]any{"command": []any{"curl", "-H", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz"}, "env": map[string]any{"OPENAI_API_KEY": "sk-abcdefghijklmnopqrstuvwxyz123456"}}
	if !containsSensitive(args) {
		t.Fatal("expected sensitive command to require approval")
	}
	redacted := redactSensitive(args)
	text := logJSON(redacted, 4000)
	if text == "" || text == logJSON(args, 4000) || strings.Contains(text, "sk-abcdefghijklmnopqrstuvwxyz123456") || strings.Contains(text, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("redaction failed: %s", text)
	}
}

func TestReadOnlyDoesNotBypassApproval(t *testing.T) {
	args := map[string]any{"command": []any{"cmd", "/c", "echo approval"}, "read_only": true, "require_approval": true}
	if !commandNeedsApproval(args) {
		t.Fatal("read_only=true must not bypass explicit require_approval=true")
	}
	args = map[string]any{"command": []any{"cmd", "/c", "echo secret"}, "read_only": true, "env": map[string]any{"OPENAI_API_KEY": "sk-abcdefghijklmnopqrstuvwxyz123456"}}
	if !commandNeedsApproval(args) {
		t.Fatal("read_only=true must not bypass sensitive-data approval")
	}
	args["_approval_granted"] = true
	if commandNeedsApproval(args) {
		t.Fatal("_approval_granted=true must prevent a second approval loop")
	}
}

func TestShellExecCommand(t *testing.T) {
	argv, err := shellExecCommand(map[string]any{"script": "npm ci; npm run build", "shell": "powershell"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(argv))
	for i, v := range argv {
		got[i] = v.(string)
	}
	if got[len(got)-1] != "npm ci; npm run build" {
		t.Fatalf("script was rewritten: %#v", got)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %#v", want, got)
		}
	}
	if _, err := shellExecCommand(map[string]any{"script": "echo ok", "shell": "wat"}); err == nil {
		t.Fatal("expected unsupported shell error")
	}
}

func TestSoftCommandTimeoutDoesNotCompleteSession(t *testing.T) {
	session := &commandSession{started: time.Now(), done: make(chan struct{}), timeoutMs: 10}
	armSoftCommandTimeout(session, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	session.mu.Lock()
	reached := session.timedOut
	session.mu.Unlock()
	if !reached {
		t.Fatal("soft timeout was not reported")
	}
	select {
	case <-session.done:
		t.Fatal("soft timeout must not complete or terminate the command session")
	default:
	}

	snapshot := sessionSnapshot(session, false)
	if snapshot["status"] != "running" || snapshot["timeout_reached"] != true {
		t.Fatalf("unexpected soft-timeout snapshot: %#v", snapshot)
	}
}

func TestWindowsCommandResolutionPrefersPATHEXTShim(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific command resolution")
	}

	dir := t.TempDir()
	bare := filepath.Join(dir, "demo")
	cmdPath := bare + ".cmd"
	if err := os.WriteFile(bare, []byte("#!/usr/bin/env bash\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cmdPath, []byte("@echo off\r\necho native\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	resolved := resolveWindowsCommand("demo")
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(cmdPath)) {
		t.Fatalf("resolved wrong Windows shim: got %q want %q", resolved, cmdPath)
	}
}

func TestNormalizeWindowsBatchPreservesArgv(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific command normalization")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "tool.cmd")
	if err := os.WriteFile(script, []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	argv := []string{script, "hello world", "100%", "a&b", `quote"inside`}
	got := normalizeWindowsCommand(argv)
	if len(got) != len(argv)+3 {
		t.Fatalf("unexpected normalized argv length: %#v", got)
	}
	if !strings.EqualFold(filepath.Base(got[0]), "cmd.exe") || got[1] != "/d" || got[2] != "/c" {
		t.Fatalf("batch command was not routed through cmd.exe: %#v", got)
	}
	if !strings.EqualFold(filepath.Clean(got[3]), filepath.Clean(script)) {
		t.Fatalf("script path changed: %#v", got)
	}
	for i, want := range argv[1:] {
		if got[i+4] != want {
			t.Fatalf("argument %d was rewritten: got %q want %q; argv=%#v", i, got[i+4], want, got)
		}
	}
}

func TestNormalizePowerShellCommandPropagatesFailures(t *testing.T) {
	got := normalizePowerShellCommand([]string{"powershell.exe", "-NoProfile", "-Command", "Get-Item missing-file"})
	if len(got) != 4 {
		t.Fatalf("unexpected argv: %#v", got)
	}
	if !strings.Contains(got[3], "$ErrorActionPreference = 'Stop'") || !strings.Contains(got[3], "$global:LASTEXITCODE") || !strings.Contains(got[3], "exit 1") {
		t.Fatalf("PowerShell command was not wrapped for reliable exit propagation: %q", got[3])
	}
}

func TestWindowsCommandToolSurface(t *testing.T) {
	listed := tools()
	byName := make(map[string]Tool, len(listed))
	for _, tool := range listed {
		byName[tool.Name] = tool
	}

	sessionTools := []string{"command_poll", "command_write", "command_terminate"}
	if runtime.GOOS == "windows" {
		for _, name := range sessionTools {
			if _, ok := byName[name]; !ok {
				t.Errorf("%s must be advertised on Windows native sessions", name)
			}
		}
		if _, ok := byName["command_resize"]; ok {
			t.Error("command_resize must remain hidden on Windows until ConPTY support exists")
		}
		execTool, ok := byName["command_exec"]
		if !ok {
			t.Fatal("command_exec missing on Windows")
		}
		props, _ := execTool.InputSchema["properties"].(map[string]any)
		for _, name := range []string{"background", "stream_stdin", "require_approval", "approval_reason"} {
			if _, ok := props[name]; !ok {
				t.Errorf("command_exec property %s must be advertised on Windows native sessions", name)
			}
		}
		for _, name := range []string{"tty", "rows", "cols"} {
			if _, ok := props[name]; ok {
				t.Errorf("command_exec property %s must not be advertised on Windows", name)
			}
		}
		return
	}

	for _, name := range append(sessionTools, "command_resize") {
		if _, ok := byName[name]; !ok {
			t.Errorf("%s must remain available on non-Windows platforms", name)
		}
	}
}

func TestToolDescriptionsPreferFilesystemForFileIO(t *testing.T) {
	byName := make(map[string]Tool)
	for _, tool := range tools() {
		byName[tool.Name] = tool
	}

	for _, name := range []string{"fs_read_file", "fs_edit_file", "fs_write_file", "fs_read_directory"} {
		desc := byName[name].Description
		if !strings.Contains(desc, "Canonical") {
			t.Errorf("%s description must identify it as the canonical filesystem path: %q", name, desc)
		}
	}

	for _, name := range []string{"command_exec", "shell_exec"} {
		desc := byName[name].Description
		if !strings.Contains(desc, "Do NOT") || !strings.Contains(desc, "fs_") {
			t.Errorf("%s description must steer plain file I/O to fs_* tools: %q", name, desc)
		}
	}
}

func TestFSSearchFindsNamesAndContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("hello needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "needle-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{workspace: dir, allowedRoots: []string{dir}}
	got, err := s.fsSearch(context.Background(), map[string]any{"path": dir, "query": "needle", "mode": "both"})
	if err != nil {
		t.Fatal(err)
	}
	results, _ := got["results"].([]any)
	if len(results) < 2 {
		t.Fatalf("expected name and content matches, got %#v", got)
	}
	foundContent, foundDirectory := false, false
	for _, raw := range results {
		item, _ := raw.(map[string]any)
		if item["kind"] == "content" && item["line"] == 1 {
			foundContent = true
		}
		if item["kind"] == "directory" && item["name"] == "needle-dir" {
			foundDirectory = true
		}
	}
	if !foundContent || !foundDirectory {
		t.Fatalf("missing expected search results: %#v", results)
	}
}

func TestFSSearchSkipsVirtualEnvironments(t *testing.T) {
	dir := t.TempDir()
	venv := filepath.Join(dir, "venv", "Lib", "site-packages")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "slow.py"), []byte("hiddenneedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("normal text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{workspace: dir, allowedRoots: []string{dir}}
	got, err := s.fsSearch(context.Background(), map[string]any{"path": dir, "query": "hiddenneedle", "mode": "content"})
	if err != nil {
		t.Fatal(err)
	}
	if got["count"] != 0 {
		t.Fatalf("venv content must be skipped, got %#v", got)
	}
}

func TestMCPToolSurface(t *testing.T) {
	want := map[string]bool{
		"session_create":    false,
		"session_list":      false,
		"read_rules":        false,
		"fs_search":         false,
		"command_inspect":   false,
		"mcp_discover":      false,
		"mcp_call":          false,
		"mcp_resource_read": false,
		"mcp_reload":        false,
		"mcp_oauth_login":   false,
	}
	for _, tool := range tools() {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing MCP tool %q", name)
		}
	}

	if !toolAnnotations("read_rules")["readOnlyHint"].(bool) {
		t.Fatal("read_rules must be annotated read-only")
	}
	if !toolAnnotations("fs_search")["readOnlyHint"].(bool) {
		t.Fatal("fs_search must be annotated read-only")
	}
	if !toolAnnotations("command_inspect")["readOnlyHint"].(bool) {
		t.Fatal("command_inspect must be annotated read-only")
	}
	if !toolAnnotations("command_inspect")["openWorldHint"].(bool) {
		t.Fatal("command_inspect must be annotated open-world because read-only SSH/network inspection crosses the local trust boundary")
	}
	if !toolAnnotations("command_exec")["openWorldHint"].(bool) {
		t.Fatal("command_exec must remain annotated open-world")
	}
	if !toolAnnotations("shell_exec")["openWorldHint"].(bool) {
		t.Fatal("shell_exec must remain annotated open-world")
	}
	if !toolAnnotations("mcp_discover")["readOnlyHint"].(bool) {
		t.Fatal("mcp_discover must be annotated read-only")
	}
	if !toolAnnotations("mcp_resource_read")["readOnlyHint"].(bool) {
		t.Fatal("mcp_resource_read must be annotated read-only")
	}
	if !toolAnnotations("mcp_call")["openWorldHint"].(bool) {
		t.Fatal("mcp_call must be annotated open-world")
	}
}

func TestSessionSchemaIsRequiredForWorkingTools(t *testing.T) {
	byName := make(map[string]Tool)
	for _, tool := range tools() {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"read_rules", "fs_search", "fs_read_file", "multi_tool", "command_exec", "mcp_call"} {
		tool := byName[name]
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["session_id"]; !ok {
			t.Fatalf("%s must expose session_id", name)
		}
		required, _ := tool.InputSchema["required"].([]string)
		found := false
		for _, field := range required {
			if field == "session_id" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s must require session_id: %#v", name, required)
		}
	}
	for _, name := range []string{"connector_status", "session_create", "session_list"} {
		tool := byName[name]
		props, _ := tool.InputSchema["properties"].(map[string]any)
		if _, ok := props["session_id"]; ok {
			t.Fatalf("%s must remain usable before a session exists", name)
		}
	}
}

func TestStoredSessionsReloadOnStartup(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CODEXPC_STATE_DIR", state)
	original := &Server{sessions: make(map[string]chatSession)}
	created, err := original.createSession("Persistent chat")
	if err != nil {
		t.Fatal(err)
	}

	reloaded := &Server{sessions: make(map[string]chatSession)}
	reloaded.loadSessions()
	item, ok := reloaded.sessionByID(created.ID)
	if !ok {
		t.Fatalf("persisted session missing after reload: %#v", created)
	}
	if item.Name != created.Name || item.CreatedAt != created.CreatedAt {
		t.Fatalf("unexpected reloaded session: %#v", item)
	}
}

func TestDeletedSessionTombstonePreventsResurrection(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CODEXPC_STATE_DIR", state)
	s := &Server{sessions: make(map[string]chatSession)}
	created, err := s.createSession("Disposable chat")
	if err != nil {
		t.Fatal(err)
	}
	deletedDir := filepath.Join(state, "deleted-sessions")
	if err := os.MkdirAll(deletedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deletedDir, created.ID), []byte("deleted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.sessionByID(created.ID); ok {
		t.Fatal("deleted session must not remain addressable")
	}
	if _, err := s.touchSession(created.ID); err == nil {
		t.Fatal("deleted session must not be resurrected by activity")
	}
	if got := s.listSessions(); len(got) != 0 {
		t.Fatalf("deleted session must be filtered from runtime list: %#v", got)
	}
	s.sessionsMu.Lock()
	if err := s.saveSessionsLocked(); err != nil {
		s.sessionsMu.Unlock()
		t.Fatal(err)
	}
	s.sessionsMu.Unlock()
	reloaded := &Server{sessions: make(map[string]chatSession)}
	reloaded.loadSessions()
	if _, ok := reloaded.sessionByID(created.ID); ok {
		t.Fatal("deleted session must stay deleted after restart")
	}
}

func TestChatSessionPersistenceAndProcessOwnership(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CODEXPC_STATE_DIR", state)
	s := &Server{sessions: make(map[string]chatSession)}
	created, err := s.createSession("Fix connector UI")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "Fix connector UI" {
		t.Fatalf("unexpected session: %#v", created)
	}
	if _, ok := s.sessionByID(created.ID); !ok {
		t.Fatalf("session missing from runtime map: %#v", created)
	}
	if _, err := os.Stat(s.sessionsPath()); err != nil {
		t.Fatalf("persistent session must write sessions.json: %v", err)
	}
	const oldUpdatedAt = "2000-01-01T00:00:00Z"
	s.sessionsMu.Lock()
	stale := s.sessions[created.ID]
	stale.UpdatedAt = oldUpdatedAt
	s.sessions[created.ID] = stale
	s.sessionsMu.Unlock()
	touched, err := s.touchSession(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if touched.UpdatedAt <= oldUpdatedAt {
		t.Fatalf("session updated_at did not advance: before=%s after=%s", oldUpdatedAt, touched.UpdatedAt)
	}

	process := &commandSession{chatSessionID: created.ID}
	if err := ensureCommandSessionOwner(process, map[string]any{"session_id": created.ID}); err != nil {
		t.Fatalf("owner session rejected: %v", err)
	}
	if err := ensureCommandSessionOwner(process, map[string]any{"session_id": "session-other"}); err == nil {
		t.Fatal("cross-session process access must be rejected")
	}
}

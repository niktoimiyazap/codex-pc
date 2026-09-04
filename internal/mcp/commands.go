package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

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

		terminateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.terminateCommand(terminateCtx, pid, session, 3*time.Second)
		cancel()
		return sessionSnapshot(session, false), ctx.Err()
	}
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
	if group, err := attachProcessGroup(command.Process.Pid, maxCommandProcesses); err == nil {
		session.mu.Lock()
		session.processGroup = group
		session.mu.Unlock()
		s.startCommandSupervisor(session)
	} else if s.logger != nil {
		s.logger.Event("WARN", "command_process_group_unavailable", map[string]any{"tool": "command_exec", "call_id": callID, "process_id": pid, "pid": command.Process.Pid, "error": err.Error()})
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
		if session.processGroup != 0 {
			closeProcessGroup(session.processGroup)
			session.processGroup = 0
		}
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

		return sessionSnapshot(session, false), nil
	case <-time.After(time.Duration(yield) * time.Millisecond):
		session.mu.Lock()
		session.yielded = true
		session.mu.Unlock()
		return sessionSnapshot(session, false), nil
	case <-ctx.Done():

		terminateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.terminateCommand(terminateCtx, pid, session, 3*time.Second)
		cancel()
		return sessionSnapshot(session, false), ctx.Err()
	}
}

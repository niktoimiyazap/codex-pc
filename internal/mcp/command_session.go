package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const maxCommandProcesses uint32 = 128

var commandProcessSequence uint64

func nextCommandProcessID() string {
	return fmt.Sprintf("go-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&commandProcessSequence, 1))
}

type commandSession struct {
	mu               sync.Mutex
	processID        string
	callID           string
	chatSessionID    string
	stdout           bytes.Buffer
	stderr           bytes.Buffer
	stdoutRead       int
	stderrRead       int
	capReached       bool
	yielded          bool
	started          time.Time
	lastOutput       time.Time
	outputBytes      int64
	outputCap        int64
	done             chan struct{}
	result           map[string]any
	err              error
	timedOut         bool
	timeoutMs        int64
	unregister       func()
	local            bool
	cmd              *exec.Cmd
	processGroup     uintptr
	supervisorReason string
	stdin            io.WriteCloser
	approvalState    string
	approvalID       string
	approvalReason   string
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

	return originalLen, nil
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
	if session.processGroup != 0 {
		out["supervised"] = true
		out["process_limit"] = maxCommandProcesses
	}
	if session.supervisorReason != "" {
		out["supervisor_reason"] = session.supervisorReason
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
		group := session.processGroup
		cmd := session.cmd
		if group != 0 {
			_ = terminateProcessGroup(group)
		}
		session.mu.Unlock()
		if group == 0 && cmd != nil && cmd.Process != nil {

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

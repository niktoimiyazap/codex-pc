package mcp

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// commandList is intentionally session-agnostic. It is the connector control
// plane: a wedged chat must not hide the process that wedged it.
func (s *Server) commandList() map[string]any {
	s.commandsMu.Lock()
	entries := make([]*commandSession, 0, len(s.commands))
	for _, session := range s.commands {
		if session != nil {
			entries = append(entries, session)
		}
	}
	s.commandsMu.Unlock()

	items := make([]map[string]any, 0, len(entries))
	for _, session := range entries {
		snapshot := sessionSnapshot(session, false)
		status := stringValue(snapshot["status"])
		if status != "running" && status != "awaiting_approval" {
			continue
		}
		item := map[string]any{
			"process_id":          session.processID,
			"session_id":          session.chatSessionID,
			"status":              snapshot["status"],
			"elapsed_sec":         snapshot["elapsed_sec"],
			"last_output_sec_ago": snapshot["last_output_sec_ago"],
			"output_bytes":        snapshot["output_bytes"],
			"cap_reached":         snapshot["cap_reached"],
			"local":               session.local,
		}
		if pid, ok := snapshot["pid"]; ok {
			item["pid"] = pid
		}
		if timeout, ok := snapshot["timeout_ms"]; ok {
			item["timeout_ms"] = timeout
		}
		if reached, ok := snapshot["timeout_reached"]; ok {
			item["timeout_reached"] = reached
		}
		if supervised, ok := snapshot["supervised"]; ok {
			item["supervised"] = supervised
			item["process_limit"] = snapshot["process_limit"]
		}
		if reason, ok := snapshot["supervisor_reason"]; ok {
			item["supervisor_reason"] = reason
		}
		if session.chatSessionID != "" {
			if chat, ok := s.sessionByID(session.chatSessionID); ok {
				item["session_name"] = chat.Name
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return numberValue(items[i]["elapsed_sec"]) > numberValue(items[j]["elapsed_sec"])
	})
	return map[string]any{"processes": items, "count": len(items), "scope": "active"}
}

func numberValue(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func (s *Server) startCommandSupervisor(session *commandSession) {
	if session == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-session.done:
				return
			case <-ticker.C:
				session.mu.Lock()
				group := session.processGroup
				if group == 0 {
					session.mu.Unlock()
					return
				}
				active, err := processGroupProcessCount(group)
				if err == nil && active >= maxCommandProcesses {
					session.supervisorReason = "process_limit_reached"
					_ = terminateProcessGroup(group)
				}
				session.mu.Unlock()
				if err == nil && active >= maxCommandProcesses {
					if s.logger != nil {
						s.logger.Event("ERROR", "command_supervisor_terminated", map[string]any{
							"tool": "command_exec", "process_id": session.processID,
							"session_id": session.chatSessionID, "active_processes": active,
							"limit": maxCommandProcesses, "reason": "process_limit_reached",
						})
					}
					return
				}
			}
		}
	}()
}

// commandEmergencyTerminate bypasses chat ownership. On Windows/local sessions
// it kills the OS process tree directly and therefore does not depend on the
// Codex app-server being responsive.
func (s *Server) commandEmergencyTerminate(ctx context.Context, processID string) (map[string]any, error) {
	if processID == "" {
		return nil, fmt.Errorf("process_id is required")
	}
	session, err := s.commandSession(processID)
	if err != nil {
		return nil, err
	}
	if session.local {
		session.mu.Lock()
		group := session.processGroup
		cmd := session.cmd
		if group != 0 {
			session.supervisorReason = "emergency_terminate"
			_ = terminateProcessGroup(group)
		}
		session.mu.Unlock()
		if group == 0 && cmd != nil && cmd.Process != nil {
			killWindowsProcessTree(cmd)
		}
	} else {
		// Non-local runners still need app-server cooperation, but bound this path
		// tightly so a dead backend cannot wedge the control request forever.
		terminateCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var response any
		if err := s.app.Request(terminateCtx, "command/exec/terminate", map[string]any{"processId": processID}, &response); err != nil {
			return sessionSnapshot(session, false), err
		}
	}

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-session.done:
	case <-timer.C:
	case <-ctx.Done():
		return sessionSnapshot(session, false), ctx.Err()
	}
	out := sessionSnapshot(session, false)
	out["emergency"] = true
	return out, nil
}

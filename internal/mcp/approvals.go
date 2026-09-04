package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func commandNeedsApproval(args map[string]any) bool {
	if boolValue(args["_approval_granted"], false) {
		return false
	}

	return boolValue(args["require_approval"], false) || len(credentialRefMap(args)) > 0 || containsSensitive(args)
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

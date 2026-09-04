package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

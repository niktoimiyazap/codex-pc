package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

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
	for _, key := range []string{"path", "filepath", "size_bytes", "encoding", "newline", "status", "job_id", "written", "changed", "replacements", "dry_run", "diff", "media_path", "exitCode", "exit_code", "process_id", "pid", "supervised", "process_limit", "supervisor_reason"} {
		if v, exists := m[key]; exists {
			dst[key] = v
		}
	}
	if v, ok := m["path"].(string); ok && v != "" {
		dst["target_path"] = v
	}
}

func mustJSON(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }

func sum(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

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

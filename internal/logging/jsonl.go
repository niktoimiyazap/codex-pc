package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Logger struct {
	mu   sync.Mutex
	path string
}

func New(stateDir string) (*Logger, error) {
	logDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(logDir, "connector.jsonl")
	if info, err := os.Stat(path); err == nil && info.Size() > 5*1024*1024 {
		_ = os.Remove(path + ".3")
		_ = os.Rename(path+".2", path+".3")
		_ = os.Rename(path+".1", path+".2")
		_ = os.Rename(path, path+".1")
	}
	return &Logger{path: path}, nil
}

func (l *Logger) Event(level, message string, data map[string]any) {
	if l == nil {
		return
	}
	now := time.Now()
	payload := map[string]any{"level": level, "logger": "codexpc", "message": message, "time": now.Format("2006-01-02T15:04:05.000")}
	if len(data) > 0 {
		payload["data"] = data
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s[%s] [%s]%s %s%s\x1b[0m\n", color(level, message), now.Format("15:04:05.000"), level, category(message), message, consoleFields(data))
}

func color(level, message string) string {
	switch {
	case level == "ERROR":
		return "\x1b[91m"
	case level == "WARN":
		return "\x1b[93m"
	case strings.Contains(message, "network") || strings.Contains(message, "stdio") || strings.Contains(message, "app_server"):
		return "\x1b[96m"
	case strings.Contains(message, "succeeded") || strings.Contains(message, "initialized") || message == "connector_start":
		return "\x1b[92m"
	case strings.Contains(message, "started") || strings.Contains(message, "stream"):
		return "\x1b[94m"
	default:
		return "\x1b[37m"
	}
}
func category(message string) string {
	switch {
	case strings.Contains(message, "network"):
		return " [NET]"
	case strings.Contains(message, "app_server"):
		return " [APP]"
	case strings.Contains(message, "stdio"):
		return " [IO]"
	case strings.Contains(message, "tool_call"):
		return " [TOOL]"
	case strings.Contains(message, "connector"):
		return " [CORE]"
	default:
		return ""
	}
}
func consoleFields(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	skip := map[string]bool{"input_preview": true, "output_preview": true, "error_preview": false, "delta": true, "diff": true, "batch_items": true}
	keys := make([]string, 0, len(data))
	for k := range data {
		if !skip[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := fmt.Sprintf("%v", data[k])
		if len(v) > 180 {
			v = v[:177] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	if e, ok := data["error_preview"]; ok {
		v := fmt.Sprintf("%v", e)
		if len(v) > 220 {
			v = v[:217] + "..."
		}
		parts = append(parts, "error="+v)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

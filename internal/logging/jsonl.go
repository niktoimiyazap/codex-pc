package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type logEvent struct {
	level   string
	message string
	data    map[string]any
	time    time.Time
}

type streamBatch struct {
	event logEvent
	count int
}

type Logger struct {
	path        string
	queue       chan logEvent
	streamQueue chan logEvent
	dropped     atomic.Uint64
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
	l := &Logger{path: path, queue: make(chan logEvent, 512), streamQueue: make(chan logEvent, 1536)}
	go l.run()
	return l, nil
}

// Event is deliberately non-blocking. Tool execution and emergency controls must
// never wait behind disk or stderr I/O. When the bounded queue is full we drop
// diagnostics rather than applying backpressure to the connector runtime.
func (l *Logger) Event(level, message string, data map[string]any) {
	if l == nil {
		return
	}
	e := logEvent{level: level, message: message, data: cloneData(data), time: time.Now()}
	target := l.queue
	if message == "chatgpt_tool_call_stream" && l.streamQueue != nil {
		target = l.streamQueue
	}
	select {
	case target <- e:
	default:
		l.dropped.Add(1)
	}
}

func cloneData(data map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}
	copy := make(map[string]any, len(data))
	for k, v := range data {
		copy[k] = v
	}
	return copy
}

func streamKey(e logEvent) string {
	if e.message != "chatgpt_tool_call_stream" || len(e.data) == 0 {
		return ""
	}
	return fmt.Sprintf("%v|%v|%v", e.data["call_id"], e.data["process_id"], e.data["stream"])
}

func (l *Logger) run() {
	file, _ := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if file != nil {
		defer file.Close()
	}
	ticker := time.NewTicker(125 * time.Millisecond)
	defer ticker.Stop()
	pending := make(map[string]*streamBatch)
	flushStreams := func() {
		for key, batch := range pending {
			if batch.count > 1 {
				batch.event.data["coalesced_chunks"] = batch.count
			}
			l.write(file, batch.event)
			delete(pending, key)
		}
	}
	queueStream := func(e logEvent) {
		key := streamKey(e)
		if key == "" {
			l.write(file, e)
			return
		}
		if batch := pending[key]; batch != nil {
			batch.count++
			batch.event.time = e.time
			if delta, ok := e.data["delta"].(string); ok {
				current, _ := batch.event.data["delta"].(string)
				if len(current) < 64*1024 {
					remaining := 64*1024 - len(current)
					if len(delta) > remaining {
						delta = delta[:remaining]
						batch.event.data["stream_batch_truncated"] = true
					}
					batch.event.data["delta"] = current + delta
				} else {
					batch.event.data["stream_batch_truncated"] = true
				}
			}
			return
		}
		pending[key] = &streamBatch{event: e, count: 1}
	}
	for {
		// Lifecycle/control events always get first chance to drain. A terminal
		// flood must not push start/yield/finish/error state behind stream noise.
		select {
		case e := <-l.queue:
			l.write(file, e)
			continue
		default:
		}
		select {
		case e := <-l.queue:
			l.write(file, e)
		case e := <-l.streamQueue:
			queueStream(e)
		case <-ticker.C:
			flushStreams()
			if dropped := l.dropped.Swap(0); dropped > 0 {
				l.write(file, logEvent{level: "WARN", message: "logger_events_dropped", time: time.Now(), data: map[string]any{"count": dropped, "reason": "bounded logging queue full"}})
			}
		}
	}
}

func (l *Logger) write(file *os.File, e logEvent) {
	payload := map[string]any{"level": e.level, "logger": "codexpc", "message": e.message, "time": e.time.Format("2006-01-02T15:04:05.000")}
	if len(e.data) > 0 {
		payload["data"] = e.data
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if file != nil {
		_, _ = file.Write(append(b, '\n'))
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s[%s] [%s]%s %s%s\x1b[0m\n", color(e.level, e.message), e.time.Format("15:04:05.000"), e.level, category(e.message), e.message, consoleFields(e.data))
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

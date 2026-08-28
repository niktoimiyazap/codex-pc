package appserver

import (
	"encoding/base64"
	"encoding/json"
	"sync"
)

type CommandChunk struct {
	ProcessID   string `json:"processId"`
	Stream      string `json:"stream"`
	DeltaBase64 string `json:"deltaBase64"`
	CapReached  bool   `json:"capReached"`
}

type StreamRegistry struct {
	mu       sync.RWMutex
	handlers map[string]func(stream string, data []byte, capReached bool)
}

func NewStreamRegistry() *StreamRegistry {
	return &StreamRegistry{handlers: make(map[string]func(string, []byte, bool))}
}

func (r *StreamRegistry) Register(processID string, fn func(stream string, data []byte, capReached bool)) func() {
	r.mu.Lock()
	r.handlers[processID] = fn
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.handlers, processID)
		r.mu.Unlock()
	}
}

func (r *StreamRegistry) Handle(method string, params json.RawMessage) {
	if method != "command/exec/outputDelta" {
		return
	}
	var chunk CommandChunk
	if json.Unmarshal(params, &chunk) != nil || chunk.ProcessID == "" {
		return
	}
	data, err := base64.StdEncoding.DecodeString(chunk.DeltaBase64)
	if err != nil {
		return
	}
	r.mu.RLock()
	fn := r.handlers[chunk.ProcessID]
	r.mu.RUnlock()
	if fn != nil {
		fn(chunk.Stream, data, chunk.CapReached)
	}
}

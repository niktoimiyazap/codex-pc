package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/niktoimiyazap/codexpc-connector/internal/appserver"
	logpkg "github.com/niktoimiyazap/codexpc-connector/internal/logging"
)

const ProtocolVersion = "2025-06-18"

type Server struct {
	app                 *appserver.Client
	streams             *appserver.StreamRegistry
	in                  *bufio.Scanner
	out                 io.Writer
	writeMu             sync.Mutex
	requestMu           sync.Mutex
	inFlight            map[string]*inFlightRequest
	protocolMu          sync.Mutex
	initialized         bool
	threadMu            sync.Mutex
	threadID            string
	started             time.Time
	workspace           string
	allowedRoots        []string
	logger              *logpkg.Logger
	inventoryMu         sync.Mutex
	inventory           []any
	inventoryAt         time.Time
	inventoryRefreshing bool
	commandsMu          sync.Mutex
	commands            map[string]*commandSession
	sessionsMu          sync.Mutex
	sessions            map[string]chatSession
	backendMu           sync.Mutex
	backendReady        chan struct{}
	backendErr          error
	backendOnce         sync.Once
}

type inFlightRequest struct {
	method string
	cancel context.CancelFunc
}

type chatSession struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func NewServer(app *appserver.Client, streams *appserver.StreamRegistry, logger *logpkg.Logger, input io.Reader, output io.Writer) *Server {
	s := bufio.NewScanner(input)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	home, _ := os.UserHomeDir()
	workspace := os.Getenv("CODEXPC_WORKSPACE")
	if workspace == "" {
		workspace = home
	}
	workspace, _ = filepath.Abs(workspace)
	roots := []string{home}
	if raw := os.Getenv("CODEXPC_ALLOWED_ROOTS"); raw != "" {
		roots = filepath.SplitList(raw)
	}
	for i := range roots {
		roots[i], _ = filepath.Abs(roots[i])
	}
	srv := &Server{app: app, streams: streams, in: s, out: output, started: time.Now(), workspace: workspace, allowedRoots: roots, logger: logger, inFlight: make(map[string]*inFlightRequest), commands: make(map[string]*commandSession), sessions: make(map[string]chatSession), backendReady: make(chan struct{})}
	srv.loadInventoryCache()
	srv.loadSessions()
	return srv
}

func (s *Server) MarkBackendReady(err error) {
	if s == nil {
		return
	}
	s.backendOnce.Do(func() {
		s.backendMu.Lock()
		s.backendErr = err
		s.backendMu.Unlock()
		if s.backendReady != nil {
			close(s.backendReady)
		}
	})
}

func (s *Server) waitBackend(ctx context.Context) error {
	if s == nil || s.backendReady == nil {
		return nil
	}
	select {
	case <-s.backendReady:
		s.backendMu.Lock()
		err := s.backendErr
		s.backendMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func toolNeedsBackend(name string) bool {
	if strings.HasPrefix(name, "fs_") || strings.HasPrefix(name, "mcp_") {
		return true
	}
	if runtime.GOOS != "windows" {
		switch name {
		case "command_exec", "command_inspect", "shell_exec", "command_write", "command_resize", "command_terminate":
			return true
		}
	}
	return false
}

func requestIDKey(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return "s:" + text
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err == nil {
		switch v := value.(type) {
		case json.Number:
			return "n:" + v.String()
		case nil:
			return "null"
		}
	}
	return string(trimmed)
}

func (s *Server) finishRequest(key string, request *inFlightRequest) {
	if key == "" || request == nil {
		return
	}
	s.requestMu.Lock()
	if current := s.inFlight[key]; current == request {
		delete(s.inFlight, key)
	}
	s.requestMu.Unlock()
}

func (s *Server) handleNotification(method string, raw json.RawMessage) {
	switch method {
	case "notifications/initialized":
		s.protocolMu.Lock()
		first := !s.initialized
		s.initialized = true
		s.protocolMu.Unlock()
		if first && s.logger != nil {
			s.logger.Event("INFO", "mcp_client_initialized", map[string]any{"protocol_version": ProtocolVersion})
		}
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason,omitempty"`
		}
		if err := json.Unmarshal(raw, &params); err != nil || len(params.RequestID) == 0 {
			return
		}
		key := requestIDKey(params.RequestID)
		s.requestMu.Lock()
		request := s.inFlight[key]
		if request == nil || request.method == "initialize" {
			s.requestMu.Unlock()
			return
		}
		cancel := request.cancel
		requestMethod := request.method
		s.requestMu.Unlock()
		cancel()
		if s.logger != nil {
			s.logger.Event("INFO", "mcp_request_cancelled", map[string]any{
				"request_id": strings.TrimSpace(string(params.RequestID)),
				"method":     requestMethod,
				"reason":     params.Reason,
			})
		}
	}
}

func (s *Server) Serve(ctx context.Context) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for s.in.Scan() {
		select {
		case <-serveCtx.Done():
			return serveCtx.Err()
		default:
		}
		line := bytes.TrimSpace(bytes.TrimPrefix(append([]byte(nil), s.in.Bytes()...), []byte{0xEF, 0xBB, 0xBF}))
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if len(msg.ID) == 0 {
			s.handleNotification(msg.Method, msg.Params)
			continue
		}

		request := msg
		requestCtx, requestCancel := context.WithCancel(serveCtx)
		requestKey := requestIDKey(request.ID)
		tracked := &inFlightRequest{method: request.Method, cancel: requestCancel}
		if requestKey != "" {
			s.requestMu.Lock()
			if s.inFlight == nil {
				s.inFlight = make(map[string]*inFlightRequest)
			}
			s.inFlight[requestKey] = tracked
			s.requestMu.Unlock()
		}
		go func() {
			defer requestCancel()
			defer s.finishRequest(requestKey, tracked)
			result, err := s.handle(requestCtx, request.Method, request.Params)

			if requestCtx.Err() != nil {
				return
			}
			resp := rpcResponse{JSONRPC: "2.0", ID: request.ID}
			if err != nil {
				resp.Error = &rpcError{Code: -32000, Message: err.Error()}
			} else {
				resp.Result = result
			}
			if err := s.write(resp); err != nil && serveCtx.Err() == nil && s.logger != nil {
				s.logger.Event("WARN", "mcp_response_write_failed", map[string]any{
					"method": request.Method,
					"error":  err.Error(),
				})
			}
		}()
	}
	cancel()
	return s.in.Err()
}

func (s *Server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "codexpc-go", "title": "CodexPC Go", "version": "0.4.0-dev"}}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var req struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		started := time.Now()
		callID := fmt.Sprintf("go-call-%d", started.UnixNano())
		var activeSession chatSession
		if toolRequiresSession(req.Name) {
			sessionID := strings.TrimSpace(stringValue(req.Arguments["session_id"]))
			if sessionID == "" {
				return nil, fmt.Errorf("SESSION_REQUIRED: call session_create with a short name before using %s", req.Name)
			}
			var sessionErr error
			activeSession, sessionErr = s.touchSession(sessionID)
			if sessionErr != nil {
				return nil, sessionErr
			}
		}
		if s.logger != nil {
			data := map[string]any{
				"tool": req.Name, "call_id": callID,
				"argument_count": len(req.Arguments),
				"argument_keys":  mapKeys(req.Arguments),
				"input_preview":  logJSON(redactSensitive(req.Arguments), 20000),
			}
			if activeSession.ID != "" {
				data["session_id"] = activeSession.ID
				data["session_name"] = activeSession.Name
			}
			copyTargetFields(data, req.Arguments)
			s.logger.Event("INFO", "chatgpt_tool_call_started", data)
		}
		var result any
		var err error
		if req.Name == "multi_tool" {
			result, err = s.multiTool(ctx, req.Arguments, callID)
		} else {
			result, err = s.callTool(ctx, req.Name, req.Arguments, callID)
		}
		if s.logger != nil {
			level, message := "INFO", "chatgpt_tool_call_succeeded"
			data := map[string]any{"tool": req.Name, "call_id": callID, "duration_ms": float64(time.Since(started).Microseconds()) / 1000}
			if activeSession.ID != "" {
				data["session_id"] = activeSession.ID
				data["session_name"] = activeSession.Name
			}
			if err != nil {
				level, message = "ERROR", "chatgpt_tool_call_failed"
				data["error_preview"] = err.Error()
			} else {
				payload := logResultPayload(result)
				if req.Name == "secret_vault" || req.Name == "credential_value" {
					payload = redactSecretVaultResult(payload)
				}
				previewLimit := 20000
				if req.Name == "read_rules" {
					previewLimit = 100000
				}
				data["output_preview"] = logJSON(redactSensitive(payload), previewLimit)
				copyResultFields(data, payload)
				if m, ok := payload.(map[string]any); ok && stringValue(m["status"]) == "running" {
					message = "chatgpt_tool_call_yielded"
					data["process_id"] = m["process_id"]
				}
			}
			s.logger.Event(level, message, data)
		}
		return result, err
	default:
		return nil, fmt.Errorf("unsupported method: %s", method)
	}
}

func (s *Server) write(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, e = fmt.Fprintf(s.out, "%s\n", b)
	return e
}

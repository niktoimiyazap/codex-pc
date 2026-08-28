package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
)

type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("app-server rpc error %d: %s", e.Code, e.Message)
}

type response struct {
	result json.RawMessage
	err    error
}

type NotificationHandler func(method string, params json.RawMessage)

type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	nextID  atomic.Uint64
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[uint64]chan response

	onNotification NotificationHandler
	workspace      string
	done           chan struct{}
	closeOnce      sync.Once
}

func Start(ctx context.Context, codexPath string, onNotification NotificationHandler) (*Client, error) {
	if codexPath == "" {
		codexPath = "codex"
	}
	cmd := exec.CommandContext(ctx, codexPath, "app-server", "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	workspace, _ := os.Getwd()
	c := &Client{
		cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, workspace: workspace,
		pending:        make(map[uint64]chan response),
		onNotification: onNotification,
		done:           make(chan struct{}),
	}
	go c.readLoop()
	go c.stderrLoop()
	go func() {
		err := cmd.Wait()
		c.failAll(fmt.Errorf("codex app-server exited: %w", err))
		c.closeOnce.Do(func() { close(c.done) })
	}()
	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		return c.cmd.Process.Kill()
	}
	return nil
}

func (c *Client) Notify(method string, params any) error {
	payload := map[string]any{"method": method, "params": params}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) Initialize(ctx context.Context, version string) error {
	var result json.RawMessage
	if err := c.Request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "codexpc_go",
			"title":   "CodexPC Go",
			"version": version,
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}, &result); err != nil {
		return err
	}
	return c.Notify("initialized", map[string]any{})
}
func (c *Client) Request(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload := map[string]any{"id": id, "method": method, "params": params}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	_, err = c.stdin.Write(append(b, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("codex app-server closed")
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out == nil || len(r.result) == 0 {
			return nil
		}
		return json.Unmarshal(r.result, out)
	}
}

func (c *Client) readLoop() {
	s := bufio.NewScanner(c.stdout)
	buf := make([]byte, 64*1024)
	s.Buffer(buf, 16*1024*1024)
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		var msg Message
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if len(msg.ID) > 0 && msg.Method != "" {
			c.handleServerRequest(msg)
			continue
		}
		if len(msg.ID) > 0 {
			var id uint64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			if msg.Error != nil {
				ch <- response{err: msg.Error}
			} else {
				ch <- response{result: msg.Result}
			}
			continue
		}
		if msg.Method != "" && c.onNotification != nil {
			c.onNotification(msg.Method, msg.Params)
		}
	}
	if err := s.Err(); err != nil {
		c.failAll(err)
	}
}

func (c *Client) handleServerRequest(msg Message) {
	var result any
	var rpcErr *RPCError
	switch msg.Method {
	case "roots/list":
		path := filepath.ToSlash(c.workspace)
		if len(path) >= 2 && path[1] == ':' {
			path = "/" + path
		}
		result = map[string]any{"roots": []map[string]any{{"uri": "file://" + path, "name": filepath.Base(c.workspace)}}}
	default:
		rpcErr = &RPCError{Code: -32601, Message: "unsupported server request: " + msg.Method}
	}
	payload := map[string]any{"id": json.RawMessage(msg.ID)}
	if rpcErr != nil {
		payload["error"] = rpcErr
	} else {
		payload["result"] = result
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.stdin.Write(append(b, '\n'))
}

func (c *Client) stderrLoop() {
	s := bufio.NewScanner(c.stderr)
	for s.Scan() {
		// Intentionally consumed here. Structured logging will be wired next.
	}
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- response{err: err}:
		default:
		}
		delete(c.pending, id)
	}
}

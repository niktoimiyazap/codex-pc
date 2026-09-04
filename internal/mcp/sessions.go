package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func stateDirectory() string {
	state := os.Getenv("CODEXPC_STATE_DIR")
	if state == "" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			state = filepath.Join(local, "CodexPCConnector")
		}
	}
	return state
}

func (s *Server) sessionsPath() string {
	state := stateDirectory()
	if state == "" {
		return ""
	}
	return filepath.Join(state, "sessions.json")
}

func (s *Server) deletedSessionPath(id string) string {
	state := stateDirectory()
	if state == "" || id == "" {
		return ""
	}
	return filepath.Join(state, "deleted-sessions", id)
}

func (s *Server) isSessionDeleted(id string) bool {
	path := s.deletedSessionPath(id)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (s *Server) loadSessions() {
	path := s.sessionsPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var payload struct {
		Version  int           `json:"version"`
		Sessions []chatSession `json:"sessions"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Version != 1 {
		return
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	for _, item := range payload.Sessions {
		if item.ID != "" && item.Name != "" && !s.isSessionDeleted(item.ID) {
			s.sessions[item.ID] = item
		}
	}
}

func (s *Server) saveSessionsLocked() error {
	path := s.sessionsPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	items := make([]chatSession, 0, len(s.sessions))
	for id, item := range s.sessions {
		if s.isSessionDeleted(id) {
			delete(s.sessions, id)
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	payload, err := json.MarshalIndent(map[string]any{"version": 1, "sessions": items}, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Server) createSession(name string) (chatSession, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return chatSession{}, fmt.Errorf("session name is required")
	}
	if len([]rune(name)) > 80 {
		return chatSession{}, fmt.Errorf("session name must be at most 80 characters")
	}
	now := time.Now()
	item := chatSession{
		ID:        fmt.Sprintf("session-%d", now.UnixNano()),
		Name:      name,
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	s.sessions[item.ID] = item
	if err := s.saveSessionsLocked(); err != nil {
		delete(s.sessions, item.ID)
		return chatSession{}, fmt.Errorf("persist session: %w", err)
	}
	return item, nil
}

func (s *Server) sessionByID(id string) (chatSession, bool) {
	if s.isSessionDeleted(id) {
		return chatSession{}, false
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	item, ok := s.sessions[id]
	return item, ok
}

func (s *Server) touchSession(id string) (chatSession, error) {
	if s.isSessionDeleted(id) {
		s.sessionsMu.Lock()
		delete(s.sessions, id)
		s.sessionsMu.Unlock()
		return chatSession{}, fmt.Errorf("UNKNOWN_SESSION: create a session with session_create before using connector tools")
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	item, ok := s.sessions[id]
	if !ok {
		return chatSession{}, fmt.Errorf("UNKNOWN_SESSION: create a session with session_create before using connector tools")
	}
	previous := item
	item.UpdatedAt = time.Now().Format(time.RFC3339)
	s.sessions[id] = item
	if err := s.saveSessionsLocked(); err != nil {
		s.sessions[id] = previous
		return chatSession{}, fmt.Errorf("persist session activity: %w", err)
	}
	return item, nil
}

func (s *Server) listSessions() []chatSession {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	items := make([]chatSession, 0, len(s.sessions))
	for id, item := range s.sessions {
		if s.isSessionDeleted(id) {
			delete(s.sessions, id)
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	return items
}

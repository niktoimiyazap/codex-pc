package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func (s *Server) inventoryPath() string {
	state := stateDirectory()
	if state == "" {
		return ""
	}
	return filepath.Join(state, "mcp_inventory.json")
}

func (s *Server) loadInventoryCache() {
	path := s.inventoryPath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var payload struct {
		Version   int     `json:"version"`
		UpdatedAt float64 `json:"updated_at"`
		Servers   []any   `json:"servers"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Version != 1 || len(payload.Servers) == 0 {
		return
	}
	s.inventoryMu.Lock()
	s.inventory = payload.Servers
	s.inventoryAt = time.Unix(int64(payload.UpdatedAt), 0)
	s.inventoryMu.Unlock()
}

func (s *Server) saveInventoryCache(servers []any) {
	now := time.Now()
	s.inventoryMu.Lock()
	s.inventory = servers
	s.inventoryAt = now
	s.inventoryMu.Unlock()

	path := s.inventoryPath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	payload := map[string]any{"version": 1, "updated_at": float64(now.Unix()), "servers": servers}
	if data, err := json.Marshal(payload); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
}

func (s *Server) cachedInventory(serverName string) ([]any, time.Time, bool) {
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	if len(s.inventory) == 0 {
		return nil, time.Time{}, false
	}
	for _, item := range s.inventory {
		server, _ := item.(map[string]any)
		if server == nil {
			continue
		}
		if serverName != "" && stringValue(server["name"]) != serverName {
			continue
		}
		tools, ok := server["tools"].([]any)
		if !ok {
			continue
		}

		if stringValue(server["inventory_error"]) != "" && len(tools) == 0 {
			continue
		}
		return s.inventory, s.inventoryAt, true
	}
	return nil, time.Time{}, false
}

func cloneInventoryTools(tools []any) []any {
	cloned := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool == nil {
			cloned = append(cloned, item)
			continue
		}
		copyTool := make(map[string]any, len(tool))
		for key, value := range tool {
			copyTool[key] = value
		}
		cloned = append(cloned, copyTool)
	}
	return cloned
}

func inventoryHasStaleServer(cached []any, serverName string) bool {
	for _, item := range cached {
		server, _ := item.(map[string]any)
		if server == nil {
			continue
		}
		if serverName != "" && stringValue(server["name"]) != serverName {
			continue
		}
		if boolValue(server["inventory_stale"], false) {
			return true
		}
	}
	return false
}

func (s *Server) refreshInventoryInBackground() {
	s.inventoryMu.Lock()
	if s.inventoryRefreshing {
		s.inventoryMu.Unlock()
		return
	}
	s.inventoryRefreshing = true
	s.inventoryMu.Unlock()
	go func() {
		defer func() {
			s.inventoryMu.Lock()
			s.inventoryRefreshing = false
			s.inventoryMu.Unlock()
		}()
		_, _ = s.discover(context.Background(), map[string]any{"refresh": true, "_force_inventory_refresh": true})
	}()
}

func inventoryResponseFromCache(cached []any, q, sn string, limit int64, configPath string, stale bool) map[string]any {
	out := make([]any, 0, len(cached))
	toolsOut := make([]any, 0)
	for _, item := range cached {
		server, _ := item.(map[string]any)
		if server == nil {
			continue
		}
		name := stringValue(server["name"])
		if sn != "" && name != sn {
			continue
		}
		matchedServer := q == "" || strings.Contains(strings.ToLower(name+" "+stringValue(server["command"])+" "+stringValue(server["url"])), q)
		matchedTool := false
		if rawTools, ok := server["tools"].([]any); ok {
			for _, rawTool := range rawTools {
				tool, _ := rawTool.(map[string]any)
				if tool == nil {
					continue
				}
				toolName := stringValue(tool["name"])
				desc := stringValue(tool["description"])
				if q != "" && !strings.Contains(strings.ToLower(toolName+" "+desc+" "+name), q) {
					continue
				}
				matchedTool = true
				copyTool := make(map[string]any, len(tool)+1)
				for k, v := range tool {
					copyTool[k] = v
				}
				copyTool["server"] = name
				toolsOut = append(toolsOut, copyTool)
			}
		}
		if matchedServer || matchedTool {
			copyServer := make(map[string]any, len(server))
			for k, v := range server {
				if k != "tools" {
					copyServer[k] = v
				}
			}
			out = append(out, copyServer)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)
		b, _ := out[j].(map[string]any)
		return stringValue(a["name"]) < stringValue(b["name"])
	})
	sort.Slice(toolsOut, func(i, j int) bool {
		a, _ := toolsOut[i].(map[string]any)
		b, _ := toolsOut[j].(map[string]any)
		return stringValue(a["server"])+"."+stringValue(a["name"]) < stringValue(b["server"])+"."+stringValue(b["name"])
	})
	return map[string]any{
		"servers":     out,
		"apps":        []any{},
		"tools":       toolsOut,
		"query":       q,
		"server_name": nilIfEmpty(sn),
		"limit":       limit,
		"tool_count":  len(toolsOut),
		"truncated":   false,
		"refreshed":   false,
		"stale":       stale,
		"source":      "inventory_cache",
		"config_path": configPath,
	}
}

func (s *Server) discover(ctx context.Context, args map[string]any) (map[string]any, error) {
	servers, configPath, err := readCodexMCPConfig()
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(stringValue(args["query"])))
	sn := strings.TrimSpace(stringValue(args["server_name"]))

	if sn == "" && q != "" {
		for _, server := range servers {
			name := strings.TrimSpace(stringValue(server["name"]))
			if strings.EqualFold(name, q) {
				sn = name
				break
			}
		}
	}
	limit := int64(50)
	if n, ok := numberAsInt(args["limit"]); ok && n > 0 {
		limit = n
	}
	refresh := boolValue(args["refresh"], false) || sn != ""
	forceInventoryRefresh := boolValue(args["_force_inventory_refresh"], false)

	if refresh && !forceInventoryRefresh {
		if cached, cachedAt, ok := s.cachedInventory(sn); ok {
			age := time.Since(cachedAt)
			markedStale := inventoryHasStaleServer(cached, sn)
			stale := age > 5*time.Minute || markedStale

			if age > 5*time.Minute || (markedStale && age > 30*time.Second) {
				s.refreshInventoryInBackground()
			}
			return inventoryResponseFromCache(cached, q, sn, limit, configPath, stale), nil
		}
	}

	if !refresh {
		out := make([]any, 0, len(servers))
		for _, server := range servers {
			name := stringValue(server["name"])
			if sn != "" && name != sn {
				continue
			}
			haystack := strings.ToLower(name + " " + stringValue(server["command"]) + " " + stringValue(server["url"]))
			if q != "" && !strings.Contains(haystack, q) {
				continue
			}
			out = append(out, server)
		}
		sort.Slice(out, func(i, j int) bool {
			a, _ := out[i].(map[string]any)
			b, _ := out[j].(map[string]any)
			return stringValue(a["name"]) < stringValue(b["name"])
		})
		return map[string]any{
			"servers":     out,
			"apps":        []any{},
			"tools":       []any{},
			"query":       stringValue(args["query"]),
			"server_name": nilIfEmpty(sn),
			"limit":       limit,
			"truncated":   false,
			"refreshed":   false,
			"source":      "codex_effective_config",
			"config_path": configPath,
		}, nil
	}

	previousToolsByServer := make(map[string][]any)
	if cached, _, ok := s.cachedInventory(""); ok {
		for _, item := range cached {
			server, _ := item.(map[string]any)
			if server == nil {
				continue
			}
			tools, _ := server["tools"].([]any)
			if len(tools) == 0 {
				continue
			}
			previousToolsByServer[stringValue(server["name"])] = cloneInventoryTools(tools)
		}
	}

	type probeResult struct {
		server map[string]any
		tools  []any
		err    error
	}
	candidates := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		name := stringValue(server["name"])
		if sn != "" && name != sn {
			continue
		}
		candidates = append(candidates, server)
	}
	results := make(chan probeResult, len(candidates))
	var wg sync.WaitGroup
	for _, server := range candidates {
		server := server
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools, err := probeCodexMCPTools(ctx, stringValue(server["name"]))
			results <- probeResult{server: server, tools: tools, err: err}
		}()
	}
	wg.Wait()
	close(results)
	out := make([]any, 0, len(candidates))
	toolsOut := make([]any, 0)
	cacheServers := make([]any, 0, len(candidates))
	for pr := range results {
		server := pr.server
		name := stringValue(server["name"])
		tools := pr.tools
		if pr.err != nil {
			server["inventory_error"] = pr.err.Error()
			server["inventory_stale"] = true
			if len(tools) == 0 {
				if fallback := previousToolsByServer[name]; len(fallback) > 0 {
					tools = cloneInventoryTools(fallback)
					server["inventory_cached_fallback"] = true
				}
			}
		}
		server["toolCount"] = len(tools)
		cachedServer := make(map[string]any, len(server)+1)
		for k, v := range server {
			cachedServer[k] = v
		}
		cachedServer["tools"] = tools
		cacheServers = append(cacheServers, cachedServer)
		matchedServer := q == "" || strings.Contains(strings.ToLower(name+" "+stringValue(server["command"])+" "+stringValue(server["url"])), q)
		matchedTool := false
		for _, item := range tools {
			tm, _ := item.(map[string]any)
			if tm == nil {
				continue
			}
			toolName := stringValue(tm["name"])
			desc := stringValue(tm["description"])
			if q != "" && !strings.Contains(strings.ToLower(toolName+" "+desc+" "+name), q) {
				continue
			}
			matchedTool = true
			tm["server"] = name
			toolsOut = append(toolsOut, tm)
		}
		if matchedServer || matchedTool {
			out = append(out, server)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i].(map[string]any)
		b, _ := out[j].(map[string]any)
		return stringValue(a["name"]) < stringValue(b["name"])
	})
	sort.Slice(toolsOut, func(i, j int) bool {
		a, _ := toolsOut[i].(map[string]any)
		b, _ := toolsOut[j].(map[string]any)
		return stringValue(a["server"])+"."+stringValue(a["name"]) < stringValue(b["server"])+"."+stringValue(b["name"])
	})
	allToolCount := len(toolsOut)
	if sn == "" {
		s.saveInventoryCache(cacheServers)
	}
	apps := []any{}
	var installed map[string]any
	if err := s.app.Request(ctx, "app/installed", map[string]any{"forceRefresh": false}, &installed); err == nil {
		if raw, ok := installed["apps"].([]any); ok {
			for _, item := range raw {
				m, _ := item.(map[string]any)
				if m == nil {
					continue
				}
				apps = append(apps, map[string]any{
					"id": m["id"], "name": m["runtimeName"],
					"enabled": m["enabled"], "callable": m["callable"],
					"source": "codex_apps",
				})
			}
		}
	}
	return map[string]any{
		"servers":     out,
		"apps":        apps,
		"tools":       toolsOut,
		"query":       stringValue(args["query"]),
		"server_name": nilIfEmpty(sn),
		"limit":       limit,
		"tool_count":  allToolCount,
		"truncated":   false,
		"refreshed":   true,
		"source":      "codex_effective_config+probed_tools+app_installed",
		"config_path": configPath,
	}, nil
}

func (s *Server) invalidateInventory() {
	s.inventoryMu.Lock()
	s.inventory = nil
	s.inventoryAt = time.Time{}
	s.inventoryMu.Unlock()
}

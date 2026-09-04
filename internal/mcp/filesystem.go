package mcp

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Server) resolvePath(v any) (string, error) {
	raw, ok := v.(string)
	if !ok || raw == "" {
		return "", fmt.Errorf("path required")
	}
	if strings.HasPrefix(raw, "~") {
		home, _ := os.UserHomeDir()
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~\\/"))
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(s.workspace, raw)
	}
	p, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	p = filepath.Clean(p)
	for _, r := range s.allowedRoots {
		rel, e := filepath.Rel(r, p)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return p, nil
		}
	}
	return "", fmt.Errorf("path outside allowed roots: %s", p)
}

func (s *Server) fsSearch(ctx context.Context, args map[string]any) (map[string]any, error) {
	root, err := s.resolvePath(args["path"])
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fs_search path must be a directory")
	}
	query := stringValue(args["query"])
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	mode := strings.ToLower(stringValue(args["mode"]))
	if mode == "" {
		mode = "name"
	}
	if mode != "name" && mode != "content" && mode != "both" {
		return nil, fmt.Errorf("mode must be name, content, or both")
	}
	caseSensitive := boolValue(args["case_sensitive"], false)
	useRegex := boolValue(args["regex"], false)
	includeHidden := boolValue(args["include_hidden"], false)
	glob := stringValue(args["glob"])
	limit := int64(100)
	if n, ok := numberAsInt(args["max_results"]); ok {
		limit = n
	}
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("max_results must be between 1 and 500")
	}

	var re *regexp.Regexp
	if useRegex {
		pattern := query
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
	}
	needle := query
	if !caseSensitive && !useRegex {
		needle = strings.ToLower(needle)
	}
	matches := func(value string) bool {
		if useRegex {
			return re.MatchString(value)
		}
		if !caseSensitive {
			value = strings.ToLower(value)
		}
		return strings.Contains(value, needle)
	}
	globMatches := func(name string) bool {
		if glob == "" {
			return true
		}
		ok, matchErr := filepath.Match(glob, name)
		return matchErr == nil && ok
	}

	results := make([]any, 0, min(int(limit), 100))
	visitedFiles, visitedDirs, skippedLarge, skippedBinary := 0, 0, 0, 0
	truncated := false
	stopReason := ""
	searchStarted := time.Now()
	const searchBudget = 10 * time.Second
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, ".next": true, "__pycache__": true, ".venv": true, "venv": true,
		"env": true, ".tox": true, ".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
		"appdata": true, ".cache": true, "cache": true, "caches": true, "temp": true, "tmp": true,
		"vendor": true, "dist": true, "build": true, "target": true, ".gradle": true, ".idea": true,
	}
	textExt := map[string]bool{
		"": true, ".txt": true, ".md": true, ".go": true, ".py": true, ".js": true, ".jsx": true,
		".ts": true, ".tsx": true, ".json": true, ".jsonl": true, ".yaml": true, ".yml": true, ".toml": true,
		".ini": true, ".cfg": true, ".conf": true, ".xml": true, ".html": true, ".htm": true, ".css": true,
		".scss": true, ".less": true, ".sql": true, ".sh": true, ".bash": true, ".ps1": true, ".cmd": true,
		".bat": true, ".rs": true, ".java": true, ".kt": true, ".kts": true, ".c": true, ".h": true, ".cpp": true,
		".hpp": true, ".cs": true, ".php": true, ".rb": true, ".swift": true, ".vue": true, ".svelte": true,
		".env": true, ".gitignore": true, ".dockerignore": true, ".csv": true, ".tsv": true, ".log": true,
	}
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if time.Since(searchStarted) >= searchBudget {
			truncated = true
			stopReason = "time_budget"
			return io.EOF
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if path == root {
			visitedDirs++
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			visitedDirs++
			if skipDirs[strings.ToLower(name)] || (!includeHidden && strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if (mode == "name" || mode == "both") && globMatches(name) && matches(name) {
				results = append(results, map[string]any{"kind": "directory", "path": path, "name": name})
				if int64(len(results)) >= limit {
					truncated = true
					return io.EOF
				}
			}
			return nil
		}
		if !includeHidden && strings.HasPrefix(name, ".") {
			return nil
		}
		visitedFiles++
		if !globMatches(name) {
			return nil
		}
		if (mode == "name" || mode == "both") && matches(name) {
			results = append(results, map[string]any{"kind": "file", "path": path, "name": name})
			if int64(len(results)) >= limit {
				truncated = true
				return io.EOF
			}
		}
		if mode == "name" {
			return nil
		}

		if glob == "" && !textExt[strings.ToLower(filepath.Ext(name))] {
			skippedBinary++
			return nil
		}
		stat, statErr := entry.Info()
		if statErr != nil || stat.Size() > 2*1024*1024 {
			if statErr == nil && stat.Size() > 2*1024*1024 {
				skippedLarge++
			}
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			skippedBinary++
			return nil
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			if !matches(line) {
				continue
			}
			snippet := strings.TrimSpace(line)
			if len(snippet) > 320 {
				snippet = snippet[:320] + "…"
			}
			results = append(results, map[string]any{"kind": "content", "path": path, "name": name, "line": i + 1, "text": snippet})
			if int64(len(results)) >= limit {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, io.EOF) {
		return nil, walkErr
	}
	return map[string]any{
		"path": root, "query": query, "mode": mode, "glob": glob,
		"case_sensitive": caseSensitive, "regex": useRegex,
		"count": len(results), "results": results, "truncated": truncated, "stop_reason": stopReason,
		"visited_files": visitedFiles, "visited_directories": visitedDirs,
		"skipped_large_files": skippedLarge, "skipped_binary_files": skippedBinary,
	}, nil
}

func (s *Server) readRules(args map[string]any) (map[string]any, error) {
	target := strings.TrimSpace(stringValue(args["path"]))
	if target == "" {
		target = s.workspace
	} else {
		resolved, err := s.resolvePath(target)
		if err != nil {
			return nil, err
		}
		target = resolved
	}
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		target = filepath.Dir(target)
	}
	target, _ = filepath.Abs(target)

	home, _ := os.UserHomeDir()
	candidates := make([]string, 0, 16)
	if home != "" {
		candidates = append(candidates, filepath.Join(home, "Desktop", "AGENTS.md"))
	}

	dirs := make([]string, 0, 12)
	for dir := target; dir != ""; {
		dirs = append(dirs, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		candidates = append(candidates, filepath.Join(dir, "AGENTS.md"), filepath.Join(dir, ".agents", "AGENTS.md"))
	}

	seen := make(map[string]bool)
	rules := make([]map[string]any, 0, 8)
	combined := strings.Builder{}
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		key := strings.ToLower(filepath.Clean(abs))
		if seen[key] {
			continue
		}
		seen[key] = true
		data, err := os.ReadFile(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read rules %s: %w", abs, err)
		}
		content := string(data)
		rules = append(rules, map[string]any{"path": abs, "content": content, "size_bytes": len(data)})
		if combined.Len() > 0 {
			combined.WriteString("\n\n")
		}
		combined.WriteString("# Rules from ")
		combined.WriteString(abs)
		combined.WriteString("\n\n")
		combined.WriteString(content)
	}

	return map[string]any{
		"target":      target,
		"count":       len(rules),
		"rules":       rules,
		"combined":    combined.String(),
		"instruction": "Apply these rules to the current project work. More specific project rules supplement or override broader rules where they conflict.",
	}, nil
}

func (s *Server) editFile(args map[string]any) (map[string]any, error) {
	p, e := s.resolvePath(args["path"])
	if e != nil {
		return nil, e
	}
	data, e := os.ReadFile(p)
	if e != nil {
		return nil, e
	}
	oldHash := hex.EncodeToString(sum(data))
	expectedHash := strings.TrimSpace(stringValue(args["expected_sha256"]))
	baseStale := expectedHash != "" && !strings.EqualFold(oldHash, expectedHash)
	text := string(data)
	edits, ok := args["edits"].([]any)
	if !ok {
		return nil, fmt.Errorf("edits required")
	}
	count := 0
	var diff strings.Builder
	for _, v := range edits {
		m, _ := v.(map[string]any)
		old := stringValue(m["old_text"])
		nw := stringValue(m["new_text"])
		want := int64(1)
		if n, ok := numberAsInt(m["expected_count"]); ok {
			want = n
		}

		matchOld := old
		matchNew := nw
		got := strings.Count(text, matchOld)
		if got == 0 && strings.Contains(old, "\n") {
			if strings.Contains(text, "\r\n") && !strings.Contains(old, "\r\n") {
				candidateOld := strings.ReplaceAll(old, "\n", "\r\n")
				candidateGot := strings.Count(text, candidateOld)
				if candidateGot > 0 {
					matchOld = candidateOld
					matchNew = strings.ReplaceAll(strings.ReplaceAll(nw, "\r\n", "\n"), "\n", "\r\n")
					got = candidateGot
				}
			} else if !strings.Contains(text, "\r\n") && strings.Contains(old, "\r\n") {
				candidateOld := strings.ReplaceAll(old, "\r\n", "\n")
				candidateGot := strings.Count(text, candidateOld)
				if candidateGot > 0 {
					matchOld = candidateOld
					matchNew = strings.ReplaceAll(nw, "\r\n", "\n")
					got = candidateGot
				}
			}
		}

		replaceAll := boolValue(m["replace_all"], false)

		if baseStale && replaceAll {
			return nil, fmt.Errorf("STALE_FILE: hash mismatch; replace_all requires a fresh read")
		}

		if got == 0 && want == 1 && !replaceAll {
			if actual, matches := whitespaceEquivalentMatch(text, old); matches == 1 {
				matchOld = actual
				got = 1
				if strings.Contains(actual, "\r\n") {
					matchNew = strings.ReplaceAll(strings.ReplaceAll(nw, "\r\n", "\n"), "\n", "\r\n")
				} else {
					matchNew = strings.ReplaceAll(nw, "\r\n", "\n")
				}
			}
		}
		if got != int(want) && !replaceAll {
			return nil, fmt.Errorf("expected %d matches, got %d (file may have changed since read, or the requested fragment is no longer unique)", want, got)
		}
		diffCount := int(want)
		if replaceAll {
			diffCount = got
		}
		appendEditDiff(&diff, text, matchOld, matchNew, diffCount)
		if replaceAll {
			text = strings.ReplaceAll(text, matchOld, matchNew)
			count += got
		} else {
			text = strings.Replace(text, matchOld, matchNew, int(want))
			count += int(want)
		}
	}
	newData := []byte(text)
	dry := boolValue(args["dry_run"], false)
	if !dry && !bytes.Equal(data, newData) {
		if e = os.WriteFile(p, newData, 0o666); e != nil {
			return nil, e
		}
	}
	return map[string]any{"path": p, "changed": !bytes.Equal(data, newData), "dry_run": dry, "replacements": count, "base_stale": baseStale, "expected_sha256": expectedHash, "old_sha256": oldHash, "new_sha256": hex.EncodeToString(sum(newData)), "encoding": "utf-8", "diff": strings.TrimSuffix(diff.String(), "\n")}, nil
}

func whitespaceEquivalentMatch(text, pattern string) (string, int) {
	if pattern == "" {
		return "", 0
	}
	var expr strings.Builder
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '\r':
			if i+1 < len(pattern) && pattern[i+1] == '\n' {
				expr.WriteString(`\r?\n`)
				i += 2
				continue
			}
			expr.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			i++
		case '\n':
			expr.WriteString(`\r?\n`)
			i++
		case ' ', '\t':
			for i < len(pattern) && (pattern[i] == ' ' || pattern[i] == '\t') {
				i++
			}
			expr.WriteString(`[ \t]+`)
		default:
			start := i
			for i < len(pattern) && pattern[i] != '\r' && pattern[i] != '\n' && pattern[i] != ' ' && pattern[i] != '\t' {
				i++
			}
			expr.WriteString(regexp.QuoteMeta(pattern[start:i]))
		}
	}
	re, err := regexp.Compile(expr.String())
	if err != nil {
		return "", 0
	}
	matches := re.FindAllStringIndex(text, 2)
	if len(matches) != 1 {
		return "", len(matches)
	}
	return text[matches[0][0]:matches[0][1]], 1
}

func appendEditDiff(out *strings.Builder, text, old, nw string, count int) {
	if count <= 0 || old == "" {
		return
	}
	from := 0
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(nw, "\n")
	for i := 0; i < count; i++ {
		rel := strings.Index(text[from:], old)
		if rel < 0 {
			break
		}
		pos := from + rel
		line := 1 + strings.Count(text[:pos], "\n")
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "@@ -%d,%d +%d,%d @@\n", line, len(oldLines), line, len(newLines))
		for _, value := range oldLines {
			out.WriteByte('-')
			out.WriteString(value)
			out.WriteByte('\n')
		}
		for _, value := range newLines {
			out.WriteByte('+')
			out.WriteString(value)
			out.WriteByte('\n')
		}
		from = pos + len(old)
	}
}

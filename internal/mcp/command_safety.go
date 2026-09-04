package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var shellWriteTargetRE = regexp.MustCompile(`(?i)(?:^|[\s;&|])(?:1\s*)?>>?\s*["']?([^\s;&|"']+)["']?`)

var psWriteTargetRE = regexp.MustCompile(`(?i)(?:set-content|add-content|out-file).*?(?:-literalpath|-path)\s+["']([^"']+)["']`)

var pyWriteTargetRE = regexp.MustCompile(`(?i)(?:open\(|write_text\()["']([^"']+)["']`)

var jsWriteTargetRE = regexp.MustCompile(`(?i)(?:writefile(?:sync)?|appendfile(?:sync)?|bun\.write)\s*\(\s*["']([^"']+)["']`)

var teeWriteTargetRE = regexp.MustCompile(`(?i)(?:^|[\s|;&])tee(?:\.exe)?\s+["']?([^\s;&|"']+)`)

type fileSnapshot struct {
	path    string
	existed bool
	data    []byte
}

func isTextPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".txt", ".md", ".json", ".jsonl", ".js", ".jsx", ".ts", ".tsx", ".html", ".htm", ".css", ".scss", ".xml", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".env", ".py", ".pyw", ".go", ".rs", ".java", ".cs", ".c", ".cc", ".cpp", ".h", ".hpp", ".ps1", ".cmd", ".bat", ".sh", ".sql", ".csv":
		return true
	}
	return false
}

func detectTextWriteTargets(cmd []string, cwd string) []string {
	joined := strings.Join(cmd, " ")
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(strings.Trim(raw, `"'`))
		low := strings.ToLower(raw)
		if raw == "" || low == "nul" || low == "$null" || low == "/dev/null" {
			return
		}
		if !filepath.IsAbs(raw) {
			raw = filepath.Join(cwd, raw)
		}
		raw = filepath.Clean(raw)
		if !isTextPath(raw) || seen[strings.ToLower(raw)] {
			return
		}
		seen[strings.ToLower(raw)] = true
		out = append(out, raw)
	}
	for _, re := range []*regexp.Regexp{shellWriteTargetRE, psWriteTargetRE, pyWriteTargetRE, jsWriteTargetRE, teeWriteTargetRE} {
		for _, m := range re.FindAllStringSubmatch(joined, -1) {
			if len(m) > 1 {
				add(m[1])
			}
		}
	}
	return out
}

func snapshotTargets(paths []string) []fileSnapshot {
	out := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		out = append(out, fileSnapshot{path: path, existed: err == nil, data: data})
	}
	return out
}

func looksMojibake(text string) bool {
	markers := []string{"Р°", "Рµ", "Рё", "Рѕ", "РЅ", "Рї", "СЂ", "С‚", "СЃ", "Р»", "Рє", "Рґ", "РІ", "Рј", "С‹", "СЏ", "Р¶", "С‡", "С€"}
	count := 0
	for _, marker := range markers {
		count += strings.Count(text, marker)
	}
	return count >= 4
}

func validateAndRestoreTextTargets(snaps []fileSnapshot) error {
	for _, snap := range snaps {
		data, err := os.ReadFile(snap.path)
		if err != nil {
			continue
		}
		if utf8.Valid(data) && !looksMojibake(string(data)) {
			continue
		}
		if snap.existed {
			_ = os.WriteFile(snap.path, snap.data, 0o644)
		} else {
			_ = os.Remove(snap.path)
		}
		return fmt.Errorf("encoding safety check failed for %s: terminal write produced non-UTF-8 or mojibake text; original file was restored", snap.path)
	}
	return nil
}

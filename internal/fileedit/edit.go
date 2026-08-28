package fileedit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Edit struct {
	OldText       string
	NewText       string
	ExpectedCount int
	ReplaceAll    bool
}

type Result struct {
	Changed      bool
	Replacements int
	OldSHA256    string
	NewSHA256    string
	Newline      string
	FinalNewline bool
}

func Apply(path, expectedSHA string, edits []Edit, dryRun bool) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	oldSHA := sum(data)
	if !strings.EqualFold(oldSHA, expectedSHA) {
		return Result{}, fmt.Errorf("STALE_FILE: file hash does not match expected_sha256")
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return Result{}, fmt.Errorf("BINARY_FILE: file contains NUL bytes and is not safe for text editing")
	}
	before := string(data)
	after := before
	replacements := 0
	for i, edit := range edits {
		if edit.OldText == "" {
			return Result{}, fmt.Errorf("INVALID_EDIT: edit %d has empty old_text", i)
		}
		count := strings.Count(after, edit.OldText)
		if count == 0 {
			return Result{}, fmt.Errorf("MATCH_NOT_FOUND: edit %d did not match", i)
		}
		if edit.ReplaceAll {
			after = strings.ReplaceAll(after, edit.OldText, edit.NewText)
			replacements += count
			continue
		}
		expected := edit.ExpectedCount
		if expected <= 0 {
			expected = 1
		}
		if count != expected {
			return Result{}, fmt.Errorf("AMBIGUOUS_MATCH: edit %d expected %d match(es), found %d", i, expected, count)
		}
		after = strings.Replace(after, edit.OldText, edit.NewText, expected)
		replacements += expected
	}
	newData := []byte(after)
	changed := string(data) != after
	newSHA := sum(newData)
	if changed && !dryRun {
		current, err := os.ReadFile(path)
		if err != nil {
			return Result{}, err
		}
		if sum(current) != oldSHA {
			return Result{}, fmt.Errorf("STALE_FILE: file changed after it was read")
		}
		if err := atomicWrite(path, newData); err != nil {
			return Result{}, err
		}
	}
	return Result{
		Changed:      changed,
		Replacements: replacements,
		OldSHA256:    oldSHA,
		NewSHA256:    newSHA,
		Newline:      detectNewline(before),
		FinalNewline: strings.HasSuffix(before, "\n") || strings.HasSuffix(before, "\r"),
	}, nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func detectNewline(text string) string {
	crlf := strings.Count(text, "\r\n")
	lf := strings.Count(text, "\n") - crlf
	cr := strings.Count(text, "\r") - crlf
	switch {
	case crlf >= lf && crlf >= cr && crlf > 0:
		return "crlf"
	case lf >= cr && lf > 0:
		return "lf"
	case cr > 0:
		return "cr"
	default:
		return "none"
	}
}

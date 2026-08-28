package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathPolicyAllowsHomeAndBlocksOutside(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewPathPolicy()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(home, "codexpc-go-test", "file.txt")
	if _, err := policy.Resolve(inside); err != nil {
		t.Fatalf("expected home path to be allowed: %v", err)
	}
	if os.PathSeparator == '\\' {
		if _, err := policy.Resolve(`C:\Windows\codexpc-go-test.txt`); err == nil {
			t.Fatal("expected Windows system path to be outside default allowed roots")
		}
	}
}

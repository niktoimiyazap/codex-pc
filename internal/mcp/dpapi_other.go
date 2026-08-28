//go:build !windows

package mcp

import "fmt"

func unprotectSecret(string) (string, error) {
	return "", fmt.Errorf("CodexPC Secret Vault DPAPI is only available on Windows")
}

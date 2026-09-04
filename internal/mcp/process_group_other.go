//go:build !windows

package mcp

func attachProcessGroup(_ int, _ uint32) (uintptr, error) { return 0, nil }
func processGroupProcessCount(_ uintptr) (uint32, error)  { return 0, nil }
func terminateProcessGroup(_ uintptr) error               { return nil }
func closeProcessGroup(_ uintptr)                         {}

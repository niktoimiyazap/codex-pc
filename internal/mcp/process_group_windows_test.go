//go:build windows

package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessGroupTracksAndTerminatesTree(t *testing.T) {
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	script := `$ps = $env:SystemRoot + '\System32\WindowsPowerShell\v1.0\powershell.exe'; Start-Sleep -Milliseconds 300; Start-Process $ps -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 30'; Start-Sleep -Seconds 30`
	cmd := exec.Command(powershell, "-NoProfile", "-Command", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	group, err := attachProcessGroup(cmd.Process.Pid, 8)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("attach process group: %v", err)
	}
	defer closeProcessGroup(group)

	deadline := time.Now().Add(3 * time.Second)
	var active uint32
	for time.Now().Before(deadline) {
		active, err = processGroupProcessCount(group)
		if err != nil {
			t.Fatal(err)
		}
		if active >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if active < 2 {
		t.Fatalf("expected parent and child in job, got %d active processes", active)
	}
	if err := terminateProcessGroup(group); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process tree did not exit after job termination")
	}
}

package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func normalizeWindowsCommand(cmd []string) []string {
	if len(cmd) == 0 {
		return cmd
	}

	resolved := resolveWindowsCommand(cmd[0])
	if resolved == "" {
		return cmd
	}
	resolved, _ = filepath.Abs(resolved)

	ext := strings.ToLower(filepath.Ext(resolved))
	if ext != ".cmd" && ext != ".bat" {
		out := append([]string(nil), cmd...)
		out[0] = resolved
		return normalizePowerShellCommand(out)
	}

	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	out := make([]string, 0, len(cmd)+3)
	out = append(out, comspec, "/d", "/c", resolved)
	out = append(out, cmd[1:]...)
	return out
}

func normalizePowerShellCommand(cmd []string) []string {
	if len(cmd) < 3 {
		return cmd
	}
	base := strings.ToLower(filepath.Base(cmd[0]))
	if base != "powershell" && base != "powershell.exe" && base != "pwsh" && base != "pwsh.exe" {
		return cmd
	}
	for i := 1; i+1 < len(cmd); i++ {
		flag := strings.ToLower(cmd[i])
		if flag != "-command" && flag != "-c" {
			continue
		}

		if i+2 != len(cmd) {
			return cmd
		}
		script := cmd[i+1]
		wrapped := `$global:LASTEXITCODE = 0; $ErrorActionPreference = 'Stop'; try { & { ` + script + ` }; $__codexOk = $?; $__codexExit = $global:LASTEXITCODE; if ($__codexExit -ne 0) { exit $__codexExit }; if (-not $__codexOk) { exit 1 } } catch { [Console]::Error.WriteLine($_.ToString()); exit 1 }`
		out := append([]string(nil), cmd...)
		out[i+1] = wrapped
		return out
	}
	return cmd
}

func resolveWindowsCommand(name string) string {
	if name == "" {
		return ""
	}

	if filepath.Ext(name) == "" {
		pathext := os.Getenv("PATHEXT")
		if strings.TrimSpace(pathext) == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, ext := range strings.Split(pathext, ";") {
			ext = strings.TrimSpace(ext)
			if ext == "" {
				continue
			}
			candidate := name + ext
			if resolved, err := exec.LookPath(candidate); err == nil && resolved != "" {
				return resolved
			}
		}
	}

	if resolved, err := exec.LookPath(name); err == nil && resolved != "" {
		return resolved
	}
	return ""
}

func localSearchShouldUseFS(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	joined := " " + strings.ToLower(strings.Join(argv, " ")) + " "

	first := strings.ToLower(strings.TrimSpace(argv[0]))
	if first == "ssh" || strings.HasSuffix(first, "\\ssh.exe") || strings.HasSuffix(first, "/ssh") {
		return false
	}
	markers := []string{
		" get-childitem ", " gci ", " select-string ",
		" rg ", " ripgrep ", " grep -r", " grep --recursive", " findstr /s", " dir /s",
		"os.walk(", ".rglob(", "rglob(", "glob('**", "glob(\"**",
	}
	for _, marker := range markers {
		if strings.Contains(joined, marker) {
			return true
		}
	}
	return false
}

func killWindowsProcessTree(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	killer := exec.Command("taskkill.exe", "/PID", fmt.Sprintf("%d", command.Process.Pid), "/T", "/F")
	if err := killer.Run(); err != nil {
		_ = command.Process.Kill()
	}
}

func commandEnvironment(args map[string]any) map[string]any {
	env := map[string]any{
		"PYTHONUTF8":       "1",
		"PYTHONIOENCODING": "utf-8",
		"LANG":             "C.UTF-8",
		"LC_ALL":           "C.UTF-8",
	}
	if supplied, ok := args["env"].(map[string]any); ok {
		for key, value := range supplied {
			env[key] = value
		}
	}
	return env
}

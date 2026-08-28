package security

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type PathPolicy struct {
	Workspace   string
	AllowedRoot []string
	DeniedWrite []string
}

func NewPathPolicyFrom(workspace string, roots []string) (*PathPolicy, error) {
	if workspace == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		workspace = home
	}
	workspace, err := normalize(workspace)
	if err != nil {
		return nil, err
	}
	allowed := make([]string, 0, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		n, err := normalize(os.ExpandEnv(root))
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, n)
	}
	if len(allowed) == 0 {
		allowed = []string{workspace}
	}
	denied := []string{}
	if runtime.GOOS == "windows" {
		for _, candidate := range []string{os.Getenv("WINDIR"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData")} {
			if candidate == "" {
				continue
			}
			if n, err := normalize(candidate); err == nil {
				denied = append(denied, n)
			}
		}
	} else {
		for _, candidate := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/proc", "/root", "/sbin", "/sys", "/usr"} {
			if n, err := normalize(candidate); err == nil {
				denied = append(denied, n)
			}
		}
	}
	return &PathPolicy{Workspace: workspace, AllowedRoot: allowed, DeniedWrite: denied}, nil
}

func NewPathPolicy() (*PathPolicy, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	workspace := os.Getenv("CODEXPC_WORKSPACE")
	if workspace == "" {
		workspace = home
	}
	workspace, err = normalize(workspace)
	if err != nil {
		return nil, err
	}

	roots := []string{home}
	if raw := os.Getenv("CODEXPC_ALLOWED_ROOTS"); raw != "" {
		roots = filepath.SplitList(raw)
	}
	allowed := make([]string, 0, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		n, err := normalize(os.ExpandEnv(root))
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, n)
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("no allowed roots configured")
	}

	denied := []string{}
	if runtime.GOOS == "windows" {
		for _, candidate := range []string{
			os.Getenv("WINDIR"),
			os.Getenv("ProgramFiles"),
			os.Getenv("ProgramFiles(x86)"),
			os.Getenv("ProgramData"),
		} {
			if candidate == "" {
				continue
			}
			if n, err := normalize(candidate); err == nil {
				denied = append(denied, n)
			}
		}
	} else {
		for _, candidate := range []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/proc", "/root", "/sbin", "/sys", "/usr"} {
			if n, err := normalize(candidate); err == nil {
				denied = append(denied, n)
			}
		}
	}
	return &PathPolicy{Workspace: workspace, AllowedRoot: allowed, DeniedWrite: denied}, nil
}

func (p *PathPolicy) Resolve(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	raw = os.ExpandEnv(raw)
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(p.Workspace, raw)
	}
	resolved, err := normalize(raw)
	if err != nil {
		return "", err
	}
	for _, root := range p.AllowedRoot {
		if within(resolved, root) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path is outside allowed roots: %s", resolved)
}

func (p *PathPolicy) EnsureWritable(path string) error {
	for _, root := range p.DeniedWrite {
		if within(path, root) {
			return fmt.Errorf("writing to protected system path is blocked: %s", path)
		}
	}
	return nil
}

func normalize(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err == nil {
		if rel == "." {
			return true
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
			return true
		}
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	cursor := path
	for {
		if info, statErr := os.Stat(cursor); statErr == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
		cursor = parent
	}
	return false
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/niktoimiyazap/codexpc-connector/internal/appserver"
	"github.com/niktoimiyazap/codexpc-connector/internal/config"
	logpkg "github.com/niktoimiyazap/codexpc-connector/internal/logging"
	"github.com/niktoimiyazap/codexpc-connector/internal/mcp"
)

const version = "0.4.0-dev"

func main() {
	codexPath := flag.String("codex", "codex", "path to codex executable")
	smoke := flag.Bool("smoke", false, "initialize app-server and run a command smoke test")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	settings, err := config.Load()
	if err != nil {
		fatal(err)
	}
	_ = os.Setenv("CODEXPC_WORKSPACE", settings.Workspace)
	_ = os.Setenv("CODEXPC_ALLOWED_ROOTS", strings.Join(settings.AllowedRoots, string(os.PathListSeparator)))
	if err := os.Chdir(settings.Workspace); err != nil {
		fatal(err)
	}
	logger, err := logpkg.New(settings.StateDir)
	if err != nil {
		fatal(err)
	}
	logger.Event("INFO", "connector_start", map[string]any{"backend": "codex_app_server", "implementation": "go", "pid": os.Getpid(), "workspace": settings.Workspace})
	logger.Event("INFO", "app_server_starting", map[string]any{"codex": *codexPath})
	defer logger.Event("INFO", "connector_stop", map[string]any{"implementation": "go"})

	streams := appserver.NewStreamRegistry()
	client, err := appserver.Start(ctx, *codexPath, streams.Handle)
	if err != nil {
		fatal(err)
	}
	defer client.Close()
	logger.Event("INFO", "app_server_process_started", nil)

	startupTimeout := time.Duration(settings.DefaultStartupTimeoutSec * float64(time.Second))
	if startupTimeout <= 0 {
		startupTimeout = 45 * time.Second
	}
	server := mcp.NewServer(client, streams, logger, os.Stdin, os.Stdout)

	if *smoke {
		initCtx, cancel := context.WithTimeout(ctx, startupTimeout)
		err = client.Initialize(initCtx, version)
		cancel()
		server.MarkBackendReady(err)
		if err != nil {
			fatal(err)
		}
		logger.Event("INFO", "app_server_initialized", map[string]any{"version": version})
		if err := smokeCommand(ctx, client, streams); err != nil {
			fatal(err)
		}
		return
	}

	// Start serving MCP immediately. The external client can complete initialize,
	// tools/list, session_create and native Windows commands while the internal
	// Codex app-server finishes booting. Backend-dependent calls wait in their own
	// request goroutine instead of being lost behind startup.
	logger.Event("INFO", "mcp_stdio_ready", map[string]any{"transport": "stdio", "backend_state": "starting"})
	go func() {
		initCtx, cancel := context.WithTimeout(ctx, startupTimeout)
		initErr := client.Initialize(initCtx, version)
		cancel()
		server.MarkBackendReady(initErr)
		if initErr != nil {
			logger.Event("ERROR", "app_server_initialize_failed", map[string]any{"error": initErr.Error()})
			return
		}
		logger.Event("INFO", "app_server_initialized", map[string]any{"version": version})
	}()

	go watchRestartRequest(ctx, settings.StateDir, logger, stop)

	serveErr := server.Serve(ctx)
	stopReason := "stdin_eof"
	if ctx.Err() != nil {
		stopReason = "context_canceled"
	} else if serveErr != nil {
		stopReason = "serve_error"
	}
	serveErrText := ""
	if serveErr != nil {
		serveErrText = serveErr.Error()
	}
	ctxErrText := ""
	if ctx.Err() != nil {
		ctxErrText = ctx.Err().Error()
	}
	logger.Event("WARN", "connector_serve_stopped", map[string]any{
		"reason":    stopReason,
		"serve_err": serveErrText,
		"ctx_err":   ctxErrText,
	})
	if serveErr != nil && serveErr != context.Canceled {
		fatal(serveErr)
	}
}

func smokeCommand(ctx context.Context, client *appserver.Client, streams *appserver.StreamRegistry) error {
	processID := fmt.Sprintf("go-smoke-%d", time.Now().UnixNano())
	unregister := streams.Register(processID, func(stream string, data []byte, _ bool) {
		if stream == "stderr" {
			_, _ = os.Stderr.Write(data)
			return
		}
		_, _ = os.Stdout.Write(data)
	})
	defer unregister()

	params := map[string]any{
		"command":   []string{"powershell", "-NoProfile", "-Command", "1..3 | ForEach-Object { Write-Output (\"go-step $_\"); Start-Sleep -Milliseconds 300 }"},
		"processId": processID,
		"timeoutMs": 10000,
	}
	if runtime.GOOS != "windows" {
		params["streamStdoutStderr"] = true
		params["outputBytesCap"] = 65536
	}
	var result map[string]any
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := client.Request(reqCtx, "command/exec", params, &result); err != nil {
		return err
	}
	b, _ := json.Marshal(result)
	fmt.Fprintf(os.Stderr, "\nresult=%s\n", b)
	return nil
}

func watchRestartRequest(ctx context.Context, stateDir string, logger *logpkg.Logger, requestStop context.CancelFunc) {
	marker := filepath.Join(stateDir, "restart.request")
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(marker); err != nil {
				continue
			}
			_ = os.Remove(marker)
			logger.Event("INFO", "connector_restart_requested", map[string]any{"source": "monitor_ui"})
			requestStop()
			return
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "codexpc-go:", err)
	os.Exit(1)
}

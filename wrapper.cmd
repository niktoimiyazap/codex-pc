@echo off
setlocal
cd /d "%~dp0"

rem Startup must be fast and deterministic. Building is explicit via build-go.cmd.
if not exist "dist\codexpc-go.exe" (
  echo CodexPC Go binary is missing: %~dp0dist\codexpc-go.exe 1>&2
  echo Run build-go.cmd once to create it. 1>&2
  exit /b 2
)

"dist\codexpc-go.exe" %*
exit /b %errorlevel%

@echo off
setlocal
set "ROOT=%~dp0.."
cd /d "%ROOT%"

if not exist "%ROOT%\dist\codexpc-go.exe" (
  echo CodexPC binary is missing: %ROOT%\dist\codexpc-go.exe 1>&2
  echo Run scripts\build.cmd once to create it. 1>&2
  exit /b 2
)

"%ROOT%\dist\codexpc-go.exe" %*
exit /b %errorlevel%

@echo off
setlocal
set PYTHONIOENCODING=utf-8
cd /d "%~dp0"
if exist activity.log del /f /q activity.log >nul 2>&1
python -m codexpc_connector %*

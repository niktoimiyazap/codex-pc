param()
$ErrorActionPreference = 'Continue'
$Repo = Split-Path -Parent $PSScriptRoot
$Monitor = Join-Path $Repo 'monitor_ui\CodexPC Monitor.pyw'
$FrontUrl = 'http://127.0.0.1:8765/'
$StateDir = Join-Path $env:LOCALAPPDATA 'CodexPCConnector'
$WrapperPidFile = Join-Path $StateDir 'wrapper.pid'
New-Item -ItemType Directory -Path $StateDir -Force | Out-Null
if (Test-Path -LiteralPath $WrapperPidFile) {
  $oldWrapperPid = 0
  [void][int]::TryParse((Get-Content -LiteralPath $WrapperPidFile -Raw -ErrorAction SilentlyContinue).Trim(), [ref]$oldWrapperPid)
  if ($oldWrapperPid -gt 0 -and $oldWrapperPid -ne $PID -and (Get-Process -Id $oldWrapperPid -ErrorAction SilentlyContinue)) {
    Write-Host "[INFO] Replacing CodexPC wrapper PID=$oldWrapperPid..." -ForegroundColor Yellow
    # Kill only the old wrapper process itself. Killing its whole tree here can
    # also terminate the newly spawned replacement wrapper before it finishes
    # taking over the frontend/tunnel/connector processes.
    Stop-Process -Id $oldWrapperPid -Force -ErrorAction SilentlyContinue
  }
}
Set-Content -LiteralPath $WrapperPidFile -Value $PID -Encoding Ascii

# Desktop shortcuts inherit the environment of Explorer, which may still have
# values from before the tunnel key or developer tools were installed.
$savedTunnelKey = [Environment]::GetEnvironmentVariable('CONTROL_PLANE_API_KEY', 'User')
if ($savedTunnelKey) { $env:CONTROL_PLANE_API_KEY = $savedTunnelKey }

$pythonDir = Join-Path $env:LOCALAPPDATA 'Programs\Python\Python313'
$pythonScripts = Join-Path $pythonDir 'Scripts'
$toolDirs = @((Join-Path $env:USERPROFILE 'bin'), (Join-Path $env:APPDATA 'npm'), $pythonDir, $pythonScripts)
foreach ($toolDir in $toolDirs) {
  if ($toolDir -and (Test-Path -LiteralPath $toolDir) -and (($env:Path -split ';') -notcontains $toolDir)) {
    $env:Path = "$toolDir;$env:Path"
  }
}

function Log([string]$Level,[string]$Message,[ConsoleColor]$Color = [ConsoleColor]::Gray) {
  $ts = Get-Date -Format 'HH:mm:ss.fff'
  Write-Host "[$ts] [$Level] " -ForegroundColor DarkGray -NoNewline
  Write-Host $Message -ForegroundColor $Color
}
function Stop-Matching([string]$Label,[scriptblock]$Filter) {
  $items = @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object $Filter)
  if ($items.Count -eq 0) { Log 'INFO' "${Label}: no stale copies" DarkGray; return }
  foreach ($p in $items) {
    Log 'WARN' "${Label}: stopping PID=$($p.ProcessId) $($p.Name)" Yellow
    Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
  }
}
function Stop-TunnelTrees {
  $items = @(Get-Process tunnel-client -ErrorAction SilentlyContinue)
  if ($items.Count -eq 0) { Log 'INFO' 'Tunnel: no stale copies' DarkGray; return }
  foreach ($p in $items) {
    Log 'WARN' "Tunnel: stopping process tree PID=$($p.Id)" Yellow
    & taskkill.exe /PID $p.Id /T /F 2>$null | Out-Null
  }
}
function Test-Tcp([string]$HostName,[int]$Port,[int]$TimeoutMs=1200) {
  $c = [Net.Sockets.TcpClient]::new()
  try {
    $a = $c.BeginConnect($HostName,$Port,$null,$null)
    if (-not $a.AsyncWaitHandle.WaitOne($TimeoutMs)) { return $false }
    $c.EndConnect($a); return $true
  } catch { return $false } finally { $c.Dispose() }
}
function Network-State {
  try { $dns = [Net.Dns]::GetHostAddresses('api.openai.com') | Select-Object -First 1 } catch { $dns = $null }
  if ($dns) { Log 'NET ' "DNS api.openai.com -> $dns" Cyan } else { Log 'NET ' 'DNS api.openai.com FAILED' Red }
  if (Test-Tcp '127.0.0.1' 8765 500) { Log 'NET ' 'Frontend 127.0.0.1:8765 listening' Green } else { Log 'NET ' 'Frontend 127.0.0.1:8765 not listening' Yellow }
}
function Resolve-TunnelClient {
  $candidates = @()
  if ($env:TUNNEL_CLIENT_PATH) { $candidates += $env:TUNNEL_CLIENT_PATH }

  $command = Get-Command tunnel-client.exe -ErrorAction SilentlyContinue
  if (-not $command) { $command = Get-Command tunnel-client -ErrorAction SilentlyContinue }
  if ($command) { $candidates += $command.Source }

  $candidates += @(
    (Join-Path $env:USERPROFILE 'bin\tunnel-client.exe'),
    (Join-Path $env:LOCALAPPDATA 'Microsoft\WinGet\Links\tunnel-client.exe'),
    (Join-Path $Repo '.local\bin\tunnel-client.exe')
  )

  foreach ($candidate in $candidates) {
    if ($candidate -and (Test-Path -LiteralPath $candidate -PathType Leaf)) {
      return (Resolve-Path -LiteralPath $candidate).Path
    }
  }
  return $null
}

Log 'INFO' "CodexPC start wrapper repo=$Repo" Cyan

$tunnelClient = Resolve-TunnelClient
if (-not $tunnelClient) {
  Log 'ERROR' 'tunnel-client is not installed or cannot be found.' Red
  Log 'INFO' 'Install the OpenAI tunnel runtime or set TUNNEL_CLIENT_PATH to tunnel-client.exe.' Yellow
  Log 'INFO' 'Searched PATH, %USERPROFILE%\bin, WinGet Links, and .local\bin.' DarkGray
  exit 127
}
Log 'INFO' "Tunnel client: $tunnelClient" DarkGray
Stop-Matching 'Frontend supervisor' { $_.Name -eq 'powershell.exe' -and $_.CommandLine -match 'frontend-supervisor\.ps1' }
Stop-Matching 'Frontend' { $_.Name -match '^pythonw?\.exe$' -and $_.CommandLine -match 'CodexPC Monitor\.pyw' }
Stop-TunnelTrees
Stop-Matching 'Connector' { ($_.Name -eq 'codexpc-go.exe' -and $_.ExecutablePath -like '*\codex-mcp-router\dist\codexpc-go.exe') -or ($_.CommandLine -match 'python(?:\.exe)?\s+-m\s+codexpc_connector') }

# Reset only the frontend-visible history at connector restart. Keep the full
# connector.jsonl log intact for diagnostics; the monitor starts reading from
# this byte offset on the new connector session.
$historyOffsetPath = Join-Path $StateDir 'frontend-history.offset'
$connectorLogPath = Join-Path $StateDir 'logs\connector.jsonl'
$historyOffset = 0
if (Test-Path -LiteralPath $connectorLogPath) {
  try { $historyOffset = (Get-Item -LiteralPath $connectorLogPath).Length } catch { $historyOffset = 0 }
}
Set-Content -LiteralPath $historyOffsetPath -Value $historyOffset -Encoding Ascii
Log 'INFO' "Frontend history reset at log offset=$historyOffset" DarkGray

Remove-Item (Join-Path $StateDir 'connector.lock') -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 350

# Promote a staged connector build only after the old connector process is gone.
$distDir = Join-Path $Repo 'dist'
$currentConnector = Join-Path $distDir 'codexpc-go.exe'
$stagedConnector = Join-Path $distDir 'codexpc-go.next.exe'
$previousConnector = Join-Path $distDir 'codexpc-go.prev.exe'
if (Test-Path -LiteralPath $stagedConnector) {
  try {
    if (Test-Path -LiteralPath $previousConnector) { Remove-Item -LiteralPath $previousConnector -Force }
    if (Test-Path -LiteralPath $currentConnector) { Move-Item -LiteralPath $currentConnector -Destination $previousConnector -Force }
    Move-Item -LiteralPath $stagedConnector -Destination $currentConnector -Force
    Log 'INFO' 'Promoted staged connector build: codexpc-go.next.exe -> codexpc-go.exe' Green
  } catch {
    Log 'ERROR' "Failed to promote staged connector build: $($_.Exception.Message)" Red
  }
}

$frontendSupervisor = Join-Path $PSScriptRoot 'frontend-supervisor.ps1'
Log 'INFO' 'Starting frontend supervisor' Cyan
Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-File','"' + $frontendSupervisor + '"') -WindowStyle Hidden
$frontReady = $false
for ($i=0; $i -lt 20; $i++) {
  Start-Sleep -Milliseconds 150
  if (Test-Tcp '127.0.0.1' 8765 150) { $frontReady=$true; break }
}
if ($frontReady) {
  Log 'INFO' "Frontend ready and auth bootstrap requested: $FrontUrl" Green
} else { Log 'ERROR' 'Frontend failed to bind 127.0.0.1:8765' Red }

Network-State
Log 'INFO' 'Starting tunnel profile=codex' Cyan
$attempt = 0
while ($true) {
  $attempt++
  Log 'INFO' "Tunnel launch attempt=$attempt" Magenta
  & $tunnelClient run --profile codex
  $exit = $LASTEXITCODE
  Log 'ERROR' "Tunnel stopped exit_code=$exit" Red
  $connector = @(Get-Process codexpc-go -ErrorAction SilentlyContinue)
  Log 'STATE' "connector_processes=$($connector.Count)" $(if($connector.Count){'Yellow'}else{'DarkGray'})
  Network-State
  Log 'WARN' 'Restarting tunnel in 3s (Ctrl+C stops wrapper)' Yellow
  Start-Sleep -Seconds 3
}


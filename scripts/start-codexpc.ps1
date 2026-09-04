param()
$ErrorActionPreference = 'Continue'
$Repo = Split-Path -Parent $PSScriptRoot
$Monitor = Join-Path $Repo 'frontend\server.pyw'
$FrontUrl = 'http://127.0.0.1:8765/'
$StateDir = if ($env:CODEXPC_STATE_DIR) { $env:CODEXPC_STATE_DIR } else { Join-Path $env:LOCALAPPDATA 'CodexPCConnector' }
$StateBin = Join-Path $StateDir 'bin'
$ConfigPath = Join-Path $StateDir 'config.toml'
$TunnelKeyPath = Join-Path $StateDir 'tunnel-runtime-key.dpapi'
$SetupPendingPath = Join-Path $StateDir 'setup.pending.json'
$WrapperPidFile = Join-Path $StateDir 'wrapper.pid'
New-Item -ItemType Directory -Path $StateDir -Force | Out-Null

function Show-CodexPCIntro {
  if ($env:CODEXPC_NO_INTRO -eq '1' -or [Console]::IsOutputRedirected) { return }

  $art = @(
    '   ____          _           ____   ____'
    '  / ___|___   __| | _____  _|  _ \ / ___|'
    ' | |   / _ \ / _` |/ _ \ \/ / |_) | |'
    ' | |__| (_) | (_| |  __/>  <|  __/| |___'
    '  \____\___/ \__,_|\___/_/\_\_|    \____|'
  )

  $cursorVisible = $true
  try {
    $cursorVisible = [Console]::CursorVisible
    [Console]::CursorVisible = $false
    Clear-Host

    $maxWidth = [int](($art | Measure-Object -Property Length -Maximum).Maximum)
    $left = [Math]::Max(0, [int](([Console]::WindowWidth - $maxWidth) / 2))
    $top = [Math]::Max(1, [int](([Console]::WindowHeight - $art.Count) / 2) - 1)
    $steps = 18

    for ($step = 1; $step -le $steps; $step++) {
      $visible = [Math]::Ceiling($maxWidth * ($step / [double]$steps))
      for ($row = 0; $row -lt $art.Count; $row++) {
        $line = $art[$row]
        $take = [Math]::Min([int]$visible, $line.Length)
        $piece = if ($take -gt 0) { $line.Substring(0, $take) } else { '' }
        [Console]::SetCursorPosition($left, $top + $row)
        Write-Host ($piece.PadRight($maxWidth)) -NoNewline -ForegroundColor Gray
      }
      Start-Sleep -Milliseconds 24
    }

    for ($row = 0; $row -lt $art.Count; $row++) {
      [Console]::SetCursorPosition($left, $top + $row)
      Write-Host ($art[$row].PadRight($maxWidth)) -NoNewline -ForegroundColor White
    }
    Start-Sleep -Milliseconds 190

    foreach ($color in @('Gray', 'DarkGray', 'Black')) {
      for ($row = 0; $row -lt $art.Count; $row++) {
        [Console]::SetCursorPosition($left, $top + $row)
        Write-Host ($art[$row].PadRight($maxWidth)) -NoNewline -ForegroundColor $color
      }
      Start-Sleep -Milliseconds 85
    }

    Clear-Host
  } catch {
    Clear-Host
  } finally {
    try { [Console]::CursorVisible = $cursorVisible } catch {}
  }

  Write-Host 'CodexPC' -ForegroundColor White
  Write-Host 'Starting local connector...' -ForegroundColor DarkGray
  Write-Host ''
}

Show-CodexPCIntro

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
# an old PATH. Add CodexPC-managed runtime locations explicitly.
$pythonDirs = @('Python314','Python313','Python312','Python311') | ForEach-Object { Join-Path $env:LOCALAPPDATA "Programs\Python\$_" }
$toolDirs = @($StateBin, (Join-Path $env:USERPROFILE 'bin'), (Join-Path $env:APPDATA 'npm'))
foreach ($pythonDir in $pythonDirs) {
  $toolDirs += $pythonDir
  $toolDirs += (Join-Path $pythonDir 'Scripts')
}
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

  $persisted = [Environment]::GetEnvironmentVariable('TUNNEL_CLIENT_PATH', 'User')
  if ($persisted) { $candidates += $persisted }
  $candidates += @(
    (Join-Path $StateBin 'tunnel-client.exe'),
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
function Read-ConfigValue([string]$Name,[string]$Default='') {
  if (-not (Test-Path -LiteralPath $ConfigPath)) { return $Default }
  try {
    foreach ($line in Get-Content -LiteralPath $ConfigPath -ErrorAction Stop) {
      $trimmed = $line.Trim()
      if (-not $trimmed -or $trimmed.StartsWith('#') -or -not $trimmed.Contains('=')) { continue }
      $parts = $trimmed.Split('=',2)
      if ($parts[0].Trim() -ne $Name) { continue }
      $raw = $parts[1].Trim()
      if ($raw.StartsWith('"')) {
        try { return ($raw | ConvertFrom-Json) } catch { return $raw.Trim('"') }
      }
      return $raw
    }
  } catch {}
  return $Default
}
function Read-TunnelKey {
  if (Test-Path -LiteralPath $TunnelKeyPath) {
    try {
      $encoded = (Get-Content -LiteralPath $TunnelKeyPath -Raw -ErrorAction Stop).Trim()
      $protected = [Convert]::FromBase64String($encoded)
      $plain = [Security.Cryptography.ProtectedData]::Unprotect($protected,$null,[Security.Cryptography.DataProtectionScope]::CurrentUser)
      return [Text.Encoding]::UTF8.GetString($plain)
    } catch {
      Log 'WARN' 'Saved tunnel credential could not be decrypted for this Windows account.' Yellow
    }
  }
  # One-release compatibility path for users upgrading from the previous launcher.
  return [Environment]::GetEnvironmentVariable('CONTROL_PLANE_API_KEY', 'User')
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
$attempt = 0
$waitingForSetup = $false
while ($true) {
  $profile = Read-ConfigValue 'tunnel_profile' 'codex'
  $tunnelId = Read-ConfigValue 'tunnel_id' ''
  $tunnelKey = Read-TunnelKey
  $setupPending = Test-Path -LiteralPath $SetupPendingPath
  if ($setupPending -or -not $tunnelKey -or $tunnelId -notmatch '^tunnel_[0-9a-f]{32}$') {
    if (-not $waitingForSetup) {
      Log 'SETUP' 'Waiting for a validated setup in the CodexPC frontend.' Yellow
      Log 'SETUP' 'Complete Setup & settings; the tunnel starts only after validation passes.' DarkGray
      $waitingForSetup = $true
    }
    Remove-Item Env:CONTROL_PLANE_API_KEY -ErrorAction SilentlyContinue
    Remove-Item Env:CONTROL_PLANE_TUNNEL_ID -ErrorAction SilentlyContinue
    $tunnelKey = $null
    Start-Sleep -Seconds 2
    continue
  }
  if ($waitingForSetup) { Log 'SETUP' 'Configuration detected. Starting tunnel.' Green }
  $waitingForSetup = $false
  $env:CONTROL_PLANE_API_KEY = $tunnelKey
  $env:CONTROL_PLANE_TUNNEL_ID = $tunnelId
  $attempt++
  Log 'INFO' "Tunnel launch attempt=$attempt profile=$profile" Magenta
  & $tunnelClient run --profile $profile
  $exit = $LASTEXITCODE
  Remove-Item Env:CONTROL_PLANE_API_KEY -ErrorAction SilentlyContinue
  Remove-Item Env:CONTROL_PLANE_TUNNEL_ID -ErrorAction SilentlyContinue
  $tunnelKey = $null
  Log 'ERROR' "Tunnel stopped exit_code=$exit" Red
  $connector = @(Get-Process codexpc-go -ErrorAction SilentlyContinue)
  Log 'STATE' "connector_processes=$($connector.Count)" $(if($connector.Count){'Yellow'}else{'DarkGray'})
  Network-State
  Log 'WARN' 'Restarting tunnel in 3s (Ctrl+C stops wrapper)' Yellow
  Start-Sleep -Seconds 3
}


param(
  [switch]$NoLaunch
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$Repo = Split-Path -Parent $PSScriptRoot
$StateDir = if ($env:CODEXPC_STATE_DIR) { $env:CODEXPC_STATE_DIR } else { Join-Path $env:LOCALAPPDATA 'CodexPCConnector' }
$StateBin = Join-Path $StateDir 'bin'
$ToolchainDir = Join-Path $StateDir 'toolchain'
New-Item -ItemType Directory -Force -Path $StateDir, $StateBin, $ToolchainDir | Out-Null

function Step([string]$Message) {
  Write-Host "`n==> $Message" -ForegroundColor Cyan
}

function Add-UserPath([string]$Directory) {
  if (-not $Directory -or -not (Test-Path -LiteralPath $Directory)) { return }
  $parts = @($env:Path -split ';' | Where-Object { $_ })
  if ($parts -notcontains $Directory) { $env:Path = "$Directory;$env:Path" }
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $userParts = @($userPath -split ';' | Where-Object { $_ })
  if ($userParts -notcontains $Directory) {
    $next = if ($userPath) { "$Directory;$userPath" } else { $Directory }
    [Environment]::SetEnvironmentVariable('Path', $next, 'User')
  }
}

function Get-GoVersion([string]$Exe) {
  if (-not $Exe -or -not (Test-Path -LiteralPath $Exe)) { return $null }
  try {
    $line = (& $Exe version 2>$null | Select-Object -First 1)
    if ($line -match 'go([0-9]+\.[0-9]+(?:\.[0-9]+)?)') { return [version]$Matches[1] }
  } catch {}
  return $null
}

function Resolve-Go([version]$Minimum) {
  $command = Get-Command go.exe -ErrorAction SilentlyContinue
  $candidates = @(
    (Join-Path $ToolchainDir 'go\bin\go.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\go\bin\go.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\Go.full\go\bin\go.exe'),
    (Join-Path $env:ProgramFiles 'Go\bin\go.exe'),
    $(if ($command) { $command.Source })
  ) | Where-Object { $_ } | Select-Object -Unique
  foreach ($candidate in $candidates) {
    $version = Get-GoVersion $candidate
    if ($version -and $version -ge $Minimum) { return $candidate }
  }
  return $null
}

function Install-Go([version]$Minimum) {
  Step "Installing Go $Minimum+"
  $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
  $releases = Invoke-RestMethod -Uri 'https://go.dev/dl/?mode=json' -Headers @{ 'User-Agent' = 'CodexPC-Installer' }
  $release = $releases | Where-Object {
    $_.stable -and ([version]($_.version -replace '^go','')) -ge $Minimum -and
    ($_.files | Where-Object { $_.filename -match "\.windows-$arch\.zip$" })
  } | Sort-Object { [version]($_.version -replace '^go','') } -Descending | Select-Object -First 1
  if (-not $release) { throw "No stable Go release >= $Minimum was found for windows-$arch." }
  $file = $release.files | Where-Object { $_.filename -match "\.windows-$arch\.zip$" } | Select-Object -First 1
  $tmpRoot = Join-Path $env:TEMP ("codexpc-go-" + [guid]::NewGuid().ToString('N'))
  $zip = Join-Path $tmpRoot $file.filename
  New-Item -ItemType Directory -Force -Path $tmpRoot | Out-Null
  try {
    Invoke-WebRequest -Uri ("https://go.dev/dl/" + $file.filename) -OutFile $zip -Headers @{ 'User-Agent' = 'CodexPC-Installer' }
    $actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($file.sha256 -and $actual -ne ([string]$file.sha256).ToLowerInvariant()) { throw 'Go download checksum verification failed.' }
    $extract = Join-Path $tmpRoot 'extract'
    Expand-Archive -LiteralPath $zip -DestinationPath $extract -Force
    $target = Join-Path $ToolchainDir 'go'
    if (Test-Path -LiteralPath $target) { Remove-Item -LiteralPath $target -Recurse -Force }
    Move-Item -LiteralPath (Join-Path $extract 'go') -Destination $target
    Add-UserPath (Join-Path $target 'bin')
    return (Join-Path $target 'bin\go.exe')
  } finally {
    Remove-Item -LiteralPath $tmpRoot -Recurse -Force -ErrorAction SilentlyContinue
  }
}

function Resolve-PythonW {
  $command = Get-Command pythonw.exe -ErrorAction SilentlyContinue
  if ($command) { return $command.Source }
  $known = @(
    (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python314\pythonw.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python313\pythonw.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python312\pythonw.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python311\pythonw.exe')
  )
  return $known | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
}

function Ensure-Python {
  $pythonw = Resolve-PythonW
  if ($pythonw) { return $pythonw }
  Step 'Installing Python for the local CodexPC frontend'
  $winget = Get-Command winget.exe -ErrorAction SilentlyContinue
  if (-not $winget) { throw 'Python is missing and winget is unavailable. Install Python 3.11+ once, then rerun install.cmd.' }
  & $winget.Source install --id Python.Python.3.13 -e --scope user --silent --accept-package-agreements --accept-source-agreements
  if ($LASTEXITCODE -ne 0) { throw "Python installation failed with exit code $LASTEXITCODE" }
  $pythonw = Resolve-PythonW
  if (-not $pythonw) { throw 'Python was installed but pythonw.exe could not be located.' }
  Add-UserPath (Split-Path -Parent $pythonw)
  return $pythonw
}

function Resolve-Codex {
  $knownDirs = @(
    (Join-Path $env:LOCALAPPDATA 'Programs\OpenAI\Codex\bin'),
    (Join-Path $env:USERPROFILE '.local\bin')
  )
  foreach ($dir in $knownDirs) {
    if (Test-Path -LiteralPath $dir) { Add-UserPath $dir }
  }
  $command = Get-Command codex.exe -ErrorAction SilentlyContinue
  if (-not $command) { $command = Get-Command codex -ErrorAction SilentlyContinue }
  if ($command) { return $command.Source }
  return $null
}

function Ensure-Codex {
  $codex = Resolve-Codex
  if ($codex) { return $codex }
  Step 'Installing OpenAI Codex CLI'
  $tmpRoot = Join-Path $env:TEMP ("codexpc-codex-" + [guid]::NewGuid().ToString('N'))
  $installer = Join-Path $tmpRoot 'install.ps1'
  New-Item -ItemType Directory -Force -Path $tmpRoot | Out-Null
  try {
    Invoke-WebRequest -Uri 'https://chatgpt.com/codex/install.ps1' -OutFile $installer -Headers @{ 'User-Agent' = 'CodexPC-Installer' }
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $installer
    if ($LASTEXITCODE -ne 0) { throw "Official Codex CLI installer failed with exit code $LASTEXITCODE" }
    $codex = Resolve-Codex
    if (-not $codex) { throw 'Codex CLI was installed but codex.exe could not be located.' }
    return $codex
  } finally {
    Remove-Item -LiteralPath $tmpRoot -Recurse -Force -ErrorAction SilentlyContinue
  }
}

function Test-CodexLogin([string]$Codex) {
  $previousPreference = $ErrorActionPreference
  try {
    # The npm PowerShell shim writes a successful login-status message to
    # stderr. Under ErrorActionPreference=Stop PowerShell 5.1 turns that into
    # NativeCommandError even though the process exits 0.
    $ErrorActionPreference = 'Continue'
    & $Codex login status 1>$null 2>$null
    return $LASTEXITCODE -eq 0
  } finally {
    $ErrorActionPreference = $previousPreference
  }
}

function Ensure-CodexLogin([string]$Codex) {
  if (Test-CodexLogin $Codex) { return }
  Step 'Signing in to Codex'
  Write-Host 'Codex CLI needs a one-time OpenAI sign-in. Complete the browser flow to continue.' -ForegroundColor Yellow
  $previousPreference = $ErrorActionPreference
  try {
    $ErrorActionPreference = 'Continue'
    & $Codex login
    $loginExit = $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $previousPreference
  }
  if ($loginExit -ne 0) { throw "Codex sign-in failed with exit code $loginExit" }
  if (-not (Test-CodexLogin $Codex)) { throw 'Codex CLI is still not authenticated.' }
}

function Resolve-TunnelClient {
  $configured = [Environment]::GetEnvironmentVariable('TUNNEL_CLIENT_PATH', 'User')
  $candidates = @(
    $env:TUNNEL_CLIENT_PATH,
    $configured,
    (Join-Path $StateBin 'tunnel-client.exe'),
    (Join-Path $env:USERPROFILE 'bin\tunnel-client.exe'),
    (Get-Command tunnel-client.exe -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source -First 1)
  ) | Where-Object { $_ }
  return $candidates | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
}

function Test-TunnelClientCompatibility([string]$TunnelClient) {
  if (-not $TunnelClient -or -not (Test-Path -LiteralPath $TunnelClient -PathType Leaf)) { return $false }
  $previousPreference = $ErrorActionPreference
  try {
    $ErrorActionPreference = 'Continue'
    $initHelp = (& $TunnelClient init --help 2>&1 | Out-String)
    $initExit = $LASTEXITCODE
    $doctorHelp = (& $TunnelClient doctor --help 2>&1 | Out-String)
    $doctorExit = $LASTEXITCODE
  } finally {
    $ErrorActionPreference = $previousPreference
  }
  return $initExit -eq 0 -and $doctorExit -eq 0 -and
    $initHelp.Contains('--profile-dir') -and $initHelp.Contains('--health-listen-addr') -and
    $doctorHelp.Contains('--profile-dir')
}

function Install-TunnelClient {
  Step 'Installing OpenAI tunnel-client'
  $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
  $release = Invoke-RestMethod -Uri 'https://api.github.com/repos/openai/tunnel-client/releases/latest' -Headers @{ 'User-Agent' = 'CodexPC-Installer'; 'Accept' = 'application/vnd.github+json' }
  $assets = @($release.assets | Where-Object {
    $_.name -match "(?:^|-)windows-$arch\.zip$" -and $_.name -notmatch 'runtime'
  } | Sort-Object { ([string]$_.name).Length })
  $asset = $assets | Select-Object -First 1
  if (-not $asset) { throw "No full tunnel-client windows-$arch release archive was found." }
  $tmpRoot = Join-Path $env:TEMP ("codexpc-tunnel-" + [guid]::NewGuid().ToString('N'))
  $zip = Join-Path $tmpRoot ([string]$asset.name)
  New-Item -ItemType Directory -Force -Path $tmpRoot | Out-Null
  try {
    Invoke-WebRequest -Uri $asset.browser_download_url -OutFile $zip -Headers @{ 'User-Agent' = 'CodexPC-Installer' }
    $actual = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
    $verified = $false
    if ($asset.digest -and ([string]$asset.digest) -match '^sha256:(.+)$') {
      if ($actual -ne $Matches[1].ToLowerInvariant()) { throw 'tunnel-client download checksum verification failed.' }
      $verified = $true
    }
    if (-not $verified) {
      $sumAsset = $release.assets | Where-Object { $_.name -eq 'SHA256SUMS.txt' } | Select-Object -First 1
      if (-not $sumAsset) { throw 'tunnel-client release has no SHA256 checksum metadata.' }
      $sumFile = Join-Path $tmpRoot 'SHA256SUMS.txt'
      Invoke-WebRequest -Uri $sumAsset.browser_download_url -OutFile $sumFile -Headers @{ 'User-Agent' = 'CodexPC-Installer' }
      $line = Get-Content -LiteralPath $sumFile | Where-Object { $_ -match ([regex]::Escape([string]$asset.name) + '$') } | Select-Object -First 1
      if (-not $line -or $line -notmatch '^([0-9a-fA-F]{64})\s+') { throw 'Could not find the tunnel-client archive in SHA256SUMS.txt.' }
      if ($actual -ne $Matches[1].ToLowerInvariant()) { throw 'tunnel-client download checksum verification failed.' }
    }
    $extract = Join-Path $tmpRoot 'extract'
    Expand-Archive -LiteralPath $zip -DestinationPath $extract -Force
    $exe = Get-ChildItem -LiteralPath $extract -Filter 'tunnel-client.exe' -File -Recurse | Select-Object -First 1
    if (-not $exe) { throw 'tunnel-client.exe was not present in the official release archive.' }
    if (Test-Path -LiteralPath $StateBin) { Get-ChildItem -LiteralPath $StateBin -Force | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue }
    Copy-Item -Path (Join-Path $exe.Directory.FullName '*') -Destination $StateBin -Recurse -Force
    $target = Join-Path $StateBin 'tunnel-client.exe'
    if (-not (Test-Path -LiteralPath $target)) { Copy-Item -LiteralPath $exe.FullName -Destination $target -Force }
    Add-UserPath $StateBin
    [Environment]::SetEnvironmentVariable('TUNNEL_CLIENT_PATH', $target, 'User')
    $env:TUNNEL_CLIENT_PATH = $target
    return $target
  } finally {
    Remove-Item -LiteralPath $tmpRoot -Recurse -Force -ErrorAction SilentlyContinue
  }
}

Set-Location $Repo
$goLine = Get-Content -LiteralPath (Join-Path $Repo 'go.mod') | Where-Object { $_ -match '^go\s+' } | Select-Object -First 1
if (-not $goLine -or $goLine -notmatch '^go\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)') { throw 'Could not read the required Go version from go.mod.' }
$requiredGo = [version]$Matches[1]

Step 'Checking dependencies'
$go = Resolve-Go $requiredGo
if (-not $go) { $go = Install-Go $requiredGo }
Add-UserPath (Split-Path -Parent $go)
$pythonw = Ensure-Python
[Environment]::SetEnvironmentVariable('CODEXPC_PYTHONW_PATH', $pythonw, 'User')
$env:CODEXPC_PYTHONW_PATH = $pythonw
$codex = Ensure-Codex
Ensure-CodexLogin $codex
$tunnelClient = Resolve-TunnelClient
if (-not (Test-TunnelClientCompatibility $tunnelClient)) {
  if ($tunnelClient) { Write-Host 'Existing tunnel-client is too old for the CodexPC setup flow; upgrading it.' -ForegroundColor Yellow }
  $tunnelClient = Install-TunnelClient
}

Write-Host "Go            $(& $go version)" -ForegroundColor DarkGray
Write-Host "Python UI     $pythonw" -ForegroundColor DarkGray
Write-Host "Codex CLI     $(& $codex --version 2>$null | Select-Object -First 1)" -ForegroundColor DarkGray
Write-Host "tunnel-client $tunnelClient" -ForegroundColor DarkGray

Step 'Downloading Go modules'
& $go mod download
if ($LASTEXITCODE -ne 0) { throw "go mod download failed with exit code $LASTEXITCODE" }

Step 'Building and testing CodexPC'
$env:Path = "$(Split-Path -Parent $go);$env:Path"
& (Join-Path $PSScriptRoot 'build.ps1') -NoDesktopCopy
if ($LASTEXITCODE -ne 0) { throw "CodexPC build failed with exit code $LASTEXITCODE" }

Write-Host "`n[OK] CodexPC is installed." -ForegroundColor Green
Write-Host 'The browser setup will ask only for your workspace and OpenAI tunnel credentials.' -ForegroundColor Gray

if (-not $NoLaunch) {
  Step 'Opening first-run setup'
  Start-Process -FilePath (Join-Path $Repo 'start.cmd')
}

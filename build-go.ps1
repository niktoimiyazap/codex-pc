param(
    [switch]$SkipSmoke,
    [switch]$NoDesktopCopy
)

$ErrorActionPreference = 'Stop'
$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Root

function Resolve-GoExe {
    $cmd = Get-Command go.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }

    $candidates = @(
        "$env:LOCALAPPDATA\Programs\Go\bin\go.exe",
        "$env:LOCALAPPDATA\Programs\Go.full\go\bin\go.exe",
        "$env:ProgramFiles\Go\bin\go.exe"
    )
    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate)) { return $candidate }
    }
    throw 'Go toolchain not found. Install Go or add go.exe to PATH.'
}

$Go = Resolve-GoExe
$Gofmt = Join-Path (Split-Path -Parent $Go) 'gofmt.exe'
if (-not (Test-Path -LiteralPath $Gofmt)) { throw "gofmt.exe not found next to $Go" }
$Dist = Join-Path $Root 'dist'
$FinalExe = Join-Path $Dist 'codexpc-go.exe'
$TempExe = Join-Path $Dist 'codexpc-go.build.exe'
$DesktopExe = Join-Path ([Environment]::GetFolderPath('Desktop')) 'codexpc-go.exe'

New-Item -ItemType Directory -Force -Path $Dist | Out-Null
if (Test-Path -LiteralPath $TempExe) { Remove-Item -LiteralPath $TempExe -Force }

Write-Host "[1/5] gofmt" -ForegroundColor Cyan
$goFiles = @(Get-ChildItem -Path (Join-Path $Root 'cmd'), (Join-Path $Root 'internal') -Recurse -Filter *.go -File | ForEach-Object FullName)
if ($goFiles.Count -gt 0) {
    & $Gofmt -w @goFiles
    if ($LASTEXITCODE -ne 0) { throw "gofmt failed with exit code $LASTEXITCODE" }
}

Write-Host "[2/5] go test ./..." -ForegroundColor Cyan
$oldWorkspace = $env:CODEXPC_WORKSPACE
$oldAllowedRoots = $env:CODEXPC_ALLOWED_ROOTS
$oldToolProfile = $env:CODEXPC_TOOL_PROFILE
try {
    Remove-Item Env:CODEXPC_WORKSPACE -ErrorAction SilentlyContinue
    Remove-Item Env:CODEXPC_ALLOWED_ROOTS -ErrorAction SilentlyContinue
    Remove-Item Env:CODEXPC_TOOL_PROFILE -ErrorAction SilentlyContinue
    & $Go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
} finally {
    if ($null -ne $oldWorkspace) { $env:CODEXPC_WORKSPACE = $oldWorkspace }
    if ($null -ne $oldAllowedRoots) { $env:CODEXPC_ALLOWED_ROOTS = $oldAllowedRoots }
    if ($null -ne $oldToolProfile) { $env:CODEXPC_TOOL_PROFILE = $oldToolProfile }
}

Write-Host "[3/5] build" -ForegroundColor Cyan
& $Go build -trimpath -o $TempExe ./cmd/codexpc
if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }

if (-not $SkipSmoke) {
    Write-Host "[4/5] smoke" -ForegroundColor Cyan
    & $TempExe --smoke
    if ($LASTEXITCODE -ne 0) { throw "smoke test failed with exit code $LASTEXITCODE" }
} else {
    Write-Host "[4/5] smoke skipped" -ForegroundColor DarkYellow
}

Write-Host "[5/5] deploy" -ForegroundColor Cyan
$DeployedExe = $FinalExe
try {
    Move-Item -LiteralPath $TempExe -Destination $FinalExe -Force
} catch {
    # A running connector locks the active binary on Windows. Staging is a
    # successful deployment state: the restart wrapper will promote it after
    # stopping the old process.
    $staged = Join-Path $Dist 'codexpc-go.next.exe'
    if (Test-Path -LiteralPath $staged) { Remove-Item -LiteralPath $staged -Force }
    Move-Item -LiteralPath $TempExe -Destination $staged -Force
    $DeployedExe = $staged
    Write-Host "Active connector is running; staged fresh build as $staged" -ForegroundColor Yellow
}

if (-not $NoDesktopCopy -and $DeployedExe -eq $FinalExe) {
    try {
        Copy-Item -LiteralPath $FinalExe -Destination $DesktopExe -Force
    } catch {
        Write-Warning "Built successfully, but desktop copy is locked. Close the manually started desktop codexpc-go.exe and copy again."
    }
}

$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $DeployedExe).Hash.ToLowerInvariant()
$size = (Get-Item -LiteralPath $DeployedExe).Length
Write-Host ""
Write-Host "Build OK" -ForegroundColor Green
Write-Host "Binary : $DeployedExe"
Write-Host "Size   : $size bytes"
Write-Host "SHA256 : $hash"
if (-not $NoDesktopCopy -and $DeployedExe -eq $FinalExe) { Write-Host "Desktop: $DesktopExe" }

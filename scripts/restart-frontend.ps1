$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$frontend = Join-Path $repo 'frontend\server.pyw'

$pythonw = $env:CODEXPC_PYTHONW_PATH
if (-not $pythonw) { $pythonw = [Environment]::GetEnvironmentVariable('CODEXPC_PYTHONW_PATH', 'User') }
if (-not $pythonw -or -not (Test-Path -LiteralPath $pythonw -PathType Leaf)) {
    $command = Get-Command pythonw.exe -ErrorAction SilentlyContinue
    if ($command) { $pythonw = $command.Source }
}
if (-not $pythonw -or -not (Test-Path -LiteralPath $pythonw -PathType Leaf)) {
    $known = @(
        (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python314\pythonw.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python313\pythonw.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python312\pythonw.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Python\Python311\pythonw.exe')
    )
    $pythonw = $known | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
}
if (-not $pythonw) { throw 'pythonw.exe was not found. Run install.cmd again.' }

$deadline = (Get-Date).AddSeconds(8)
do {
    $listener = Get-NetTCPConnection -LocalPort 8765 -State Listen -ErrorAction SilentlyContinue
    if (-not $listener) { break }
    Start-Sleep -Milliseconds 100
} while ((Get-Date) -lt $deadline)

Start-Process -FilePath $pythonw -ArgumentList @('"' + $frontend + '"', '--no-browser') -WindowStyle Hidden

param([switch]$NoBrowser)
$ErrorActionPreference = 'Continue'

$createdNew = $false
$supervisorMutex = [System.Threading.Mutex]::new($true, 'Local\CodexPCFrontendSupervisor', [ref]$createdNew)
if (-not $createdNew) {
    $supervisorMutex.Dispose()
    exit 0
}

$repo = Split-Path -Parent $PSScriptRoot
$monitor = Join-Path $repo 'frontend\server.pyw'
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

$first = $true
try {
    while ($true) {
        try {
            $args = @('"' + $monitor + '"')
            if ($NoBrowser -or -not $first) { $args += '--no-browser' }
            $first = $false
            $proc = Start-Process -FilePath $pythonw -ArgumentList $args -WindowStyle Hidden -PassThru
            Wait-Process -Id $proc.Id
            $proc.Refresh()
            if ($proc.ExitCode -eq 42) { break }
        } catch {
        }
        Start-Sleep -Milliseconds 250
    }
} finally {
    try { $supervisorMutex.ReleaseMutex() } catch {}
    $supervisorMutex.Dispose()
}

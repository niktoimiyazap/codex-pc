$ErrorActionPreference = 'Stop'
$monitor = Join-Path $PSScriptRoot 'CodexPC Monitor.pyw'
$pythonw = (Get-Command pythonw.exe -ErrorAction SilentlyContinue).Source
if (-not $pythonw) {
    $python = (Get-Command python.exe -ErrorAction Stop).Source
    $pythonw = Join-Path (Split-Path $python) 'pythonw.exe'
}
Start-Process -FilePath $pythonw -ArgumentList @('"' + $monitor + '"') -WindowStyle Hidden

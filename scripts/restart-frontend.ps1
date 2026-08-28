$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$monitor = Join-Path $repo 'monitor_ui\CodexPC Monitor.pyw'
$pythonw = 'C:\Users\niktoimiya\AppData\Local\Programs\Python\Python313\pythonw.exe'

$deadline = (Get-Date).AddSeconds(8)
do {
    $listener = Get-NetTCPConnection -LocalPort 8765 -State Listen -ErrorAction SilentlyContinue
    if (-not $listener) { break }
    Start-Sleep -Milliseconds 100
} while ((Get-Date) -lt $deadline)

$quotedMonitor = '"' + $monitor + '"'
Start-Process -FilePath $pythonw -ArgumentList @($quotedMonitor, '--no-browser') -WindowStyle Hidden

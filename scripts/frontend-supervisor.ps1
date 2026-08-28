$ErrorActionPreference = 'Continue'

$repo = Split-Path -Parent $PSScriptRoot
$monitor = Join-Path $repo 'monitor_ui\CodexPC Monitor.pyw'
$pythonw = (Get-Command pythonw.exe -ErrorAction SilentlyContinue).Source
if (-not $pythonw) {
    $python = (Get-Command python.exe -ErrorAction Stop).Source
    $pythonw = Join-Path (Split-Path $python) 'pythonw.exe'
}

$first = $true
while ($true) {
    try {
        $args = @('"' + $monitor + '"')
        if (-not $first) { $args += '--no-browser' }
        $first = $false
        $proc = Start-Process -FilePath $pythonw -ArgumentList $args -WindowStyle Hidden -PassThru
        Wait-Process -Id $proc.Id
    } catch {
    }
    Start-Sleep -Milliseconds 250
}

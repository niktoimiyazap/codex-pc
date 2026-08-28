param()
$ErrorActionPreference = 'SilentlyContinue'
$Repo = Split-Path -Parent $PSScriptRoot
$Dist = Join-Path $Repo 'dist'
$Current = Join-Path $Dist 'codexpc-go.exe'
$Staged = Join-Path $Dist 'codexpc-go.next.exe'
$Previous = Join-Path $Dist 'codexpc-go.prev.exe'

$oldConnectorPids = @(Get-Process codexpc-go -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Id)
Get-Process tunnel-client -ErrorAction SilentlyContinue | ForEach-Object {
  & taskkill.exe /PID $_.Id /T /F 2>$null | Out-Null
}
Get-Process codexpc-go -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

# Do not race the wrapper restart: wait until every connector that existed
# before this restart is actually gone before promoting or accepting a new one.
$stopDeadline = (Get-Date).AddSeconds(5)
do {
  $stillAlive = @($oldConnectorPids | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
  if ($stillAlive.Count -eq 0) { break }
  $stillAlive | ForEach-Object { Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue }
  Start-Sleep -Milliseconds 100
} while ((Get-Date) -lt $stopDeadline)

Start-Sleep -Milliseconds 250
if (Test-Path -LiteralPath $Staged) {
  for ($i = 0; $i -lt 20; $i++) {
    try {
      if (Test-Path -LiteralPath $Previous) { Remove-Item -LiteralPath $Previous -Force }
      if (Test-Path -LiteralPath $Current) { Move-Item -LiteralPath $Current -Destination $Previous -Force }
      Move-Item -LiteralPath $Staged -Destination $Current -Force
      break
    } catch {
      Start-Sleep -Milliseconds 150
    }
  }
}

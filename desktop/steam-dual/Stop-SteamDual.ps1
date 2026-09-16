$ErrorActionPreference = 'Stop'
$expected = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual\tetherflow-steam-core.exe'
Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $expected } | Stop-Process
Write-Host 'SteamDual stopped. USB leak guard remains enabled; ordinary USB internet is still blocked.'

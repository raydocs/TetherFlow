# Use PowerShell 7 so behavior matches the supported/tested runtime.
$ErrorActionPreference = 'Stop'
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (!$admin) {
    $pwsh = (Get-Command pwsh.exe -ErrorAction Stop).Source
    Start-Process $pwsh -Verb RunAs -WindowStyle Hidden -ArgumentList @('-NoProfile','-File',('"'+$PSCommandPath+'"'))
    return
}
$stateDir = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual'
New-Item -ItemType Directory -Force $stateDir | Out-Null
Start-Transcript -Path (Join-Path $stateDir 'launcher.log') -Force
try { & "$PSScriptRoot\Start-SteamDual.ps1" -Background }
catch { Write-Output ($_ | Out-String); exit 1 }
finally { Stop-Transcript }

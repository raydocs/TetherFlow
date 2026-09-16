# Use PowerShell 7 so behavior matches the supported/tested runtime.
$ErrorActionPreference = 'Stop'
# Windows PowerShell 5.1 blocks scripts under the default Restricted policy and
# is untested here: hop into PowerShell 7 with an explicit policy override.
if ($PSVersionTable.PSVersion.Major -lt 7) {
    $pwsh = Get-Command pwsh.exe -ErrorAction SilentlyContinue
    if (!$pwsh) {
        Write-Host 'PowerShell 7 (pwsh) is required. Install it with: winget install Microsoft.PowerShell'
        exit 1
    }
    $relaunch = @('-NoProfile','-ExecutionPolicy','Bypass','-File',('"'+$PSCommandPath+'"'))
    $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($admin) { & $pwsh.Source @relaunch } else { Start-Process $pwsh.Source -Verb RunAs -WindowStyle Hidden -ArgumentList $relaunch }
    return
}
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (!$admin) {
    $pwsh = (Get-Command pwsh.exe -ErrorAction Stop).Source
    Start-Process $pwsh -Verb RunAs -WindowStyle Hidden -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-File',('"'+$PSCommandPath+'"'))
    return
}
$stateDir = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual'
New-Item -ItemType Directory -Force $stateDir | Out-Null
Start-Transcript -Path (Join-Path $stateDir 'launcher.log') -Force
try { & "$PSScriptRoot\Start-SteamDual.ps1" -Background }
catch { Write-Output ($_ | Out-String); exit 1 }
finally { Stop-Transcript }

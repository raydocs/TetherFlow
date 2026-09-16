# Remove the automatic SteamDual watchdog (scheduled task + processes).
$ErrorActionPreference = 'Stop'
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (!$admin) { throw 'Run from an elevated PowerShell.' }

Get-CimInstance Win32_Process -Filter "Name like 'powershell.exe' or Name like 'pwsh.exe'" |
    Where-Object { $_.CommandLine -like '*SteamDual.Watchdog.ps1*' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName 'TetherFlow SteamDual' -Confirm:$false

Write-Output 'Automatic mode removed; the core and watchdog are stopped.'
Write-Output 'The USB leak guard (if present) stays until .\Remove-UsbGuard.ps1 is run.'

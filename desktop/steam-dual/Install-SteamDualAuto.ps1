# One-time customer setup: register the SteamDual watchdog as a scheduled task.
# After this single elevated run, Steam dual-network mode starts automatically at
# logon whenever the TetherFlow phone is attached - no PowerShell, no prompts.
$ErrorActionPreference = 'Stop'
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (!$admin) { throw 'Run this once from an elevated PowerShell: powershell -NoProfile -ExecutionPolicy Bypass -File .\Install-SteamDualAuto.ps1' }
$watchdog = Join-Path $PSScriptRoot 'SteamDual.Watchdog.ps1'
if (!(Test-Path $watchdog)) { throw "Watchdog script not found: $watchdog" }

# PowerShell 7 preferred; Windows PowerShell 5.1 also works for these scripts.
$engine = Get-Command pwsh.exe -ErrorAction SilentlyContinue
$exe = if ($engine) { $engine.Source } else { "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" }
$action = New-ScheduledTaskAction -Execute $exe -Argument ('-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "' + $watchdog + '"')
$trigger = New-ScheduledTaskTrigger -AtLogOn
$trigger.Delay = 'PT30S'
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
$principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Highest
Register-ScheduledTask -TaskName 'TetherFlow SteamDual' -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Force | Out-Null
Start-ScheduledTask -TaskName 'TetherFlow SteamDual'

Write-Output 'Automatic Steam dual-network mode is installed.'
Write-Output ' - Starts ~30s after logon when the phone (TetherFlow) is attached; stops when unplugged.'
Write-Output ' - Logs: %LOCALAPPDATA%\TetherFlow\SteamDual\watchdog.log (plus core-out.log / core-err.log)'
Write-Output ' - Remove anytime: powershell -NoProfile -ExecutionPolicy Bypass -File .\Uninstall-SteamDualAuto.ps1'

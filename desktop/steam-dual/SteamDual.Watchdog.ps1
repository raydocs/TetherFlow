# Always-on automation loop: start the SteamDual core when the TetherFlow phone
# appears, stop it when the phone leaves. Runs hidden via the scheduled task
# registered by Install-SteamDualAuto.ps1; safe to run manually for testing.
$ErrorActionPreference = 'Continue'
Set-StrictMode -Version Latest
. "$PSScriptRoot\SteamDual.Common.ps1"

$mutex = New-Object System.Threading.Mutex($false, 'Global\TetherFlow-SteamDual-Watchdog')
$ownsDuty = $false
try { $ownsDuty = $mutex.WaitOne(0) }
catch [System.Threading.AbandonedMutexException] { $ownsDuty = $true }   # previous owner was killed; take over
if (!$ownsDuty) { return }

$stateDir = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual'
New-Item -ItemType Directory -Force $stateDir | Out-Null
$logPath = Join-Path $stateDir 'watchdog.log'
function Write-WatchLog([string]$Message) {
    if ((Test-Path $logPath) -and (Get-Item $logPath).Length -gt 512KB) { Remove-Item $logPath }
    "$([DateTime]::Now.ToString('s')) $Message" | Add-Content $logPath
}

Write-WatchLog 'watchdog started'
while ($true) {
    try {
        $phone = Test-SteamDualPhone
        $core = Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue
        if ($phone -and !$core) {
            Write-WatchLog 'phone detected and core is down; starting'
            $out = & "$PSScriptRoot\Start-SteamDual.ps1" -Background 2>&1
            Write-WatchLog ($out | Out-String).Trim()
        } elseif (!$phone -and $core) {
            Write-WatchLog 'phone gone; stopping core (Steam falls back to Wi-Fi)'
            $out = & "$PSScriptRoot\Stop-SteamDual.ps1" 2>&1
            Write-WatchLog ($out | Out-String).Trim()
        }
    } catch {
        Write-WatchLog "error: $($_.Exception.Message)"
    }
    Start-Sleep -Seconds 20
}

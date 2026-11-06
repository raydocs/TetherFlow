#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$HomeIP,
    [ValidateSet('Dual', 'HomeOnly', 'PhoneOnly')][string]$Mode = 'Dual',
    [string]$Executable = "$PSScriptRoot\tetherflow-steam-accelerator.exe",
    [string]$IProxy = '',
    [string]$DeviceUDID,
    [switch]$CheckPhoneOnly,
    [switch]$UseExistingTunnel,
    [switch]$HttpOnly,
    [int]$HomeWeight = 1,
    [int]$PhoneWeight = 4,
    [string]$AllowedHosts
)
$ErrorActionPreference = 'Stop'
$uplinkMode = @{ Dual = 'dual'; HomeOnly = 'home'; PhoneOnly = 'phone' }[$Mode]
$usePhone = $Mode -ne 'HomeOnly'
if (-not $usePhone -and ($CheckPhoneOnly -or $IProxy -or $UseExistingTunnel -or $DeviceUDID)) {
    throw '-Mode HomeOnly does not use the iPhone: remove -CheckPhoneOnly, -IProxy, -UseExistingTunnel and -DeviceUDID.'
}
if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) { throw "Missing executable: $Executable" }
if (-not $CheckPhoneOnly) {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Open PowerShell as administrator, then run this script. It installs a temporary Steam discovery entry; no system proxy is changed.'
    }
    # Read-only checks, so USB is not needed in home mode. Exit 2 means a FAIL line above: stop.
    $doctorArgs = @('--doctor', '--uplink-mode', $uplinkMode)
    if ($UseExistingTunnel -or $IProxy) { $doctorArgs += '--external-tunnel' }
    if ($DeviceUDID -and -not ($UseExistingTunnel -or $IProxy)) { $doctorArgs += @('--device-udid', $DeviceUDID) }
    if ($HttpOnly) { $doctorArgs += '--http-only' }
    if ($HomeIP) { $doctorArgs += @('--home-ip', $HomeIP) }
    & $Executable @doctorArgs
    if ($LASTEXITCODE -ne 0) { throw "Doctor reported a FAIL (exit code $LASTEXITCODE). Fix the FAIL line above; nothing was changed." }
    if (-not $HomeIP) {
        $HomeIP = Read-Host 'Home Wi-Fi IPv4 from the list above (not iPhone Ethernet)'
        $doctorArgs += @('--home-ip', $HomeIP)
        & $Executable @doctorArgs
        if ($LASTEXITCODE -ne 0) { throw "Doctor reported a FAIL (exit code $LASTEXITCODE). Fix the FAIL line above; nothing was changed." }
    }
}
$tunnel = $null
try {
    if ($usePhone -and $IProxy -and -not $UseExistingTunnel) {
        $iproxyCommand = Get-Command $IProxy -ErrorAction Stop
        $iproxyArgs = @('-l', '-s', '127.0.0.1')
        if ($DeviceUDID) {
            if ($DeviceUDID -notmatch '^[A-Za-z0-9-]+$') { throw 'Invalid device UDID.' }
            $iproxyArgs += @('-u', $DeviceUDID)
        }
        $iproxyArgs += '8283:8283'
        $tunnel = Start-Process -FilePath $iproxyCommand.Source -ArgumentList $iproxyArgs -PassThru -NoNewWindow
        Start-Sleep -Seconds 1
        if ($tunnel.HasExited) { throw 'iproxy exited. Check Apple Devices, pairing, one attached iPhone, and iproxy --help syntax.' }
    }
    if ($usePhone) {
        Write-Host 'On iPhone: open TetherFlow, tap Start, keep it foreground. The accelerator asks for the code below, with hidden input.'
    } else {
        Write-Host 'Home-only mode: the iPhone, USB and the session code are not used.'
    }
    $cliArgs = @()
    if ($UseExistingTunnel -or $IProxy) { $cliArgs += '--external-tunnel' }
    if ($DeviceUDID -and -not ($UseExistingTunnel -or $IProxy)) { $cliArgs += @('--device-udid', $DeviceUDID) }
    if ($CheckPhoneOnly) {
        $cliArgs += '--check-phone'
    } else {
        # One report file per mode, so a home-only baseline never overwrites a dual run.
        $cliArgs += @('--uplink-mode', $uplinkMode, '--home-ip', $HomeIP, '--enable-steam', '--home-weight', "$HomeWeight", '--phone-weight', "$PhoneWeight", '--report', "$PSScriptRoot\tetherflow-status-$uplinkMode.json")
        if ($HttpOnly) { $cliArgs += '--http-only' }
        if ($AllowedHosts) { $cliArgs += @('--allow-hosts', $AllowedHosts) }
        Write-Host 'After startup succeeds, fully exit and reopen Steam once. Download a game and watch the path counters and concurrent_seconds in the report.'
        Write-Host 'No relay traffic means Steam is NOT using this path. Do not mistake configuration for acceleration.'
        Write-Host 'Stop with Ctrl+C. The launcher/guardian removes only its own Steam discovery entry.'
    }
    & $Executable @cliArgs
    if ($LASTEXITCODE -ne 0) { throw "Accelerator failed with exit code $LASTEXITCODE. Read the error above; no speed claim is implied." }
} finally {
    if ($tunnel -and -not $tunnel.HasExited) { Stop-Process -Id $tunnel.Id -ErrorAction SilentlyContinue }
}

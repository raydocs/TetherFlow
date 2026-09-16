[CmdletBinding()]
param(
    [string]$HomeInterface = 'Wi-Fi',
    [string]$PhoneInterface = '',
    [string]$PhoneAddress = '',
    [ValidateRange(1,65535)][int]$PhonePort = 8282,
    [int]$PhoneWeight = 5,
    [int]$HomeWeight = 1,
    [string[]]$ProcessNames = @('steam.exe'),
    [switch]$NoAdbTunnel,
    [switch]$PrepareOnly,
    [switch]$ProxyTest,
    [switch]$Background
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. "$PSScriptRoot\SteamDual.Common.ps1"

# -File callers pass "a.exe,b.exe" as one string; normalize to a real array.
$ProcessNames = @($ProcessNames | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ })

$stateDir = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual'
New-Item -ItemType Directory -Force $stateDir | Out-Null
$core = Install-SteamDualCore $stateDir
$homeAdapter = Get-NetAdapter -Name $HomeInterface
if ($homeAdapter.Status -ne 'Up') { throw 'Home interface is not connected.' }

# Phone egress: prefer the ADB USB tunnel (loopback, immune to the RNDIS
# single-flow cap); fall back to the USB-tethering gateway address.
$adbTunnel = $false
if (!$PSBoundParameters.ContainsKey('PhoneAddress') -and !$NoAdbTunnel) {
    try { Install-Adb | Out-Null } catch { Write-Host "[!] platform-tools install failed: $($_.Exception.Message)" }
    if (Find-Adb) {
        try {
            Enable-AdbTunnel $PhonePort $PhonePort | Out-Null
            $PhoneAddress = '127.0.0.1'
            $adbTunnel = $true
            Write-Host "[+] ADB USB tunnel active: phone proxy at 127.0.0.1:$PhonePort (bypasses the RNDIS per-flow cap)."
        } catch {
            Write-Host "[!] ADB tunnel unavailable: $($_.Exception.Message)"
            Write-Host '[!] Falling back to the USB tethering (RNDIS) path.'
        }
    }
}

# The USB-tethering adapter: mandatory for the direct path, optional with ADB.
if (!$PhoneInterface) {
    $phones = @(Get-NetAdapter | Where-Object { $_.Status -eq 'Up' -and $_.InterfaceDescription -match 'RNDIS|Remote NDIS|USB.*Ethernet|NCM' })
    if ($phones.Count -gt 1) { throw 'Specify -PhoneInterface explicitly; USB adapter detection was ambiguous.' }
    if ($phones.Count -eq 1) { $PhoneInterface = $phones[0].Name }
}
if (!$adbTunnel) {
    if (!$PhoneInterface) { throw 'No USB tethering adapter is up. Enable USB tethering, or authorize ADB to use the tunnel path.' }
    $phone = Get-NetAdapter -Name $PhoneInterface
    if ($phone.Status -ne 'Up' -or $phone.ifIndex -eq $homeAdapter.ifIndex) { throw 'Two different connected adapters are required.' }
    if (!$PhoneAddress) {
        $gateways = @(Get-NetRoute -InterfaceIndex $phone.ifIndex -DestinationPrefix '0.0.0.0/0' | Select-Object -ExpandProperty NextHop -Unique)
        if ($gateways.Count -ne 1) { throw 'Specify -PhoneAddress explicitly.' }
        $PhoneAddress = $gateways[0]
    }
}
$ip = [System.Net.IPAddress]::Parse($PhoneAddress)
if ($ip.AddressFamily -ne [System.Net.Sockets.AddressFamily]::InterNetwork) { throw 'PhoneAddress must be the USB IPv4 gateway of the phone, or 127.0.0.1 with the ADB tunnel.' }
# Probe without inheriting system PAC/proxy settings. Old APKs only support /pac.
$probe = [System.Net.WebRequest]::Create("http://${PhoneAddress}:${PhonePort}/pac")
$probe.Proxy = $null
$probe.Timeout = 5000
$response = $probe.GetResponse()
$response.Close()

$config = New-SteamDualConfig $HomeInterface $PhoneInterface $PhoneAddress $PhonePort -PhoneWeight $PhoneWeight -HomeWeight $HomeWeight -ProcessNames $ProcessNames -ProxyTest:$ProxyTest
$configPath = Join-Path $stateDir $(if ($ProxyTest) {'proxy-test.json'} else {'config.json'})
$config | ConvertTo-Json -Depth 20 | Set-Content -Encoding UTF8 $configPath
& $core -t -d $stateDir -f $configPath
if ($LASTEXITCODE -ne 0) { throw 'Core rejected generated configuration.' }
if ($PrepareOnly) { Write-Output "Prepared: $configPath"; return }
if (Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue) { throw 'SteamDual is already running. Stop it before starting another instance.' }
if (!$ProxyTest) {
    $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (!$admin) { throw 'TUN and the USB leak guard require an Administrator PowerShell. Configuration has been prepared; rerun this command as Administrator.' }
    if ($PhoneInterface) {
        # Persist the guard deliberately: a crash, core exit or unplug must never
        # silently restore ordinary USB tethering for a resumed Steam download.
        Enable-SteamDualGuard $PhoneInterface $PhoneAddress
        Prefer-SteamDualHomeRoute $HomeInterface $PhoneInterface $stateDir
    } else {
        Write-Host '[!] No USB-tethering adapter present; the firewall guard and route adjustments were skipped (ADB-tunnel-only mode).'
    }
}
Write-Host 'Pause/resume Steam after TUN starts, so existing connections are replaced.'
Write-Host 'Ctrl+C stops the core. The USB leak guard remains until explicitly removed.'
Write-Host "Live counters: pwsh -File `"$PSScriptRoot\Get-SteamDualStatus.ps1`""
if ($Background) {
    $child = Start-Process -FilePath $core -ArgumentList @('-d', ('"'+$stateDir+'"'), '-f', ('"'+$configPath+'"')) -WindowStyle Hidden -RedirectStandardOutput (Join-Path $stateDir 'core-out.log') -RedirectStandardError (Join-Path $stateDir 'core-err.log') -PassThru
    Start-Sleep -Seconds 3
    if ($child.HasExited) { throw 'Core failed to start. Inspect core-out.log and core-err.log. USB guard remains enabled.' }
    Write-Output "Core running as PID $($child.Id). Use Stop-SteamDual.ps1 as Administrator to stop it."
    return
}
& $core -d $stateDir -f $configPath
if ($LASTEXITCODE -ne 0) { throw "Core exited with code $LASTEXITCODE. USB guard remains enabled." }


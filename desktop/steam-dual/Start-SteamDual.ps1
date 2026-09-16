[CmdletBinding()]
param(
    [string]$HomeInterface = 'Wi-Fi',
    [string]$PhoneInterface = '',
    [string]$PhoneAddress = '',
    [ValidateRange(1,65535)][int]$PhonePort = 8282,
    [switch]$PrepareOnly,
    [switch]$ProxyTest,
    [switch]$Background
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. "$PSScriptRoot\SteamDual.Common.ps1"

$stateDir = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual'
New-Item -ItemType Directory -Force $stateDir | Out-Null
$core = Install-SteamDualCore $stateDir
$homeAdapter = Get-NetAdapter -Name $HomeInterface
if ($homeAdapter.Status -ne 'Up') { throw 'Home interface is not connected.' }
if (!$PhoneInterface) {
    $phones = @(Get-NetAdapter | Where-Object { $_.Status -eq 'Up' -and $_.InterfaceDescription -match 'RNDIS|Remote NDIS|USB.*Ethernet|NCM' })
    if ($phones.Count -ne 1) { throw 'Specify -PhoneInterface explicitly; USB adapter detection was ambiguous.' }
    $PhoneInterface = $phones[0].Name
}
$phone = Get-NetAdapter -Name $PhoneInterface
if ($phone.Status -ne 'Up' -or $phone.ifIndex -eq $homeAdapter.ifIndex) { throw 'Two different connected adapters are required.' }
if (!$PhoneAddress) {
    $gateways = @(Get-NetRoute -InterfaceIndex $phone.ifIndex -DestinationPrefix '0.0.0.0/0' | Select-Object -ExpandProperty NextHop -Unique)
    if ($gateways.Count -ne 1) { throw 'Specify -PhoneAddress explicitly.' }
    $PhoneAddress = $gateways[0]
}
$ip = [System.Net.IPAddress]::Parse($PhoneAddress)
if ($ip.AddressFamily -ne [System.Net.Sockets.AddressFamily]::InterNetwork -or [System.Net.IPAddress]::IsLoopback($ip)) { throw 'PhoneAddress must be the USB IPv4 address of the phone.' }
# Probe without inheriting system PAC/proxy settings. Old APKs only support /pac.
$probe = [System.Net.WebRequest]::Create("http://${PhoneAddress}:${PhonePort}/pac")
$probe.Proxy = $null
$probe.Timeout = 5000
$response = $probe.GetResponse()
$response.Close()

$config = New-SteamDualConfig $HomeInterface $PhoneInterface $PhoneAddress $PhonePort -ProxyTest:$ProxyTest
$configPath = Join-Path $stateDir $(if ($ProxyTest) {'proxy-test.json'} else {'config.json'})
$config | ConvertTo-Json -Depth 20 | Set-Content -Encoding UTF8 $configPath
& $core -t -d $stateDir -f $configPath
if ($LASTEXITCODE -ne 0) { throw 'Core rejected generated configuration.' }
if ($PrepareOnly) { Write-Output "Prepared: $configPath"; return }
if (Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue) { throw 'SteamDual is already running. Stop it before starting another instance.' }
if (!$ProxyTest) {
    $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if (!$admin) { throw 'TUN and the USB leak guard require an Administrator PowerShell. Configuration has been prepared; rerun this command as Administrator.' }
    # Persist the guard deliberately: a crash, core exit or unplug must never
    # silently restore ordinary USB tethering for a resumed Steam download.
    Enable-SteamDualGuard $PhoneInterface $PhoneAddress
    Prefer-SteamDualHomeRoute $HomeInterface $PhoneInterface $stateDir
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


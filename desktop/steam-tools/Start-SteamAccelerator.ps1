#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$HomeIP,
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
if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) { throw "Missing executable: $Executable" }
if (-not $CheckPhoneOnly) {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Open PowerShell as administrator, then run this script. It installs a temporary Steam discovery entry; no system proxy is changed.'
    }
    & $Executable --doctor
    if ($LASTEXITCODE -ne 0) { throw 'Diagnostics failed.' }
    if (-not $HomeIP) { $HomeIP = Read-Host 'Home Wi-Fi IPv4 from the list above (not iPhone Ethernet)' }
}
$tunnel = $null
$tokenPath = $null
try {
    if ($IProxy -and -not $UseExistingTunnel) {
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
    Write-Host 'On iPhone: open TetherFlow, tap Start, keep it foreground. Enter its session token privately below.'
    $secure = Read-Host 'iPhone session token (hidden)' -AsSecureString
    # Create empty file, restrict ACL, then write secret. No token in command-line or transcript.
    $tokenPath = Join-Path ([IO.Path]::GetTempPath()) ('tetherflow-' + [Guid]::NewGuid().ToString('N') + '.token')
    $stream = [IO.File]::Open($tokenPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    $stream.Dispose()
    $acl = New-Object Security.AccessControl.FileSecurity
    $acl.SetAccessRuleProtection($true, $false)
    $rule = New-Object Security.AccessControl.FileSystemAccessRule([Security.Principal.WindowsIdentity]::GetCurrent().User, 'FullControl', 'Allow')
    $acl.AddAccessRule($rule)
    Set-Acl -LiteralPath $tokenPath -AclObject $acl
    $ptr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try {
        $plain = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr)
        [IO.File]::WriteAllText($tokenPath, $plain, (New-Object Text.UTF8Encoding($false)))
    } finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr)
        $plain = $null
        $secure.Dispose()
    }
    $cliArgs = @('--phone-token-file', $tokenPath)
    if ($UseExistingTunnel -or $IProxy) { $cliArgs += '--external-tunnel' }
    if ($DeviceUDID -and -not ($UseExistingTunnel -or $IProxy)) { $cliArgs += @('--device-udid', $DeviceUDID) }
    if ($CheckPhoneOnly) {
        $cliArgs += '--check-phone'
    } else {
        $cliArgs += @('--home-ip', $HomeIP, '--enable-steam', '--home-weight', "$HomeWeight", '--phone-weight', "$PhoneWeight", '--report', "$PSScriptRoot\tetherflow-status.json")
        if ($HttpOnly) { $cliArgs += '--http-only' }
        if ($AllowedHosts) { $cliArgs += @('--allow-hosts', $AllowedHosts) }
        Write-Host 'After startup succeeds, fully exit and reopen Steam once. Download a game and watch both path counters.'
        Write-Host 'No relay traffic means Steam is NOT using this path. Do not mistake configuration for acceleration.'
        Write-Host 'Stop with Ctrl+C. The launcher/guardian removes only its own Steam discovery entry.'
    }
    & $Executable @cliArgs
    if ($LASTEXITCODE -ne 0) { throw "Accelerator failed with exit code $LASTEXITCODE. Read the error above; no speed claim is implied." }
} finally {
    if ($tokenPath -and (Test-Path -LiteralPath $tokenPath)) { Remove-Item -LiteralPath $tokenPath -Force }
    if ($tunnel -and -not $tunnel.HasExited) { Stop-Process -Id $tunnel.Id -ErrorAction SilentlyContinue }
}

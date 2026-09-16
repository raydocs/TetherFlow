# Run explicitly as Administrator only when ordinary USB tethering is wanted.
$ErrorActionPreference = 'Stop'
if (Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue) { throw 'Stop SteamDual before removing its USB leak guard.' }
$backupPath = Join-Path $env:LOCALAPPDATA 'TetherFlow\SteamDual\usb-route-backup.json'
if (Test-Path $backupPath) {
    $backup = Get-Content $backupPath -Raw | ConvertFrom-Json
    Set-NetIPInterface -InterfaceAlias $backup.InterfaceAlias -AddressFamily IPv4 -InterfaceMetric $backup.InterfaceMetric -AutomaticMetric $backup.AutomaticMetric
    Remove-Item -LiteralPath $backupPath
}
Remove-NetFirewallRule -Name 'TetherFlow-SteamDual-USB-Guard'
Write-Host 'Ordinary USB tethering is allowed again.'

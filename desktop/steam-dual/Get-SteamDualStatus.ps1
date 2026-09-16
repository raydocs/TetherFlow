param([switch]$ProxyTest)
$ErrorActionPreference = 'Stop'
$configPath = Join-Path $env:LOCALAPPDATA ('TetherFlow\SteamDual\' + $(if ($ProxyTest) {'proxy-test.json'} else {'config.json'}))
$config = Get-Content $configPath -Raw | ConvertFrom-Json
$headers = @{Authorization="Bearer $($config.secret)"}
$result = Invoke-RestMethod 'http://127.0.0.1:17892/connections' -Headers $headers
$result.connections | Select-Object @{n='Process';e={$_.metadata.process}},@{n='Destination';e={"$($_.metadata.host) $($_.metadata.destinationIP):$($_.metadata.destinationPort)"}},@{n='Path';e={$_.chains -join ' -> '}},download,upload | Format-Table -AutoSize
Write-Host 'Byte counters cover currently open connections, not historical totals.'
Get-NetFirewallRule -Name 'TetherFlow-SteamDual-USB-Guard' -ErrorAction SilentlyContinue | Select-Object DisplayName,Enabled,Action

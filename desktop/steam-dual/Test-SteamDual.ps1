$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. "$PSScriptRoot\SteamDual.Common.ps1"
function Assert($Condition, [string]$Message) { if (!$Condition) { throw $Message } }

# Weighted egress expansion (default 4 phone : 1 home).
$cfg = New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282
Assert ($cfg.tun.enable -eq $true) 'Production must intercept Steam with TUN.'
Assert ($cfg.proxies.Count -eq 5) 'Default weights must expand to five egress nodes.'
Assert ($cfg.proxies[0]['name'] -eq 'PHONE-8282' -and $cfg.proxies[0].type -eq 'http' -and $cfg.proxies[0].port -eq 8282) 'Phone must use the HTTP proxy.'
Assert ($cfg.proxies[0]['interface-name'] -eq 'USB test') 'Phone must bind USB.'
Assert ($cfg.proxies[3]['name'] -eq 'PHONE-8282-r4' -and $cfg.proxies[3].server -eq '10.81.25.145' -and $cfg.proxies[3]['interface-name'] -eq 'USB test') 'Phone replicas must reuse the endpoint and bind USB.'
Assert ($cfg.proxies[4]['name'] -eq 'HOME-WIFI' -and $cfg.proxies[4].type -eq 'direct' -and $cfg.proxies[4]['interface-name'] -eq 'Home test') 'Home must bind an interface.'
Assert ($cfg.rules[-1] -eq 'MATCH,HOME-WIFI') 'No unbound DIRECT fallback is permitted.'
Assert ($cfg['proxy-groups'][0].strategy -eq 'round-robin') 'Connections must be distributed.'
Assert ($cfg['proxy-groups'][0].proxies.Count -eq 5) 'The balance group must contain every replica.'
Assert ($cfg['proxy-groups'][0].proxies[0] -eq 'PHONE-8282' -and $cfg['proxy-groups'][0].proxies[4] -eq 'HOME-WIFI') 'Phone replicas must precede home in the rotation.'
Assert ($cfg.rules[0].Contains('PROCESS-NAME,steam.exe') -and $cfg.rules[0].Contains('NETWORK,TCP')) 'Only Steam HTTP(S) TCP should be balanced.'
$testCfg = New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282 -ProxyTest
Assert (!$testCfg.tun.enable) 'Proxy tests must not modify routing.'

# Multi-process capture (other big downloaders besides Steam).
$multi = New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282 -ProcessNames @('steam.exe','egsbootstrap.exe')
Assert ($multi.rules[0] -eq 'AND,((OR,(PROCESS-NAME,steam.exe),(PROCESS-NAME,egsbootstrap.exe)),(NETWORK,TCP),(OR,((DST-PORT,80),(DST-PORT,443)))),STEAM-DUAL') 'Multiple processes must combine into one OR block.'
$rejected = $false
try { New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282 -ProcessNames @('Steam.EXE') } catch { $rejected = $true }
Assert $rejected 'Process names must be lowercase .exe names.'

# ADB-tunnel (loopback) mode: custom weights and no interface binding.
$loop = New-SteamDualConfig 'Home test' '' '127.0.0.1' 8282 -PhoneWeight 2 -HomeWeight 1
Assert ($loop.proxies.Count -eq 3) 'Explicit weights must control replica counts.'
Assert ($loop.proxies[0].server -eq '127.0.0.1' -and !$loop.proxies[0].Contains('interface-name')) 'Loopback phone nodes must not bind an interface.'
Assert ($loop.proxies[1].server -eq '127.0.0.1' -and !$loop.proxies[1].Contains('interface-name')) 'Loopback replicas must stay unbound.'
Assert ($loop.proxies[2]['name'] -eq 'HOME-WIFI' -and $loop.proxies[2]['interface-name'] -eq 'Home test') 'Home still binds its interface in tunnel mode.'
Assert ($loop.tun.'route-exclude-address' -contains '127.0.0.1/32') 'Loopback must stay outside the TUN routes.'
$rejected = $false
try { New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282 -PhoneWeight 0 } catch { $rejected = $true }
Assert $rejected 'Non-positive weights must be rejected.'
$rejected = $false
try { New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282 -PhoneWeight 9 -HomeWeight 9 } catch { $rejected = $true }
Assert $rejected 'Weight sums above sixteen must be rejected.'

# Exercise actual guard address construction without changing Windows Firewall.
$script:captured = $null
function Get-NetFirewallProfile { [pscustomobject]@{Enabled=$true} }
function Get-NetFirewallRule { return $null }
function New-NetFirewallRule { param($Name,$DisplayName,$InterfaceAlias,$RemoteAddress,$Direction,$Action,$Profile)
    $script:captured = $PSBoundParameters
}
Enable-SteamDualGuard 'USB test' '10.81.25.145'
Assert ($script:captured.InterfaceAlias -eq 'USB test') 'Guard must only target USB.'
Assert ($script:captured.Action -eq 'Block') 'Guard must block.'
Assert (($script:captured.RemoteAddress -join ',') -eq '0.0.0.1-10.81.25.144,10.81.25.146-255.255.255.254,::/1,8000::/1') 'Guard must block IPv4/IPv6 internet while allowing the phone address.'
Enable-SteamDualGuard 'USB test' '127.0.0.1'
Assert (($script:captured.RemoteAddress -join ',') -eq '0.0.0.1-255.255.255.254,::/1,8000::/1') 'ADB-tunnel guard must block the whole internet on the USB adapter.'
function Get-NetFirewallProfile { [pscustomobject]@{Enabled=$false} }
$rejected = $false
try { Enable-SteamDualGuard 'USB test' '10.81.25.145' } catch { $rejected = $true }
Assert $rejected 'Disabled firewall must prevent startup.'

# Find-Adb must ignore a nonexistent ADB_PATH instead of returning it.
$env:ADB_PATH = Join-Path $env:TEMP ('no-such-adb-' + [guid]::NewGuid().ToString('N') + '.exe')
try {
    Assert ((Find-Adb) -ne $env:ADB_PATH) 'Find-Adb must ignore nonexistent ADB_PATH entries.'
} finally { Remove-Item Env:\ADB_PATH }
Write-Output 'PASS: weighted egress, ADB-loopback config, guard ranges (tether and tunnel modes), weight validation, adb discovery.'

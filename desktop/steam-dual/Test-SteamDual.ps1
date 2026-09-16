$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. "$PSScriptRoot\SteamDual.Common.ps1"
function Assert($Condition, [string]$Message) { if (!$Condition) { throw $Message } }
$cfg = New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282
Assert ($cfg.tun.enable -eq $true) 'Production must intercept Steam with TUN.'
Assert ($cfg.proxies.Count -eq 2) 'Unexpected egress path.'
Assert ($cfg.proxies[0]['interface-name'] -eq 'Home test') 'Home must bind an interface.'
Assert ($cfg.proxies[1].type -eq 'http' -and $cfg.proxies[1].port -eq 8282) 'Phone must use the HTTP proxy.'
Assert ($cfg.proxies[1]['interface-name'] -eq 'USB test') 'Phone must bind USB.'
Assert ($cfg.rules[-1] -eq 'MATCH,HOME-WIFI') 'No unbound DIRECT fallback is permitted.'
Assert ($cfg['proxy-groups'][0].strategy -eq 'round-robin') 'Connections must be distributed.'
Assert ($cfg['proxy-groups'][0].proxies.Count -eq 2) 'No hidden fallback node.'
Assert ($cfg.rules[0].Contains('PROCESS-NAME,steam.exe') -and $cfg.rules[0].Contains('NETWORK,TCP')) 'Only Steam HTTP(S) TCP should be balanced.'
$testCfg = New-SteamDualConfig 'Home test' 'USB test' '10.81.25.145' 8282 -ProxyTest
Assert (!$testCfg.tun.enable) 'Proxy tests must not modify routing.'

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
function Get-NetFirewallProfile { [pscustomobject]@{Enabled=$false} }
$rejected = $false
try { Enable-SteamDualGuard 'USB test' '10.81.25.145' } catch { $rejected = $true }
Assert $rejected 'Disabled firewall must prevent startup.'
Write-Output 'PASS: interface binding, proxy-only phone path, TUN scope, guard ranges and disabled-firewall rejection.'



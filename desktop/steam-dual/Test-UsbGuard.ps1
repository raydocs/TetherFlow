param([string]$PhoneInterface='Ethernet 3', [string]$PhoneAddress='10.81.25.145', [int]$PhonePort=8282)
$ErrorActionPreference='Stop'
$rule = Get-NetFirewallRule -Name 'TetherFlow-SteamDual-USB-Guard'
if ($rule.Enabled -ne 'True' -or $rule.Action -ne 'Block') { throw 'USB guard is not enabled.' }
$adapter = Get-NetAdapter -Name $PhoneInterface
$sourceIP = @(Get-NetIPAddress -InterfaceIndex $adapter.ifIndex -AddressFamily IPv4 | Where-Object {$_.AddressState -eq 'Preferred'})[0].IPAddress
function Test-PinnedTCP([string]$Target, [int]$Port) {
    $socket = [System.Net.Sockets.Socket]::new([System.Net.Sockets.AddressFamily]::InterNetwork,[System.Net.Sockets.SocketType]::Stream,[System.Net.Sockets.ProtocolType]::Tcp)
    try {
        # IP_UNICAST_IF (31) prevents the TUN/default route from carrying this test.
        $socket.SetSocketOption([System.Net.Sockets.SocketOptionLevel]::IP,[System.Net.Sockets.SocketOptionName]31,[System.Net.IPAddress]::HostToNetworkOrder([int]$adapter.ifIndex))
        $socket.Bind([System.Net.IPEndPoint]::new([System.Net.IPAddress]::Parse($sourceIP),0))
        $task = $socket.ConnectAsync([System.Net.IPAddress]::Parse($Target),$Port)
        try { return ($task.Wait(4000) -and $socket.Connected) } catch { return $false }
    } finally { $socket.Dispose() }
}
if (!(Test-PinnedTCP $PhoneAddress $PhonePort)) { throw 'Phone proxy is not reachable through the guarded USB interface.' }
if (Test-PinnedTCP '1.1.1.1' 443) { throw 'LEAK: direct USB internet is still reachable.' }
Write-Output 'PASS: phone proxy reachable; ordinary USB TCP internet blocked with socket pinned to USB.'

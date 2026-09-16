function Install-SteamDualCore([string]$Directory) {
    $version = 'v1.19.31'
    $sha256 = '93d14e9a13b49b2f2d256202d02cc8d14a7c4695edf084cae0f941986bc9c218'
    $archive = Join-Path $Directory "mihomo-$version.zip"
    $target = Join-Path $Directory 'tetherflow-steam-core.exe'
    if (!(Test-Path $archive) -or (Get-FileHash $archive -Algorithm SHA256).Hash -ne $sha256) {
        Invoke-WebRequest "https://github.com/MetaCubeX/mihomo/releases/download/$version/mihomo-windows-amd64-compatible-$version.zip" -OutFile $archive -UseBasicParsing
    }
    if ((Get-FileHash $archive -Algorithm SHA256).Hash -ne $sha256) { throw 'Core archive checksum mismatch.' }
    if (Get-Process tetherflow-steam-core -ErrorAction SilentlyContinue) { throw 'Stop the running SteamDual core before preparing another configuration.' }
    $expanded = Join-Path $Directory $version
    Expand-Archive -LiteralPath $archive -DestinationPath $expanded -Force
    $executables = @(Get-ChildItem $expanded -Filter '*.exe')
    if ($executables.Count -ne 1) { throw 'Unexpected core archive layout.' }
    Copy-Item $executables[0].FullName $target -Force
    return $target
}

function New-SteamDualConfig([string]$HomeInterface, [string]$PhoneInterface, [string]$PhoneAddress, [int]$PhonePort, [switch]$ProxyTest) {
    $rules = @(
        'AND,((PROCESS-NAME,steam.exe),(NETWORK,TCP),(OR,((DST-PORT,80),(DST-PORT,443)))),STEAM-DUAL',
        'MATCH,HOME-WIFI'
    )
    if ($ProxyTest) { $rules = @('MATCH,STEAM-DUAL') }
    return [ordered]@{
        'mixed-port' = 17890
        'allow-lan' = $false
        'bind-address' = '127.0.0.1'
        mode = 'rule'
        'log-level' = 'info'
        ipv6 = $false
        'find-process-mode' = 'always'
        'interface-name' = $HomeInterface
        'external-controller' = '127.0.0.1:17892'
        secret = [guid]::NewGuid().ToString('N')
        tun = @{
            enable = !$ProxyTest
            device = 'TetherFlowSteam'
            stack = 'mixed'
            'auto-route' = $true
            'auto-detect-interface' = $false
            'strict-route' = $true
            'dns-hijack' = @('any:53','tcp://any:53')
            'route-exclude-address' = @("$PhoneAddress/32")
        }
        dns = @{
            enable = $true
            listen = '127.0.0.1:17893'
            ipv6 = $false
            'enhanced-mode' = 'fake-ip'
            'fake-ip-range' = '198.18.0.1/16'
            'default-nameserver' = @('1.1.1.1','8.8.8.8')
            nameserver = @('https://1.1.1.1/dns-query','https://8.8.8.8/dns-query')
        }
        proxies = @(
            @{ name='HOME-WIFI'; type='direct'; 'interface-name'=$HomeInterface },
            @{ name='PHONE-8282'; type='http'; server=$PhoneAddress; port=$PhonePort; 'interface-name'=$PhoneInterface }
        )
        'proxy-groups' = @(@{
            name='STEAM-DUAL'; type='load-balance'; strategy='round-robin'
            proxies=@('HOME-WIFI','PHONE-8282')
            url='https://www.gstatic.com/generate_204'; interval=30; lazy=$false
        })
        rules = $rules
    }
}

function Enable-SteamDualGuard([string]$PhoneInterface, [string]$PhoneAddress) {
    # Windows block rules override allows. Block every remote IP except the
    # phone itself, rather than an all-IP block plus a nonfunctional allow rule.
    $bytes = [System.Net.IPAddress]::Parse($PhoneAddress).GetAddressBytes()
    [array]::Reverse($bytes)
    $value = [BitConverter]::ToUInt32($bytes,0)
    if ($value -eq 0 -or $value -eq [uint32]::MaxValue) { throw 'Invalid phone address.' }
    function Convert-IP([uint32]$n) {
        $b = [BitConverter]::GetBytes($n); [array]::Reverse($b)
        return ([System.Net.IPAddress]::new($b)).ToString()
    }
    # Windows rejects the unspecified/broadcast endpoints and IPv6 /0 here.
    $ranges = @("0.0.0.1-$(Convert-IP ($value-1))", "$(Convert-IP ($value+1))-255.255.255.254", '::/1', '8000::/1')
    $ruleName = 'TetherFlow-SteamDual-USB-Guard'
    # Fail before starting TUN if Windows Firewall cannot enforce this guard.
    if (@(Get-NetFirewallProfile | Where-Object { !$_.Enabled }).Count) { throw 'All Windows Firewall profiles must be enabled for the USB leak guard.' }
    $old = Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue
    if ($old) {
        Set-NetFirewallRule -Name $ruleName -InterfaceAlias $PhoneInterface -RemoteAddress $ranges -Enabled True -Action Block -Direction Outbound -Profile Any | Out-Null
    } else {
        New-NetFirewallRule -Name $ruleName -DisplayName 'TetherFlow: block ordinary USB internet (keep phone proxy reachable)' -InterfaceAlias $PhoneInterface -RemoteAddress $ranges -Direction Outbound -Action Block -Profile Any | Out-Null
    }
}

function Prefer-SteamDualHomeRoute([string]$HomeInterface, [string]$PhoneInterface, [string]$StateDirectory) {
    $homeIP = Get-NetIPInterface -InterfaceAlias $HomeInterface -AddressFamily IPv4
    $phoneIP = Get-NetIPInterface -InterfaceAlias $PhoneInterface -AddressFamily IPv4
    $homeRoutes = @(Get-NetRoute -InterfaceAlias $HomeInterface -DestinationPrefix '0.0.0.0/0')
    if (!$homeRoutes.Count) { throw 'Home interface has no default route.' }
    $homeCost = $homeIP.InterfaceMetric + ($homeRoutes | Measure-Object RouteMetric -Minimum).Minimum
    $phoneMetric = [int]$homeCost + 100
    if ($phoneMetric -gt 9999) { throw 'Home routing metric is too high; configure home routing manually.' }
    $backupPath = Join-Path $StateDirectory 'usb-route-backup.json'
    if (Test-Path $backupPath) {
        $backup = Get-Content $backupPath -Raw | ConvertFrom-Json
        if ($backup.InterfaceAlias -ne $PhoneInterface) { throw 'A previous USB adapter has a saved route setting. Restore it before switching adapters.' }
    } else {
        @{ InterfaceAlias=$PhoneInterface; InterfaceMetric=$phoneIP.InterfaceMetric; AutomaticMetric=[string]$phoneIP.AutomaticMetric } | ConvertTo-Json | Set-Content $backupPath
    }
    # Keep normal Wi-Fi usable when the TUN core stops; the USB guard persists.
    Set-NetIPInterface -InterfaceAlias $PhoneInterface -AddressFamily IPv4 -AutomaticMetric Disabled -InterfaceMetric $phoneMetric
}



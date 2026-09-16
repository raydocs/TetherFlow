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

function New-SteamDualConfig([string]$HomeInterface, [string]$PhoneInterface, [string]$PhoneAddress, [int]$PhonePort, [int]$PhoneWeight = 5, [int]$HomeWeight = 1, [string[]]$ProcessNames = @('steam.exe'), [switch]$ProxyTest) {
    if ($PhoneWeight -lt 1 -or $HomeWeight -lt 1 -or ($PhoneWeight + $HomeWeight) -gt 16) {
        throw 'PhoneWeight and HomeWeight must be >= 1 and sum to at most 16.'
    }
    if (!$ProcessNames -or $ProcessNames.Count -lt 1 -or $ProcessNames.Count -gt 8) {
        throw 'ProcessNames must contain between 1 and 8 executable names.'
    }
    foreach ($p in $ProcessNames) {
        if ($p -notmatch '^[a-z0-9_. -]+$' -or $p.EndsWith('.exe') -eq $false) {
            throw "ProcessNames entries must be lowercase .exe names: $p"
        }
    }
    $procMatch = if ($ProcessNames.Count -eq 1) { "(PROCESS-NAME,$($ProcessNames[0]))" }
                 else { '(OR,' + (($ProcessNames | ForEach-Object { "(PROCESS-NAME,$_)" }) -join ',') + ')' }
    # Round-robin hands every replica an equal share of new connections, so the
    # replica ratio approximates the intended bandwidth split between paths.
    $isLoopback = [System.Net.IPAddress]::IsLoopback([System.Net.IPAddress]::Parse($PhoneAddress))
    $rules = @(
        "AND,($procMatch,(NETWORK,TCP),(OR,((DST-PORT,80),(DST-PORT,443)))),STEAM-DUAL",
        'MATCH,HOME-WIFI'
    )
    if ($ProxyTest) { $rules = @('MATCH,STEAM-DUAL') }
    $egress = @()
    for ($i = 1; $i -le $PhoneWeight; $i++) {
        $name = if ($i -eq 1) { 'PHONE-8282' } else { "PHONE-8282-r$i" }
        $node = [ordered]@{ name=$name; type='http'; server=$PhoneAddress; port=$PhonePort }
        if (!$isLoopback) { $node['interface-name'] = $PhoneInterface }
        $egress += $node
    }
    for ($i = 1; $i -le $HomeWeight; $i++) {
        $name = if ($i -eq 1) { 'HOME-WIFI' } else { "HOME-WIFI-r$i" }
        $egress += @{ name=$name; type='direct'; 'interface-name'=$HomeInterface }
    }
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
        proxies = $egress
        'proxy-groups' = @(@{
            name='STEAM-DUAL'; type='load-balance'; strategy='round-robin'
            proxies=@($egress | ForEach-Object { $_.name })
            url='https://www.gstatic.com/generate_204'; interval=60; lazy=$false
        })
        rules = $rules
    }
}

function Find-Adb {
    $candidates = @()
    if ($env:ADB_PATH) { $candidates += $env:ADB_PATH }
    if ($env:LOCALAPPDATA) { $candidates += (Join-Path $env:LOCALAPPDATA 'Android\Sdk\platform-tools\adb.exe') }
    if ($env:USERPROFILE) { $candidates += (Join-Path $env:USERPROFILE 'platform-tools\adb.exe') }
    $candidates += 'C:\platform-tools\adb.exe'
    $cmd = Get-Command adb.exe -ErrorAction SilentlyContinue
    if ($cmd) { $candidates += $cmd.Source }
    foreach ($c in $candidates) { if ($c -and (Test-Path $c)) { return $c } }
    return ''
}

function Install-Adb {
    $adb = Find-Adb
    if ($adb) { return $adb }
    # Official Google platform-tools bundle; same trust model as the mihomo fetch.
    $sdkDir = Join-Path $env:LOCALAPPDATA 'Android\Sdk'
    New-Item -ItemType Directory -Force $sdkDir | Out-Null
    $archive = Join-Path $sdkDir 'platform-tools.zip'
    Invoke-WebRequest 'https://dl.google.com/android/repository/platform-tools-latest-windows.zip' -OutFile $archive -UseBasicParsing
    Expand-Archive -LiteralPath $archive -DestinationPath $sdkDir -Force
    Remove-Item -LiteralPath $archive
    $adb = Find-Adb
    if (!$adb) { throw 'platform-tools extraction finished without adb.exe.' }
    return $adb
}

function Enable-AdbTunnel([int]$LocalPort = 8282, [int]$DevicePort = 8282) {
    $adb = Find-Adb
    if (!$adb) { throw 'adb.exe not found. Run Install-Adb first.' }
    $listed = @(& $adb devices | Select-Object -Skip 1 | Where-Object { $_.Trim() })
    $ready = @($listed | Where-Object { $_ -match '\sdevice\s*$' })
    if ($ready.Count -eq 0) {
        if (@($listed | Where-Object { $_ -match 'unauthorized' }).Count) {
            throw 'the phone shows as unauthorized. Unlock it, accept the "Allow USB debugging" dialog (tick always allow), then retry.'
        }
        throw 'no ADB device is visible. Enable Developer options -> USB debugging on the phone.'
    }
    if ($ready.Count -gt 1) { throw 'multiple ADB devices are connected; keep only the TetherFlow phone attached.' }
    & $adb forward "tcp:$LocalPort" "tcp:$DevicePort" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "adb forward tcp:$LocalPort failed." }
    # The tunnel must actually reach TetherFlow before callers rely on it.
    $probe = [System.Net.WebRequest]::Create("http://127.0.0.1:$LocalPort/pac")
    $probe.Proxy = $null
    $probe.Timeout = 5000
    $response = $probe.GetResponse()
    $response.Close()
    return $true
}

function Enable-SteamDualGuard([string]$PhoneInterface, [string]$PhoneAddress) {
    # Windows block rules override allows. Block every remote IP except the
    # phone itself, rather than an all-IP block plus a nonfunctional allow rule.
    if ([System.Net.IPAddress]::IsLoopback([System.Net.IPAddress]::Parse($PhoneAddress))) {
        # ADB-tunnel mode reaches the phone via loopback, so nothing on the USB
        # adapter needs to stay reachable: block the whole internet on it.
        $ranges = @('0.0.0.1-255.255.255.254', '::/1', '8000::/1')
    } else {
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
    }
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



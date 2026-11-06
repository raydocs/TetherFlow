#Requires -Version 5.1
<#
.SYNOPSIS
  Tests Start-SteamAccelerator.ps1 from an EXTRACTED package, under real Windows PowerShell 5.1,
  in a directory whose name has a space and CJK characters, using a fake executable.

.DESCRIPTION
  The launcher is run as a child `powershell.exe -NoProfile -File` (never dot-sourced). The
  executable it starts is testdata\fakeaccel, which records its argv and stdin to a file and
  exits with a chosen code. Nothing here touches the real hosts file or the network.

  Asserted: no token in argv; no token written to any file; exit code propagation; the -Mode,
  -DeviceUDID, -HttpOnly and weight mappings; invalid UDID rejection; -CheckPhoneOnly skips the
  admin and doctor path; the .ps1 files are pure ASCII (5.1 reads a BOM-less file as ANSI); the
  package manifest matches the files. With -RunRealDoctor the packaged real executable's
  `--doctor --json` must report the Apple USB service as FAIL (a CI runner has none).

  Run it elevated (the launcher's admin gate must pass). Needs: -PackageZip made by package.ps1
  and -FakeAccel built with `go build ./steam-tools/testdata/fakeaccel`.
  Not covered here (hardware gate H7): the non-admin refusal, SmartScreen, execution policy
  and Mark of the Web.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PackageZip,
    [Parameter(Mandatory = $true)][string]$FakeAccel,
    [string]$WorkRoot = $env:TEMP,
    [string]$ExpectedCommit,
    [string]$ExpectedZipSha256,
    [switch]$RunRealDoctor
)
$ErrorActionPreference = 'Stop'

$script:Failures = New-Object System.Collections.Generic.List[string]
function Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    if ($Ok) {
        Write-Host "PASS  $Name"
    } else {
        Write-Host "FAIL  $Name  $Detail"
        $script:Failures.Add($Name) | Out-Null
    }
}
# Error text may be wrapped at the console width, so compare with all white space removed.
function Has-Text {
    param([string]$Haystack, [string]$Needle)
    return (($Haystack -replace '\s', '').IndexOf(($Needle -replace '\s', ''), [StringComparison]::Ordinal) -ge 0)
}
function Join-Args {
    param($Items)
    return [string]::Join([string][char]1, [string[]]@($Items))
}
function Has-Pair {
    param($Items, [string]$Name, [string]$Value)
    $list = @($Items)
    for ($i = 0; $i -lt ($list.Count - 1); $i++) {
        if ($list[$i] -ceq $Name -and $list[$i + 1] -ceq $Value) { return $true }
    }
    return $false
}
function Has-Flag {
    param($Items, [string]$Name)
    return (@($Items) -ccontains $Name)
}

# ---- environment: this must be the real Windows PowerShell 5.1 ----
$psExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (-not (Test-Path -LiteralPath $psExe)) { throw "Windows PowerShell not found at $psExe" }
$version = $PSVersionTable.PSVersion
if ($version.Major -ne 5 -or $version.Minor -ne 1) {
    throw "Run Test-Launcher.ps1 with Windows PowerShell 5.1 (powershell.exe -File), not $($PSVersionTable.PSEdition) $version."
}
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run Test-Launcher.ps1 from an elevated PowerShell: the launcher refuses to continue without administrator rights, and these tests must reach its doctor and start steps.'
}
foreach ($required in @($PackageZip, $FakeAccel)) {
    if (-not (Test-Path -LiteralPath $required -PathType Leaf)) { throw "Missing input file: $required" }
}
Add-Type -AssemblyName System.IO.Compression.FileSystem

# ---- a work directory with a space and CJK characters in its name ----
$cjk = [string][char]0x6D4B + [string][char]0x8BD5 + [string][char]0x76EE + [string][char]0x5F55
$stamp = [guid]::NewGuid().ToString('N').Substring(0, 8)
$work = Join-Path $WorkRoot ("tf launcher $cjk $stamp")
$pkg = Join-Path $work 'extracted package'
$binDir = Join-Path $work 'fake bin'
$record = Join-Path $work 'record.jsonl'
New-Item -ItemType Directory -Path $pkg, $binDir | Out-Null
$fake = Join-Path $binDir 'fakeaccel.exe'
Copy-Item -LiteralPath $FakeAccel -Destination $fake

Write-Host "Package: $PackageZip"
$zipHash = (Get-FileHash -LiteralPath $PackageZip -Algorithm SHA256).Hash.ToLowerInvariant()
Write-Host "Package SHA-256: $zipHash"
if ($ExpectedZipSha256) {
    Check 'package zip is the file that will be uploaded' ($zipHash -eq $ExpectedZipSha256.ToLowerInvariant()) "got $zipHash"
}
[IO.Compression.ZipFile]::ExtractToDirectory($PackageZip, $pkg)
$launcher = Join-Path $pkg 'Start-SteamAccelerator.ps1'
Write-Host "Extracted to: $pkg"

# ---- package contents and manifest ----
$expectedFiles = @('MANIFEST.txt', 'README.zh-CN.md', 'START-HERE.zh-CN.txt', 'Start-SteamAccelerator.ps1', 'tetherflow-steam-accelerator.exe')
$actualFiles = @(Get-ChildItem -LiteralPath $pkg -Recurse -File | ForEach-Object { $_.FullName.Substring($pkg.Length + 1) } | Sort-Object)
Check 'package holds exactly the expected files' ((Join-Args $actualFiles) -ceq (Join-Args ($expectedFiles | Sort-Object))) ("found: " + ($actualFiles -join ', '))
$manifestPath = Join-Path $pkg 'MANIFEST.txt'
$manifest = ''
if (Test-Path -LiteralPath $manifestPath) { $manifest = [IO.File]::ReadAllText($manifestPath) }
$exePath = Join-Path $pkg 'tetherflow-steam-accelerator.exe'
if (Test-Path -LiteralPath $exePath) {
    $exeHash = (Get-FileHash -LiteralPath $exePath -Algorithm SHA256).Hash.ToLowerInvariant()
    Check 'manifest records the packaged executable SHA-256' ($manifest.Contains("executable_sha256: $exeHash")) "expected $exeHash"
}
Check 'manifest says unsigned experimental' ($manifest -match 'unsigned experimental')
Check 'manifest records the Authenticode status' ($manifest -match '(?m)^authenticode_status: \S')
if ($ExpectedCommit) {
    Check 'manifest records the commit' ($manifest.Contains("commit: $ExpectedCommit")) "expected $ExpectedCommit"
} else {
    Check 'manifest records a 40-character commit' ($manifest -match '(?m)^commit: [0-9a-f]{40}$')
}
$sourceLauncher = Join-Path $PSScriptRoot 'Start-SteamAccelerator.ps1'
if (Test-Path -LiteralPath $sourceLauncher) {
    $a = (Get-FileHash -LiteralPath $sourceLauncher -Algorithm SHA256).Hash
    $b = (Get-FileHash -LiteralPath $launcher -Algorithm SHA256).Hash
    Check 'packaged launcher equals the repository launcher' ($a -eq $b)
}

# ---- the scripts are pure ASCII (5.1 reads a BOM-less file as the ANSI code page) ----
$asciiTargets = @($launcher, $PSCommandPath, (Join-Path $PSScriptRoot 'package.ps1'))
foreach ($path in $asciiTargets) {
    if (-not (Test-Path -LiteralPath $path)) { continue }
    $bytes = [IO.File]::ReadAllBytes($path)
    $bad = 0
    foreach ($byte in $bytes) { if ($byte -gt 127) { $bad++ } }
    Check ("pure ASCII, no BOM: " + (Split-Path -Leaf $path)) ($bad -eq 0) "$bad byte(s) above 127"
}

# ---- running a child process ----
function ConvertTo-ArgString {
    param($Items)
    $parts = foreach ($a in @($Items)) {
        if ($a -match '^-[A-Za-z]+$') { $a } else { '"' + ($a -replace '"', '\"') + '"' }
    }
    return ($parts -join ' ')
}
function Invoke-Child {
    param([string]$File, [string]$ArgumentString, [string]$StdinText = '', [hashtable]$ExtraEnv = @{})
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $File
    $psi.Arguments = $ArgumentString
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.StandardOutputEncoding = [Text.Encoding]::UTF8
    $psi.StandardErrorEncoding = [Text.Encoding]::UTF8
    $psi.CreateNoWindow = $true
    $psi.WorkingDirectory = $work
    $psi.EnvironmentVariables['FAKEACCEL_RECORD'] = $record
    foreach ($key in $ExtraEnv.Keys) { $psi.EnvironmentVariables[[string]$key] = [string]$ExtraEnv[$key] }
    $process = [System.Diagnostics.Process]::Start($psi)
    $stdout = $process.StandardOutput.ReadToEndAsync()
    $stderr = $process.StandardError.ReadToEndAsync()
    if ($StdinText) { $process.StandardInput.Write($StdinText) }
    $process.StandardInput.Close()
    if (-not $process.WaitForExit(180000)) {
        $process.Kill()
        throw "Timed out: $File $ArgumentString"
    }
    $process.WaitForExit()
    return [pscustomobject]@{ ExitCode = $process.ExitCode; StdOut = $stdout.Result; StdErr = $stderr.Result }
}
function Invoke-Launcher {
    param($LauncherArgs, [string]$StdinText = '', [hashtable]$ExtraEnv = @{})
    if (Test-Path -LiteralPath $record) { Remove-Item -LiteralPath $record -Force }
    $env1 = @{ FAKEACCEL_READ_STDIN = '1' }
    foreach ($key in $ExtraEnv.Keys) { $env1[$key] = $ExtraEnv[$key] }
    $argString = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + $launcher + '" ' + (ConvertTo-ArgString $LauncherArgs)
    $result = Invoke-Child -File $psExe -ArgumentString $argString -StdinText $StdinText -ExtraEnv $env1
    $records = New-Object System.Collections.Generic.List[object]
    if (Test-Path -LiteralPath $record) {
        foreach ($line in [IO.File]::ReadAllLines($record, [Text.Encoding]::UTF8)) {
            if ($line.Trim()) { $records.Add((ConvertFrom-Json $line)) | Out-Null }
        }
    }
    $result | Add-Member -NotePropertyName Records -NotePropertyValue $records.ToArray()
    $result | Add-Member -NotePropertyName All -NotePropertyValue ($result.StdOut + "`n" + $result.StdErr)
    return $result
}
function Get-FileState {
    $state = @{}
    $listings = @(
        @(Get-ChildItem -LiteralPath $pkg -File -Recurse -Force -ErrorAction SilentlyContinue),
        @(Get-ChildItem -LiteralPath $env:TEMP -File -Force -ErrorAction SilentlyContinue)
    )
    foreach ($files in $listings) {
        foreach ($f in $files) { $state[$f.FullName] = "$($f.Length)|$($f.LastWriteTimeUtc.Ticks)" }
    }
    return $state
}
function Find-Secret {
    param($Before, $After, [string]$Secret)
    $found = New-Object System.Collections.Generic.List[string]
    foreach ($path in $After.Keys) {
        if ($Before.ContainsKey($path) -and $Before[$path] -eq $After[$path]) { continue }
        try {
            $bytes = [IO.File]::ReadAllBytes($path)
            if ($bytes.Length -gt 50MB) { continue }
            $latin = [Text.Encoding]::GetEncoding(28591).GetString($bytes)
            $wide = [Text.Encoding]::Unicode.GetString($bytes)
            if ($latin.Contains($Secret) -or $wide.Contains($Secret)) { $found.Add($path) | Out-Null }
        } catch {
            continue # a locked runner file cannot be ours
        }
    }
    return ,$found.ToArray()
}

$home1 = '192.168.1.23'
$canary = 'tfcanary' + [guid]::NewGuid().ToString('N').Substring(0, 24)

# ---- Windows PowerShell version, seen from the child ----
$ver = Invoke-Child -File $psExe -ArgumentString '-NoProfile -NonInteractive -Command "$PSVersionTable.PSVersion.ToString()"'
Check 'the child shell is Windows PowerShell 5.1' ($ver.StdOut.Trim().StartsWith('5.1')) ("reported '" + $ver.StdOut.Trim() + "'")

# ---- Case 1: default Dual run: arguments, stdin, no token in argv, no token file ----
$before = Get-FileState
$r = Invoke-Launcher -LauncherArgs @('-HomeIP', $home1, '-Executable', $fake) -StdinText ($canary + "`r`n")
$after = Get-FileState
Check 'dual: exit code 0' ($r.ExitCode -eq 0) $r.All
Check 'dual: doctor then the run (two invocations)' ($r.Records.Count -eq 2) ("saw " + $r.Records.Count)
if ($r.Records.Count -eq 2) {
    $doctorExpected = @('--doctor', '--uplink-mode', 'dual', '--home-ip', $home1)
    Check 'dual: doctor arguments' ((Join-Args $r.Records[0].args) -ceq (Join-Args $doctorExpected)) (Join-Args $r.Records[0].args)
    $runExpected = @('--uplink-mode', 'dual', '--home-ip', $home1, '--enable-steam', '--home-weight', '1', '--phone-weight', '4', '--report', (Join-Path $pkg 'tetherflow-status-dual.json'))
    Check 'dual: run arguments' ((Join-Args $r.Records[1].args) -ceq (Join-Args $runExpected)) (Join-Args $r.Records[1].args)
    Check 'dual: stdin reaches the executable (the token is typed there)' ($r.Records[1].stdin_read -and $r.Records[1].stdin_text.Contains($canary))
    $argvText = (@($r.Records | ForEach-Object { Join-Args $_.args }) -join ' ')
    Check 'dual: no token in any argv' (-not $argvText.Contains($canary))
}
Check 'dual: no token in the launcher output' (-not $r.All.Contains($canary))
$leaks = Find-Secret -Before $before -After $after -Secret $canary
Check 'dual: no token file in the package directory or the temp directory' ($leaks.Count -eq 0) ($leaks -join '; ')

# ---- Case 2: exit codes ----
$r = Invoke-Launcher -LauncherArgs @('-HomeIP', $home1, '-Executable', $fake) -ExtraEnv @{ FAKEACCEL_EXIT = '7' }
Check 'accelerator exit 7: the launcher fails' ($r.ExitCode -ne 0) "exit $($r.ExitCode)"
Check 'accelerator exit 7: the code is reported' (Has-Text $r.All 'Accelerator failed with exit code 7') $r.All
Check 'accelerator exit 7: both invocations happened' ($r.Records.Count -eq 2)
$r = Invoke-Launcher -LauncherArgs @('-HomeIP', $home1, '-Executable', $fake) -ExtraEnv @{ FAKEACCEL_DOCTOR_EXIT = '2' }
Check 'doctor exit 2: the launcher fails' ($r.ExitCode -ne 0) "exit $($r.ExitCode)"
Check 'doctor exit 2: the code is reported and nothing was started' ((Has-Text $r.All 'Doctor reported a FAIL (exit code 2)') -and $r.Records.Count -eq 1) $r.All
Check 'doctor exit 2: only the doctor ran' ($r.Records.Count -eq 1 -and (Has-Flag $r.Records[0].args '--doctor'))

# ---- Case 3: -Mode mapping ----
foreach ($case in @(@('HomeOnly', 'home'), @('PhoneOnly', 'phone'), @('Dual', 'dual'))) {
    $r = Invoke-Launcher -LauncherArgs @('-Mode', $case[0], '-HomeIP', $home1, '-Executable', $fake)
    $label = "mode $($case[0])"
    Check "${label}: exit 0" ($r.ExitCode -eq 0) $r.All
    if ($r.Records.Count -eq 2) {
        Check "${label}: --uplink-mode $($case[1]) in doctor and run" ((Has-Pair $r.Records[0].args '--uplink-mode' $case[1]) -and (Has-Pair $r.Records[1].args '--uplink-mode' $case[1]))
        Check "${label}: report file named for the mode" (Has-Pair $r.Records[1].args '--report' (Join-Path $pkg ("tetherflow-status-" + $case[1] + ".json")))
        if ($case[0] -eq 'HomeOnly') {
            $all = @($r.Records[0].args) + @($r.Records[1].args)
            Check "${label}: no phone, USB or tunnel flag" (-not ((Has-Flag $all '--device-udid') -or (Has-Flag $all '--external-tunnel') -or (Has-Flag $all '--check-phone')))
        }
    } else {
        Check "${label}: two invocations" $false ("saw " + $r.Records.Count)
    }
}
$r = Invoke-Launcher -LauncherArgs @('-Mode', 'HomeOnly', '-DeviceUDID', 'ABCD-1234', '-HomeIP', $home1, '-Executable', $fake)
Check 'HomeOnly with -DeviceUDID is refused before anything runs' ($r.ExitCode -ne 0 -and $r.Records.Count -eq 0 -and (Has-Text $r.All '-Mode HomeOnly does not use the iPhone')) $r.All

# ---- Case 4: -DeviceUDID, -HttpOnly, weights, allowed hosts ----
$r = Invoke-Launcher -LauncherArgs @('-Mode', 'PhoneOnly', '-HomeIP', $home1, '-DeviceUDID', 'ABCD-1234', '-HttpOnly', '-HomeWeight', '2', '-PhoneWeight', '3', '-AllowedHosts', 'steamcontent.com,steampipe.akamaized.net', '-Executable', $fake)
Check 'options: exit 0' ($r.ExitCode -eq 0) $r.All
if ($r.Records.Count -eq 2) {
    Check 'options: --device-udid in doctor and run' ((Has-Pair $r.Records[0].args '--device-udid' 'ABCD-1234') -and (Has-Pair $r.Records[1].args '--device-udid' 'ABCD-1234'))
    Check 'options: --http-only in doctor and run' ((Has-Flag $r.Records[0].args '--http-only') -and (Has-Flag $r.Records[1].args '--http-only'))
    Check 'options: weights' ((Has-Pair $r.Records[1].args '--home-weight' '2') -and (Has-Pair $r.Records[1].args '--phone-weight' '3'))
    Check 'options: --allow-hosts' (Has-Pair $r.Records[1].args '--allow-hosts' 'steamcontent.com,steampipe.akamaized.net')
} else {
    Check 'options: two invocations' $false ("saw " + $r.Records.Count)
}
# A hostile-looking UDID must arrive as one inert argv element, never be split or executed.
$odd = 'a b;calc'
$r = Invoke-Launcher -LauncherArgs @('-HomeIP', $home1, '-DeviceUDID', $odd, '-Executable', $fake)
$oddOk = $false
if ($r.Records.Count -eq 2) { $oddOk = Has-Pair $r.Records[1].args '--device-udid' $odd }
Check 'a UDID with a space and a semicolon stays one argv element' $oddOk $r.All

# ---- Case 5: invalid UDID rejection on the external-tunnel path ----
$r = Invoke-Launcher -LauncherArgs @('-HomeIP', $home1, '-IProxy', 'cmd.exe', '-DeviceUDID', 'bad udid;calc', '-Executable', $fake)
Check 'invalid UDID with -IProxy is rejected' ($r.ExitCode -ne 0 -and (Has-Text $r.All 'Invalid device UDID')) $r.All
$ranRun = $false
foreach ($rec in $r.Records) { if (-not (Has-Flag $rec.args '--doctor')) { $ranRun = $true } }
Check 'invalid UDID: the accelerator was never started' (-not $ranRun)

# ---- Case 6: -CheckPhoneOnly skips the admin and doctor path ----
$before = Get-FileState
$r = Invoke-Launcher -LauncherArgs @('-CheckPhoneOnly', '-Executable', $fake) -StdinText ($canary + "`r`n")
$after = Get-FileState
Check 'check-phone: exit 0' ($r.ExitCode -eq 0) $r.All
Check 'check-phone: exactly one invocation, --check-phone, no doctor and no hosts flag' ($r.Records.Count -eq 1 -and (Has-Flag $r.Records[0].args '--check-phone') -and -not (Has-Flag $r.Records[0].args '--doctor') -and -not (Has-Flag $r.Records[0].args '--enable-steam'))
if ($r.Records.Count -eq 1) {
    Check 'check-phone: token only on stdin' ($r.Records[0].stdin_text.Contains($canary) -and -not (Join-Args $r.Records[0].args).Contains($canary))
}
$leaks = Find-Secret -Before $before -After $after -Secret $canary
Check 'check-phone: no token file' ($leaks.Count -eq 0) ($leaks -join '; ')

# ---- Case 7: a missing executable ----
$r = Invoke-Launcher -LauncherArgs @('-HomeIP', $home1, '-Executable', (Join-Path $binDir 'missing.exe'))
Check 'missing executable is reported' ($r.ExitCode -ne 0 -and (Has-Text $r.All 'Missing executable') -and $r.Records.Count -eq 0) $r.All

# ---- Case 8: the packaged real executable's doctor, read-only (CI only) ----
if ($RunRealDoctor) {
    $d = Invoke-Child -File $exePath -ArgumentString '--doctor --json --uplink-mode phone'
    Check 'real doctor: exit status 2 (a FAIL)' ($d.ExitCode -eq 2) "exit $($d.ExitCode)`n$($d.StdErr)"
    $report = $null
    try { $report = $d.StdOut | ConvertFrom-Json } catch { $report = $null }
    Check 'real doctor: output is JSON' ($null -ne $report) $d.StdOut
    if ($null -ne $report) {
        $usb = @($report.checks | Where-Object { $_.name -eq 'apple-usb-service' })
        Check 'real doctor: Apple USB service is FAIL on the runner' ($usb.Count -eq 1 -and $usb[0].status -eq 'FAIL') ($d.StdOut)
        Check 'real doctor: overall result is FAIL' ($report.result -eq 'FAIL')
    }
}

Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
if ($script:Failures.Count -gt 0) {
    Write-Host ''
    Write-Host ("FAILED: " + $script:Failures.Count + " check(s)")
    foreach ($name in $script:Failures) { Write-Host "  - $name" }
    exit 1
}
Write-Host ''
Write-Host 'All launcher checks passed (Windows PowerShell 5.1, extracted package, fake executable).'
exit 0

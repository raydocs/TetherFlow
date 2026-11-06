#Requires -Version 5.1
<#
.SYNOPSIS
  Builds the reproducible Windows experimental package. It replaces the hand-built zip.

.DESCRIPTION
  The zip holds, flat and in a fixed order: the executable, Start-SteamAccelerator.ps1,
  README.zh-CN.md, START-HERE.zh-CN.txt and a generated MANIFEST.txt (commit, executable
  SHA-256, the Get-AuthenticodeSignature status, "unsigned experimental"). Entry order, names,
  timestamps and compression are fixed, so the same inputs give the same zip bytes on the
  same .NET runtime; -Verify builds it twice and fails if the two differ.

  The only pipeline output is the full path of the zip. CI tests that file after extracting
  it and uploads that same file.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Executable,
    [string]$OutDir = (Join-Path $PSScriptRoot 'dist'),
    [string]$Commit,
    [string]$ZipName,
    [switch]$Verify
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) { throw "Missing executable: $Executable" }
if (-not $Commit) { $Commit = $env:GITHUB_SHA }
if (-not $Commit) {
    $Commit = ((& git -C $PSScriptRoot rev-parse HEAD) | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Cannot determine the commit: pass -Commit.' }
}
$Commit = $Commit.ToLowerInvariant()
if ($Commit -notmatch '^[0-9a-f]{40}$') { throw "Commit must be a full 40-character SHA-1, got: $Commit" }
if (-not $ZipName) { $ZipName = 'tetherflow-steam-windows-experimental-' + $Commit.Substring(0, 12) + '.zip' }
if ($ZipName -notmatch '^[A-Za-z0-9._-]+\.zip$') { throw "Unsafe zip name: $ZipName" }

$exeName = 'tetherflow-steam-accelerator.exe'
$sourceFiles = [ordered]@{
    'Start-SteamAccelerator.ps1' = (Join-Path $PSScriptRoot 'Start-SteamAccelerator.ps1')
    'README.zh-CN.md'            = (Join-Path $PSScriptRoot 'README.zh-CN.md')
    'START-HERE.zh-CN.txt'       = (Join-Path $PSScriptRoot 'START-HERE.zh-CN.txt')
}
foreach ($name in $sourceFiles.Keys) {
    if (-not (Test-Path -LiteralPath $sourceFiles[$name] -PathType Leaf)) { throw "Missing package file: $($sourceFiles[$name])" }
}

function Get-Sha256 {
    param([byte[]]$Bytes)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try { return (($sha.ComputeHash($Bytes) | ForEach-Object { $_.ToString('x2') }) -join '') } finally { $sha.Dispose() }
}

$contents = @{}
$contents[$exeName] = [IO.File]::ReadAllBytes((Resolve-Path -LiteralPath $Executable).Path)
foreach ($name in $sourceFiles.Keys) { $contents[$name] = [IO.File]::ReadAllBytes($sourceFiles[$name]) }

try {
    $signature = Get-AuthenticodeSignature -LiteralPath (Resolve-Path -LiteralPath $Executable).Path
    $signatureStatus = [string]$signature.Status
} catch {
    $signatureStatus = 'Unavailable'
}

$lines = New-Object System.Collections.Generic.List[string]
$lines.Add('TetherFlow Steam accelerator - Windows package') | Out-Null
$lines.Add('status: unsigned experimental') | Out-Null
$lines.Add('commit: ' + $Commit) | Out-Null
$lines.Add('executable: ' + $exeName) | Out-Null
$lines.Add('executable_sha256: ' + (Get-Sha256 $contents[$exeName])) | Out-Null
$lines.Add('authenticode_status: ' + $signatureStatus) | Out-Null
$lines.Add('files:') | Out-Null
foreach ($name in $sourceFiles.Keys) { $lines.Add('  ' + (Get-Sha256 $contents[$name]) + '  ' + $name) | Out-Null }
$lines.Add('') | Out-Null
$lines.Add('This build is experimental. It is not code-signed by a trusted publisher, so Windows may warn about it.') | Out-Null
$lines.Add('It is not evidence that Steam uses the local relay, and it makes no speed claim.') | Out-Null
$manifestText = ($lines -join "`n") + "`n"
$contents['MANIFEST.txt'] = (New-Object System.Text.UTF8Encoding($false)).GetBytes($manifestText)

$names = [string[]]@($contents.Keys)
[Array]::Sort($names, [StringComparer]::Ordinal)
$epoch = New-Object System.DateTimeOffset -ArgumentList 2020, 1, 1, 0, 0, 0, ([TimeSpan]::Zero)

function New-PackageZip {
    param([string]$Path)
    if (Test-Path -LiteralPath $Path) { Remove-Item -LiteralPath $Path -Force }
    $stream = [IO.File]::Open($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::ReadWrite)
    try {
        $zip = New-Object System.IO.Compression.ZipArchive -ArgumentList $stream, ([System.IO.Compression.ZipArchiveMode]::Create), $false
        try {
            foreach ($name in $names) {
                $entry = $zip.CreateEntry($name, [System.IO.Compression.CompressionLevel]::Optimal)
                $entry.LastWriteTime = $epoch
                $entryStream = $entry.Open()
                try {
                    $bytes = [byte[]]$contents[$name]
                    $entryStream.Write($bytes, 0, $bytes.Length)
                } finally {
                    $entryStream.Dispose()
                }
            }
        } finally {
            $zip.Dispose()
        }
    } finally {
        $stream.Dispose()
    }
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$zipPath = Join-Path (Resolve-Path -LiteralPath $OutDir).Path $ZipName
New-PackageZip -Path $zipPath
$zipHash = (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant()

if ($Verify) {
    $second = Join-Path ([IO.Path]::GetTempPath()) ('tf-package-verify-' + [guid]::NewGuid().ToString('N') + '.zip')
    try {
        New-PackageZip -Path $second
        $secondHash = (Get-FileHash -LiteralPath $second -Algorithm SHA256).Hash.ToLowerInvariant()
    } finally {
        Remove-Item -LiteralPath $second -Force -ErrorAction SilentlyContinue
    }
    if ($secondHash -ne $zipHash) { throw "The package is not reproducible: $zipHash then $secondHash" }
    Write-Host 'Reproducible: a second build gave the same zip bytes.'
}

# Verify the zip by reading it back: names in order, no extras.
$zipRead = [IO.Compression.ZipFile]::OpenRead($zipPath)
try {
    $written = @($zipRead.Entries | ForEach-Object { $_.FullName })
} finally {
    $zipRead.Dispose()
}
if ([string]::Join('|', $written) -cne [string]::Join('|', $names)) { throw 'The zip entries are not the expected set.' }

Write-Host "Package: $zipPath"
Write-Host "SHA-256: $zipHash"
Write-Host "Commit:  $Commit"
Write-Host "Authenticode status of the executable: $signatureStatus (unsigned experimental)"
$zipPath

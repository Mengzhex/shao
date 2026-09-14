<#
.SYNOPSIS
tmon installer for Windows.

.DESCRIPTION
Downloads the latest release, verifies its checksum, installs it, and puts the
install directory on the user PATH.

    irm https://github.com/Mengzhex/tmon/releases/latest/download/install.ps1 | iex

.PARAMETER InstallDir
Where to put tmon.exe. Defaults to $HOME\bin.

.PARAMETER Version
A tag such as v0.1.0. Defaults to the latest release.

.PARAMETER Repo
owner/name, if you forked it.
#>
[CmdletBinding()]
param(
    [string]$InstallDir = "$HOME\bin",
    [string]$Version = 'latest',
    [string]$Repo = 'Mengzhex/tmon'
)

$ErrorActionPreference = 'Stop'

# TLS 1.2 is not the default in Windows PowerShell 5.1, and GitHub refuses
# anything older. PowerShell 7 already negotiates it.
if ($PSVersionTable.PSVersion.Major -lt 6) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
}

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}

$asset = "tmon_windows_$arch.zip"
$base = if ($Version -eq 'latest') {
    "https://github.com/$Repo/releases/latest/download"
} else {
    "https://github.com/$Repo/releases/download/$Version"
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("tmon-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null

try {
    Write-Host "tmon: downloading $asset"
    $zip = Join-Path $tmp $asset
    # Invoke-WebRequest's progress bar makes large downloads several times
    # slower in Windows PowerShell.
    $prev = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'
    try {
        Invoke-WebRequest -Uri "$base/$asset" -OutFile $zip -UseBasicParsing
    } finally {
        $ProgressPreference = $prev
    }

    # A silent corrupt download is worse than a loud failure.
    try {
        $sums = Join-Path $tmp 'checksums.txt'
        Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -UseBasicParsing
        $want = (Select-String -Path $sums -Pattern ([regex]::Escape($asset) + '$') |
            Select-Object -First 1).Line -split '\s+' | Select-Object -First 1
        if (-not $want) { throw "checksums.txt has no entry for $asset" }
        $got = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
        if ($got -ne $want.ToLower()) {
            throw "checksum mismatch for ${asset}:`n  expected $want`n  got      $got"
        }
        Write-Host 'tmon: checksum ok'
    } catch [System.Net.WebException] {
        Write-Host 'tmon: checksums.txt unavailable, skipping verification'
    }

    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    $binary = Join-Path $tmp 'tmon.exe'
    if (-not (Test-Path $binary)) { throw 'archive did not contain tmon.exe' }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $target = Join-Path $InstallDir 'tmon.exe'

    # A running tmon.exe cannot be overwritten. Say which command to run rather
    # than leaving the user with "the process cannot access the file".
    try {
        Copy-Item $binary $target -Force
    } catch [System.IO.IOException] {
        throw "could not replace $target -- tmon is probably still running. Stop it with 'tmon end' and 'tmon serve --stop', then run this again."
    }

    Write-Host "tmon: installed to $target"
    & $target version

    $userPath = [Environment]::GetEnvironmentVariable('PATH', 'User')
    if ($userPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable('PATH', "$userPath;$InstallDir", 'User')
        Write-Host ''
        Write-Host "Added $InstallDir to your user PATH. Open a new terminal for it to take effect."
    }

    Write-Host ''
    Write-Host "Next: run 'tmon start' in a terminal you want recorded."
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

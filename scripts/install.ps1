[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

function Stop-Installer {
    param([string]$Message)
    throw "glab-mr-graph installer: $Message"
}

function Get-MrGraphAliasCommand {
    param([object[]]$Lines)

    foreach ($lineValue in $Lines) {
        $line = [string]$lineValue
        $parts = $line.Trim() -split '\s+', 2
        if ($parts.Length -eq 2 -and $parts[0] -eq 'mr-graph') {
            return $parts[1].TrimStart('!')
        }
    }
    return $null
}

function Get-PathEntries {
    param([AllowNull()][string]$Value)
    if ([string]::IsNullOrEmpty($Value)) {
        return @()
    }
    return @($Value -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}

function Test-SamePath {
    param([string]$Left, [string]$Right)
    $leftValue = [Environment]::ExpandEnvironmentVariables($Left.Trim().Trim('"'))
    $rightValue = [Environment]::ExpandEnvironmentVariables($Right.Trim().Trim('"'))
    $leftNormalized = [System.IO.Path]::GetFullPath($leftValue).TrimEnd('\', '/')
    $rightNormalized = [System.IO.Path]::GetFullPath($rightValue).TrimEnd('\', '/')
    return [StringComparer]::OrdinalIgnoreCase.Equals($leftNormalized, $rightNormalized)
}

function Add-UserPathEntry {
    param([string]$Entry)

    $userEntries = @(Get-PathEntries ([Environment]::GetEnvironmentVariable('Path', 'User')))
    $hasUserEntry = $false
    foreach ($pathEntry in $userEntries) {
        if (Test-SamePath $pathEntry $Entry) {
            $hasUserEntry = $true
            break
        }
    }
    if (-not $hasUserEntry) {
        $userEntries += $Entry
        [Environment]::SetEnvironmentVariable('Path', ($userEntries -join ';'), 'User')
    }

    $processEntries = @(Get-PathEntries $env:Path)
    $hasProcessEntry = $false
    foreach ($pathEntry in $processEntries) {
        if (Test-SamePath $pathEntry $Entry) {
            $hasProcessEntry = $true
            break
        }
    }
    if (-not $hasProcessEntry) {
        $env:Path = (($processEntries + $Entry) -join ';')
    }

    return (-not $hasUserEntry)
}

function Remove-UserPathEntry {
    param([string]$Entry)

    $userEntries = @(Get-PathEntries ([Environment]::GetEnvironmentVariable('Path', 'User')) | Where-Object { -not (Test-SamePath $_ $Entry) })
    [Environment]::SetEnvironmentVariable('Path', ($userEntries -join ';'), 'User')
    $processEntries = @(Get-PathEntries $env:Path | Where-Object { -not (Test-SamePath $_ $Entry) })
    $env:Path = ($processEntries -join ';')
}

$glab = Get-Command glab -ErrorAction SilentlyContinue
if ($null -eq $glab) {
    Stop-Installer 'glab is required; install GitLab CLI and run glab auth login first'
}
if ($null -eq (Get-Command sh -ErrorAction SilentlyContinue)) {
    Stop-Installer 'Git for Windows sh is required because glab runs shell aliases through sh'
}

$aliasOutput = @(& glab alias list 2>$null)
if ($LASTEXITCODE -ne 0) {
    Stop-Installer 'cannot inspect glab aliases; refusing to overwrite an unknown alias state'
}
$aliasCommand = 'exec glab-mr-graph.exe "$@"'
$existingAlias = Get-MrGraphAliasCommand $aliasOutput
if ($null -ne $existingAlias -and $existingAlias -ne $aliasCommand) {
    Stop-Installer 'glab alias mr-graph already exists and is not managed by this installation; remove it manually before installing'
}

$version = if ([string]::IsNullOrEmpty($env:GLAB_MR_GRAPH_VERSION)) { 'latest' } else { $env:GLAB_MR_GRAPH_VERSION }
if ($version -eq 'latest') {
    $versionPath = 'latest/download'
} elseif ($version -match '^v[0-9]+\.[0-9]+\.[0-9]+$') {
    $versionPath = "download/$version"
} else {
    Stop-Installer 'GLAB_MR_GRAPH_VERSION must be latest or a release tag such as v0.1.0'
}

$rawArch = if (-not [string]::IsNullOrEmpty($env:PROCESSOR_ARCHITEW6432)) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($rawArch.ToUpperInvariant()) {
    'AMD64' { $releaseArch = 'amd64' }
    'ARM64' { $releaseArch = 'arm64' }
    'X86' { $releaseArch = '386' }
    default { Stop-Installer "unsupported architecture: $rawArch (supported: AMD64, ARM64, x86)" }
}

$assetName = "windows-$releaseArch.exe"
$downloadUrl = "https://github.com/ota-takeru/glab-mr-graph/releases/$versionPath/$assetName"
$requestedInstallDir = if (-not [string]::IsNullOrEmpty($env:GLAB_MR_GRAPH_INSTALL_DIR)) {
    $env:GLAB_MR_GRAPH_INSTALL_DIR
} elseif (-not [string]::IsNullOrEmpty($env:LOCALAPPDATA)) {
    Join-Path $env:LOCALAPPDATA 'glab-mr-graph\bin'
} else {
    Stop-Installer 'LOCALAPPDATA is not available; set GLAB_MR_GRAPH_INSTALL_DIR'
}

[System.IO.Directory]::CreateDirectory($requestedInstallDir) | Out-Null
$installDir = [System.IO.Path]::GetFullPath($requestedInstallDir).TrimEnd('\', '/')
$binaryPath = Join-Path $installDir 'glab-mr-graph.exe'
$pathMarker = Join-Path $installDir '.glab-mr-graph-path-added'
$markerExisted = Test-Path -LiteralPath $pathMarker -PathType Leaf
$tempPath = Join-Path $installDir ('.glab-mr-graph.download.{0}.tmp' -f ([guid]::NewGuid().ToString('N')))
$backupPath = $null
$installed = $false
$pathAdded = $false

try {
    if (-not [string]::IsNullOrEmpty($env:GLAB_MR_GRAPH_ASSET_PATH)) {
        Copy-Item -LiteralPath $env:GLAB_MR_GRAPH_ASSET_PATH -Destination $tempPath -Force
    } else {
        Invoke-WebRequest -Uri $downloadUrl -OutFile $tempPath -UseBasicParsing -TimeoutSec 120
    }
    if (-not (Test-Path -LiteralPath $tempPath -PathType Leaf) -or (Get-Item -LiteralPath $tempPath).Length -eq 0) {
        Stop-Installer 'downloaded release asset is empty'
    }

    if (Test-Path -LiteralPath $binaryPath) {
        $backupPath = Join-Path $installDir ('.glab-mr-graph.backup.{0}.tmp' -f ([guid]::NewGuid().ToString('N')))
        Move-Item -LiteralPath $binaryPath -Destination $backupPath
    }
    Move-Item -LiteralPath $tempPath -Destination $binaryPath
    $installed = $true

    $pathAdded = Add-UserPathEntry $installDir
    if ($markerExisted -or $pathAdded) {
        [System.IO.File]::WriteAllText($pathMarker, "path-added-v1`n")
    }

    & glab alias set --shell mr-graph $aliasCommand
    if ($LASTEXITCODE -ne 0) {
        Stop-Installer "binary installed at $binaryPath, but glab alias mr-graph could not be registered"
    }

    if ($null -ne $backupPath) {
        Remove-Item -LiteralPath $backupPath -Force
        $backupPath = $null
    }
    $installed = $false
} catch {
    if (Test-Path -LiteralPath $tempPath) {
        Remove-Item -LiteralPath $tempPath -Force -ErrorAction SilentlyContinue
    }
    if ($installed -and (Test-Path -LiteralPath $binaryPath)) {
        Remove-Item -LiteralPath $binaryPath -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $backupPath -and (Test-Path -LiteralPath $backupPath)) {
        Move-Item -LiteralPath $backupPath -Destination $binaryPath -Force -ErrorAction SilentlyContinue
    }
    if ($pathAdded -and -not $markerExisted) {
        Remove-UserPathEntry $installDir
        Remove-Item -LiteralPath $pathMarker -Force -ErrorAction SilentlyContinue
    }
    throw
} finally {
    if (Test-Path -LiteralPath $tempPath) {
        Remove-Item -LiteralPath $tempPath -Force -ErrorAction SilentlyContinue
    }
}

Write-Host "Installed $binaryPath"
Write-Host 'Run: glab mr-graph'

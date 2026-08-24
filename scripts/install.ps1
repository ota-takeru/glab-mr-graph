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

function Get-GlabShellPath {
    $shell = Get-Command sh -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -ne $shell) {
        return $shell.Source
    }

    $git = Get-Command git -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $git) {
        return $null
    }
    $gitShell = [System.IO.Path]::GetFullPath((Join-Path (Split-Path -Parent $git.Source) '..\bin\sh.exe'))
    if (Test-Path -LiteralPath $gitShell -PathType Leaf) {
        return $gitShell
    }
    return $null
}

function Save-InstallerAsset {
    param([AllowNull()][string]$LocalPath, [string]$Uri, [string]$Destination)

    if (-not [string]::IsNullOrEmpty($LocalPath)) {
        Copy-Item -LiteralPath $LocalPath -Destination $Destination -Force
    } else {
        Invoke-WebRequest -Uri $Uri -OutFile $Destination -UseBasicParsing -TimeoutSec 120
    }
    if (-not (Test-Path -LiteralPath $Destination -PathType Leaf) -or (Get-Item -LiteralPath $Destination).Length -eq 0) {
        Stop-Installer "downloaded release asset is empty: $Uri"
    }
}

$glab = Get-Command glab -ErrorAction SilentlyContinue
if ($null -eq $glab) {
    Stop-Installer 'glab is required; install GitLab CLI and run glab auth login first'
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
$shimAssetName = "windows-shim-$releaseArch.exe"
$shimDownloadUrl = "https://github.com/ota-takeru/glab-mr-graph/releases/$versionPath/$shimAssetName"
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
$runtimeDir = Join-Path $installDir 'runtime'
$shimPath = Join-Path $runtimeDir 'sh.exe'
$shimMarker = Join-Path $runtimeDir '.glab-mr-graph-shim-managed'
$runtimePathMarker = Join-Path $runtimeDir '.glab-mr-graph-path-added'
$shimMarkerExisted = Test-Path -LiteralPath $shimMarker -PathType Leaf
$runtimeMarkerExisted = Test-Path -LiteralPath $runtimePathMarker -PathType Leaf
$existingShellPath = Get-GlabShellPath
$forceShimFixture = -not [string]::IsNullOrEmpty($env:GLAB_MR_GRAPH_SHIM_ASSET_PATH)
$installShim = $forceShimFixture -or $null -eq $existingShellPath -or $shimMarkerExisted

if ($installShim) {
    [System.IO.Directory]::CreateDirectory($runtimeDir) | Out-Null
    if ((Test-Path -LiteralPath $shimPath -PathType Leaf) -and -not $shimMarkerExisted) {
        Stop-Installer "refusing to replace an unmanaged shell runtime at $shimPath"
    }
}

$tempPath = Join-Path $installDir ('.glab-mr-graph.download.{0}.tmp' -f ([guid]::NewGuid().ToString('N')))
$shimTempPath = if ($installShim) { Join-Path $runtimeDir ('.glab-mr-graph-shim.download.{0}.tmp' -f ([guid]::NewGuid().ToString('N'))) } else { $null }
$backupPath = $null
$shimBackupPath = $null
$installed = $false
$shimInstalled = $false
$pathAdded = $false
$runtimePathAdded = $false

try {
    Save-InstallerAsset $env:GLAB_MR_GRAPH_ASSET_PATH $downloadUrl $tempPath
    if ($installShim) {
        Save-InstallerAsset $env:GLAB_MR_GRAPH_SHIM_ASSET_PATH $shimDownloadUrl $shimTempPath
    }

    if (Test-Path -LiteralPath $binaryPath) {
        $backupPath = Join-Path $installDir ('.glab-mr-graph.backup.{0}.tmp' -f ([guid]::NewGuid().ToString('N')))
        Move-Item -LiteralPath $binaryPath -Destination $backupPath
    }
    Move-Item -LiteralPath $tempPath -Destination $binaryPath
    $installed = $true

    if ($installShim) {
        if (Test-Path -LiteralPath $shimPath -PathType Leaf) {
            $shimBackupPath = Join-Path $runtimeDir ('.glab-mr-graph-shim.backup.{0}.tmp' -f ([guid]::NewGuid().ToString('N')))
            Move-Item -LiteralPath $shimPath -Destination $shimBackupPath
        }
        Move-Item -LiteralPath $shimTempPath -Destination $shimPath
        $shimInstalled = $true
        [System.IO.File]::WriteAllText($shimMarker, "managed-v1`n")
    }

    $pathAdded = Add-UserPathEntry $installDir
    if ($markerExisted -or $pathAdded) {
        [System.IO.File]::WriteAllText($pathMarker, "path-added-v1`n")
    }
    if ($installShim) {
        $runtimePathAdded = Add-UserPathEntry $runtimeDir
        if ($runtimeMarkerExisted -or $runtimePathAdded) {
            [System.IO.File]::WriteAllText($runtimePathMarker, "path-added-v1`n")
        }
    }

    & glab alias set --shell mr-graph $aliasCommand
    if ($LASTEXITCODE -ne 0) {
        Stop-Installer "binary installed at $binaryPath, but glab alias mr-graph could not be registered"
    }

    if ($null -ne $backupPath) {
        Remove-Item -LiteralPath $backupPath -Force
        $backupPath = $null
    }
    if ($null -ne $shimBackupPath) {
        Remove-Item -LiteralPath $shimBackupPath -Force
        $shimBackupPath = $null
    }
    $installed = $false
    $shimInstalled = $false
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
    if ($shimInstalled -and (Test-Path -LiteralPath $shimPath)) {
        Remove-Item -LiteralPath $shimPath -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $shimBackupPath -and (Test-Path -LiteralPath $shimBackupPath)) {
        Move-Item -LiteralPath $shimBackupPath -Destination $shimPath -Force -ErrorAction SilentlyContinue
    }
    if ($pathAdded -and -not $markerExisted) {
        Remove-UserPathEntry $installDir
        Remove-Item -LiteralPath $pathMarker -Force -ErrorAction SilentlyContinue
    }
    if ($runtimePathAdded -and -not $runtimeMarkerExisted) {
        Remove-UserPathEntry $runtimeDir
        Remove-Item -LiteralPath $runtimePathMarker -Force -ErrorAction SilentlyContinue
    }
    if ($installShim -and -not $shimMarkerExisted) {
        Remove-Item -LiteralPath $shimMarker -Force -ErrorAction SilentlyContinue
    }
    throw
} finally {
    if (Test-Path -LiteralPath $tempPath) {
        Remove-Item -LiteralPath $tempPath -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $shimTempPath -and (Test-Path -LiteralPath $shimTempPath)) {
        Remove-Item -LiteralPath $shimTempPath -Force -ErrorAction SilentlyContinue
    }
}

Write-Host "Installed $binaryPath"
Write-Host 'Run: glab mr-graph'

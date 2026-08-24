[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

function Write-UninstallWarning {
    param([string]$Message)
    Write-Warning "glab-mr-graph uninstaller: $Message"
}

function Get-MrGraphAliasCommand {
    param([object[]]$Lines)
    foreach ($lineValue in $Lines) {
        $parts = ([string]$lineValue).Trim() -split '\s+', 2
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

function Remove-UserPathEntry {
    param([string]$Entry)
    $userEntries = @(Get-PathEntries ([Environment]::GetEnvironmentVariable('Path', 'User')) | Where-Object { -not (Test-SamePath $_ $Entry) })
    [Environment]::SetEnvironmentVariable('Path', ($userEntries -join ';'), 'User')
    $processEntries = @(Get-PathEntries $env:Path | Where-Object { -not (Test-SamePath $_ $Entry) })
    $env:Path = ($processEntries -join ';')
}

$requestedInstallDir = if (-not [string]::IsNullOrEmpty($env:GLAB_MR_GRAPH_INSTALL_DIR)) {
    $env:GLAB_MR_GRAPH_INSTALL_DIR
} elseif (-not [string]::IsNullOrEmpty($env:LOCALAPPDATA)) {
    Join-Path $env:LOCALAPPDATA 'glab-mr-graph\bin'
} else {
    Write-UninstallWarning 'LOCALAPPDATA is not available; set GLAB_MR_GRAPH_INSTALL_DIR'
    $null
}

if ($null -eq $requestedInstallDir) {
    return
}

$installDir = [System.IO.Path]::GetFullPath($requestedInstallDir).TrimEnd('\', '/')
$binaryPath = Join-Path $installDir 'glab-mr-graph.exe'
$pathMarker = Join-Path $installDir '.glab-mr-graph-path-added'
$runtimeDir = Join-Path $installDir 'runtime'
$shimPath = Join-Path $runtimeDir 'sh.exe'
$shimMarker = Join-Path $runtimeDir '.glab-mr-graph-shim-managed'
$runtimePathMarker = Join-Path $runtimeDir '.glab-mr-graph-path-added'
$expectedAlias = 'exec glab-mr-graph.exe "$@"'

if ($null -ne (Get-Command glab -ErrorAction SilentlyContinue)) {
    try {
        $aliasOutput = @(& glab alias list 2>$null)
        if ($LASTEXITCODE -ne 0) {
            throw 'alias list failed'
        }
        $existingAlias = Get-MrGraphAliasCommand $aliasOutput
        if ($null -ne $existingAlias) {
            if ($existingAlias -eq $expectedAlias) {
                & glab alias delete mr-graph *> $null
                if ($LASTEXITCODE -eq 0) {
                    Write-Host 'Removed glab alias mr-graph'
                } else {
                    Write-UninstallWarning 'could not remove the managed glab alias mr-graph'
                }
            } else {
                Write-UninstallWarning 'preserving existing glab alias mr-graph because it is not managed by this installation'
            }
        }
    } catch {
        Write-UninstallWarning 'could not inspect glab aliases; preserving alias state'
    }
} else {
    Write-UninstallWarning 'glab was not found; preserving alias state'
}

if (Test-Path -LiteralPath $binaryPath) {
    try {
        Remove-Item -LiteralPath $binaryPath -Force
        Write-Host "Removed $binaryPath"
    } catch {
        Write-UninstallWarning "could not remove $binaryPath"
    }
}

if (Test-Path -LiteralPath $pathMarker -PathType Leaf) {
    try {
        Remove-UserPathEntry $installDir
        Remove-Item -LiteralPath $pathMarker -Force
    } catch {
        Write-UninstallWarning "could not remove $installDir from the user PATH"
    }
}

if (Test-Path -LiteralPath $shimMarker -PathType Leaf) {
    try {
        if (Test-Path -LiteralPath $shimPath -PathType Leaf) {
            Remove-Item -LiteralPath $shimPath -Force
            Write-Host "Removed $shimPath"
        }
        Remove-Item -LiteralPath $shimMarker -Force
    } catch {
        Write-UninstallWarning "could not remove the managed shell runtime at $shimPath"
    }
}

if (Test-Path -LiteralPath $runtimePathMarker -PathType Leaf) {
    try {
        Remove-UserPathEntry $runtimeDir
        Remove-Item -LiteralPath $runtimePathMarker -Force
    } catch {
        Write-UninstallWarning "could not remove $runtimeDir from the user PATH"
    }
}

if (Test-Path -LiteralPath $runtimeDir -PathType Container) {
    Remove-Item -LiteralPath $runtimeDir -ErrorAction SilentlyContinue
}

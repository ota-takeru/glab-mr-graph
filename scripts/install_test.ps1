[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$repoDir = Split-Path -Parent $PSScriptRoot
$installScript = Join-Path $PSScriptRoot 'install.ps1'
$uninstallScript = Join-Path $PSScriptRoot 'uninstall.ps1'
$tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('glab-mr-graph-install-' + [guid]::NewGuid().ToString('N'))
$installDir = Join-Path $tempRoot 'install dir'
$aliasState = Join-Path $tempRoot 'alias.txt'
$downloadLog = Join-Path $tempRoot 'download.log'
$downloadAsset = Join-Path $tempRoot 'download.exe'
$assetV2 = Join-Path $tempRoot 'asset-v2.exe'
$graphAsset = Join-Path $tempRoot 'glab-mr-graph.exe'
$shimAsset = Join-Path $tempRoot 'shim.exe'
$shimAssetV2 = Join-Path $tempRoot 'shim-v2.exe'
$originalUserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$originalProcessPath = $env:Path
$originalGlabConfig = $env:GLAB_CONFIG_DIR
$realGlab = Get-Command glab -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw "install_test.ps1: $Message" }
}

function Assert-Equal {
    param($Actual, $Expected, [string]$Message)
    if ($Actual -ne $Expected) { throw "install_test.ps1: $Message; actual=<$Actual> expected=<$Expected>" }
}

function global:glab {
    $arguments = @($args)
    $global:LASTEXITCODE = 0
    if ($arguments.Count -ge 2 -and $arguments[0] -eq 'alias' -and $arguments[1] -eq 'list') {
        Write-Output "Alias`tCommand"
        if ((Test-Path -LiteralPath $env:FAKE_GLAB_ALIAS) -and (Get-Item -LiteralPath $env:FAKE_GLAB_ALIAS).Length -gt 0) {
            Write-Output ("mr-graph`t" + (Get-Content -LiteralPath $env:FAKE_GLAB_ALIAS -Raw))
        }
        return
    }
    if ($arguments.Count -ge 2 -and $arguments[0] -eq 'alias' -and $arguments[1] -eq 'set') {
        if ($env:FAKE_GLAB_SET_FAIL -eq '1') {
            $global:LASTEXITCODE = 9
            return
        }
        Set-Content -LiteralPath $env:FAKE_GLAB_ALIAS -Value $arguments[-1] -NoNewline
        return
    }
    if ($arguments.Count -ge 2 -and $arguments[0] -eq 'alias' -and $arguments[1] -eq 'delete') {
        Set-Content -LiteralPath $env:FAKE_GLAB_ALIAS -Value '' -NoNewline
        return
    }
    if ($arguments.Count -ge 1 -and $arguments[0] -eq 'mr-graph') {
        $binary = Join-Path $env:GLAB_MR_GRAPH_INSTALL_DIR 'glab-mr-graph.exe'
        & $binary @($arguments | Select-Object -Skip 1)
        $global:LASTEXITCODE = 0
        return
    }
    $global:LASTEXITCODE = 2
}

function global:Invoke-WebRequest {
    param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing, [int]$TimeoutSec)
    if ($env:FAKE_DOWNLOAD_FAIL -eq '1') {
        throw 'fixture download failure'
    }
    Add-Content -LiteralPath $env:FAKE_DOWNLOAD_LOG -Value $Uri
    Copy-Item -LiteralPath $env:FAKE_DOWNLOAD_ASSET -Destination $OutFile -Force
}

try {
    [System.IO.Directory]::CreateDirectory($installDir) | Out-Null
    Set-Content -LiteralPath $aliasState -Value '' -NoNewline
    Set-Content -LiteralPath $downloadLog -Value '' -NoNewline
    Copy-Item -LiteralPath $env:ComSpec -Destination $downloadAsset
    [System.IO.File]::WriteAllBytes($assetV2, [byte[]](1, 2, 3, 4, 5))
    Push-Location $repoDir
    try {
        & go build -o $graphAsset ./cmd/glab-mr-graph
        if ($LASTEXITCODE -ne 0) { throw 'failed to build graph integration fixture' }
        & go build -o $shimAsset ./cmd/glab-mr-graph-shim
        if ($LASTEXITCODE -ne 0) { throw 'failed to build shell shim fixture' }
    } finally {
        Pop-Location
    }
    [System.IO.File]::WriteAllBytes($shimAssetV2, [byte[]](6, 7, 8, 9, 10, 11))

    $env:FAKE_GLAB_ALIAS = $aliasState
    $env:FAKE_DOWNLOAD_LOG = $downloadLog
    $env:FAKE_DOWNLOAD_ASSET = $downloadAsset
    $env:GLAB_MR_GRAPH_INSTALL_DIR = $installDir
    $env:GLAB_MR_GRAPH_ASSET_PATH = $env:ComSpec
    $env:GLAB_MR_GRAPH_SHIM_ASSET_PATH = $shimAsset
    $env:PROCESSOR_ARCHITECTURE = 'AMD64'
    $env:PROCESSOR_ARCHITEW6432 = ''
    [Environment]::SetEnvironmentVariable('Path', '', 'User')

    & $installScript | Out-Null
    $binaryPath = Join-Path $installDir 'glab-mr-graph.exe'
    $runtimeDir = Join-Path $installDir 'runtime'
    $installedShim = Join-Path $runtimeDir 'sh.exe'
    Assert-True (Test-Path -LiteralPath $binaryPath) 'local fixture was not installed'
    Assert-True (Test-Path -LiteralPath $installedShim) 'private shell shim was not installed'
    Assert-Equal ((Get-Content -LiteralPath $aliasState -Raw)) 'exec glab-mr-graph.exe "$@"' 'managed alias command differs'
    $launchOutput = glab mr-graph /d /c 'echo launched'
    Assert-Equal (($launchOutput | Out-String).Trim()) 'launched' 'glab mr-graph did not forward arguments'
    Assert-True (([Environment]::GetEnvironmentVariable('Path', 'User')) -like "*$installDir*") 'install directory was not added to user PATH'
    Assert-True (Test-Path -LiteralPath (Join-Path $installDir '.glab-mr-graph-path-added')) 'PATH ownership marker is missing'
    Assert-True (Test-Path -LiteralPath (Join-Path $runtimeDir '.glab-mr-graph-shim-managed')) 'shim ownership marker is missing'
    Assert-True (Test-Path -LiteralPath (Join-Path $runtimeDir '.glab-mr-graph-path-added')) 'runtime PATH ownership marker is missing'
    $userPathEntries = @([Environment]::GetEnvironmentVariable('Path', 'User') -split ';')
    Assert-Equal $userPathEntries[0] $installDir 'install directory was not added before the private runtime'
    Assert-Equal $userPathEntries[1] $runtimeDir 'private runtime was not appended to user PATH'

    $shimOutput = & $installedShim -c 'exec glab-mr-graph.exe "$@"' -- /d /c 'echo shim-launched'
    Assert-Equal (($shimOutput | Out-String).Trim()) 'shim-launched' 'private shim did not forward the managed command'
    & $installedShim -c 'exec glab-mr-graph.exe "$@"' -- /d /c 'exit 17'
    Assert-Equal $LASTEXITCODE 17 'private shim did not preserve the target exit code'

    $env:GLAB_MR_GRAPH_ASSET_PATH = $assetV2
    & $installScript | Out-Null
    Assert-Equal ((Get-Item -LiteralPath $binaryPath).Length) 5 'repeat install did not upgrade binary'

    $env:GLAB_MR_GRAPH_ASSET_PATH = $env:ComSpec
    $previousShimLength = (Get-Item -LiteralPath $installedShim).Length
    $env:GLAB_MR_GRAPH_SHIM_ASSET_PATH = $shimAssetV2
    $env:FAKE_GLAB_SET_FAIL = '1'
    $failed = $false
    try { & $installScript | Out-Null } catch { $failed = $true }
    Assert-True $failed 'alias registration failure unexpectedly succeeded'
    Assert-Equal ((Get-Item -LiteralPath $binaryPath).Length) 5 'alias registration failure did not restore existing binary'
    Assert-Equal ((Get-Item -LiteralPath $installedShim).Length) $previousShimLength 'alias registration failure did not restore the existing shim'
    Remove-Item Env:FAKE_GLAB_SET_FAIL
    $env:GLAB_MR_GRAPH_SHIM_ASSET_PATH = $shimAsset

    Remove-Item Env:GLAB_MR_GRAPH_ASSET_PATH
    $env:FAKE_DOWNLOAD_FAIL = '1'
    $failed = $false
    try { & $installScript | Out-Null } catch { $failed = $true }
    Assert-True $failed 'download failure unexpectedly succeeded'
    Assert-Equal ((Get-Item -LiteralPath $binaryPath).Length) 5 'download failure replaced existing binary'
    Remove-Item Env:FAKE_DOWNLOAD_FAIL

    Remove-Item Env:GLAB_MR_GRAPH_SHIM_ASSET_PATH
    & $installScript | Out-Null
    Assert-True ((Get-Content -LiteralPath $downloadLog -Raw) -like '*/releases/latest/download/windows-amd64.exe*') 'latest download URL differs'
    Assert-True ((Get-Content -LiteralPath $downloadLog -Raw) -like '*/releases/latest/download/windows-shim-amd64.exe*') 'latest shim download URL differs'
    $env:GLAB_MR_GRAPH_VERSION = 'v1.2.3'
    & $installScript | Out-Null
    Assert-True ((Get-Content -LiteralPath $downloadLog -Raw) -like '*/releases/download/v1.2.3/windows-amd64.exe*') 'pinned download URL differs'
    Assert-True ((Get-Content -LiteralPath $downloadLog -Raw) -like '*/releases/download/v1.2.3/windows-shim-amd64.exe*') 'pinned shim download URL differs'
    $env:GLAB_MR_GRAPH_VERSION = 'v1.bad.3'
    $failed = $false
    try { & $installScript | Out-Null } catch { $failed = $true }
    Assert-True $failed 'invalid pinned version unexpectedly succeeded'
    Remove-Item Env:GLAB_MR_GRAPH_VERSION

    Set-Content -LiteralPath $aliasState -Value 'exec something-else "$@"' -NoNewline
    $env:GLAB_MR_GRAPH_ASSET_PATH = $assetV2
    $failed = $false
    try { & $installScript | Out-Null } catch { $failed = $true }
    Assert-True $failed 'unrelated alias collision unexpectedly succeeded'
    & $uninstallScript | Out-Null
    Assert-Equal ((Get-Content -LiteralPath $aliasState -Raw)) 'exec something-else "$@"' 'uninstall removed unrelated alias'
    Assert-True (-not (Test-Path -LiteralPath $binaryPath)) 'uninstall did not remove exact binary'
    Assert-True (([Environment]::GetEnvironmentVariable('Path', 'User')) -notlike "*$installDir*") 'uninstall did not remove installer-owned PATH entry'
    Assert-True (-not (Test-Path -LiteralPath $installedShim)) 'uninstall did not remove the managed shim'
    Assert-True (([Environment]::GetEnvironmentVariable('Path', 'User')) -notlike "*$runtimeDir*") 'uninstall did not remove installer-owned runtime PATH entry'

    Set-Content -LiteralPath $aliasState -Value '' -NoNewline
    & $installScript | Out-Null
    & $uninstallScript | Out-Null
    Assert-Equal ((Get-Item -LiteralPath $aliasState).Length) 0 'managed alias was not removed'
    Assert-True (-not (Test-Path -LiteralPath $binaryPath)) 'managed binary was not removed'
    Assert-True (-not (Test-Path -LiteralPath $installedShim)) 'managed shim was not removed'

    $env:FAKE_GLAB_SET_FAIL = '1'
    $failed = $false
    try { & $installScript | Out-Null } catch { $failed = $true }
    Assert-True $failed 'first-install alias failure unexpectedly succeeded'
    Assert-True (-not (Test-Path -LiteralPath $binaryPath)) 'first-install alias failure left a binary behind'
    Assert-True (([Environment]::GetEnvironmentVariable('Path', 'User')) -notlike "*$installDir*") 'first-install alias failure left a PATH entry behind'
    Assert-True (-not (Test-Path -LiteralPath $installedShim)) 'first-install alias failure left a shim behind'
    Assert-True (([Environment]::GetEnvironmentVariable('Path', 'User')) -notlike "*$runtimeDir*") 'first-install alias failure left a runtime PATH entry behind'
    Remove-Item Env:FAKE_GLAB_SET_FAIL

    [Environment]::SetEnvironmentVariable('Path', $installDir, 'User')
    $env:Path = $originalProcessPath + ';' + $installDir
    & $installScript | Out-Null
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $installDir '.glab-mr-graph-path-added'))) 'installer claimed a pre-existing PATH entry'
    & $uninstallScript | Out-Null
    Assert-Equal ([Environment]::GetEnvironmentVariable('Path', 'User')) $installDir 'uninstall removed a pre-existing PATH entry'

    [Environment]::SetEnvironmentVariable('Path', ($installDir + ';' + $runtimeDir), 'User')
    $env:Path = $originalProcessPath + ';' + $installDir + ';' + $runtimeDir
    & $installScript | Out-Null
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $installDir '.glab-mr-graph-path-added'))) 'installer claimed a pre-existing binary PATH entry'
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $runtimeDir '.glab-mr-graph-path-added'))) 'installer claimed a pre-existing runtime PATH entry'
    & $uninstallScript | Out-Null
    Assert-Equal ([Environment]::GetEnvironmentVariable('Path', 'User')) ($installDir + ';' + $runtimeDir) 'uninstall removed pre-existing binary or runtime PATH entries'

    $realShellDir = Join-Path $tempRoot 'real-shell'
    [System.IO.Directory]::CreateDirectory($realShellDir) | Out-Null
    Copy-Item -LiteralPath $env:ComSpec -Destination (Join-Path $realShellDir 'sh.exe')
    [Environment]::SetEnvironmentVariable('Path', $realShellDir, 'User')
    $env:Path = $realShellDir + ';' + $originalProcessPath
    Remove-Item Env:GLAB_MR_GRAPH_SHIM_ASSET_PATH -ErrorAction SilentlyContinue
    & $installScript | Out-Null
    Assert-True (-not (Test-Path -LiteralPath $installedShim)) 'installer added a private shim even though a real shell was available'
    $preferredPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    Assert-True $preferredPath.StartsWith($realShellDir + ';', [StringComparison]::OrdinalIgnoreCase) 'installer changed the real shell PATH priority'
    & $uninstallScript | Out-Null
    Assert-Equal ([Environment]::GetEnvironmentVariable('Path', 'User')) $realShellDir 'uninstall removed a pre-existing real shell PATH entry'

    if ($null -ne $realGlab) {
        Remove-Item Function:\glab
        [Environment]::SetEnvironmentVariable('Path', $originalUserPath, 'User')
        $env:Path = $originalProcessPath
        $realInstallDir = Join-Path $tempRoot 'real glab install'
        $realConfigDir = Join-Path $tempRoot 'real-glab-config'
        [System.IO.Directory]::CreateDirectory($realConfigDir) | Out-Null
        $env:GLAB_CONFIG_DIR = $realConfigDir
        $env:GLAB_MR_GRAPH_INSTALL_DIR = $realInstallDir
        $env:GLAB_MR_GRAPH_ASSET_PATH = $graphAsset
        $env:GLAB_MR_GRAPH_SHIM_ASSET_PATH = $shimAsset
        & $installScript | Out-Null
        $helpOutput = & $realGlab.Source mr-graph --help 2>&1 | Out-String
        Assert-Equal $LASTEXITCODE 0 'real glab did not launch the graph through the private shim'
        Assert-True ($helpOutput -match 'hostname') 'real glab did not return graph help output through the private shim'
        & $uninstallScript | Out-Null
        $aliasOutput = & $realGlab.Source alias list | Out-String
        Assert-True ($aliasOutput -notmatch '(?m)^mr-graph\s') 'real glab integration left the managed alias behind'
    }

    Write-Output 'install_test.ps1: ok'
} finally {
    [Environment]::SetEnvironmentVariable('Path', $originalUserPath, 'User')
    $env:Path = $originalProcessPath
    if ($null -eq $originalGlabConfig) {
        Remove-Item Env:GLAB_CONFIG_DIR -ErrorAction SilentlyContinue
    } else {
        $env:GLAB_CONFIG_DIR = $originalGlabConfig
    }
    Remove-Item Function:\glab -ErrorAction SilentlyContinue
    Remove-Item Function:\Invoke-WebRequest -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    foreach ($name in @('FAKE_GLAB_ALIAS', 'FAKE_DOWNLOAD_LOG', 'FAKE_DOWNLOAD_ASSET', 'FAKE_DOWNLOAD_FAIL', 'FAKE_GLAB_SET_FAIL', 'GLAB_MR_GRAPH_INSTALL_DIR', 'GLAB_MR_GRAPH_ASSET_PATH', 'GLAB_MR_GRAPH_SHIM_ASSET_PATH', 'GLAB_MR_GRAPH_VERSION')) {
        Remove-Item "Env:$name" -ErrorAction SilentlyContinue
    }
}

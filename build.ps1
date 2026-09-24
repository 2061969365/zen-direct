# build.ps1 - one-shot local build: version stamp + Windows file
# properties + self-sign + verify.
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\build.ps1
#   .\build.ps1 -Out zen-direct.exe        # output name (default)
#   .\build.ps1 -Version 1.2.3             # override VERSION file
#   .\build.ps1 -NoSign                    # skip self-sign (CI)
#   .\build.ps1 -SkipVerify                # skip final verify pass
#
# Bumping the version = edit the VERSION file, commit, rebuild.
param(
  [string]$Out = 'zen-direct.exe',
  [string]$Version = '',
  [switch]$NoSign,
  [switch]$SkipVerify
)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root  # go build / go vet resolve against the module here

if ($Version -eq '') {
  $Version = (Get-Content (Join-Path $root 'VERSION') -TotalCount 1).Trim()
}
$ver = $Version.TrimStart('v', 'V')
$parts = $ver.Split('.')
while ($parts.Count -lt 3) { $parts += '0' }
$ver4 = ($parts[0..2] -join '.') + '.0'
$tag = "v$($parts[0..2] -join '.')"
Write-Host "=== build zen-direct $tag ==="

# --- 1) Windows version resource (exe Properties > Details) ---
$syso = Join-Path $root 'version_windows_amd64.syso'
$jsonPath = Join-Path $env:TEMP 'zen-direct-versioninfo.json'
$vi = @{
  FixedFileInfo = @{
    FileVersion    = @{ Major = [int]$parts[0]; Minor = [int]$parts[1]; Patch = [int]$parts[2]; Build = 0 }
    ProductVersion = @{ Major = [int]$parts[0]; Minor = [int]$parts[1]; Patch = [int]$parts[2]; Build = 0 }
    FileFlagsMask  = '3f'
    FileFlags      = '00'
    FileOS         = '40004'
    FileType       = '01'
    FileSubType    = '00'
  }
  StringFileInfo = @{
    Comments         = 'OpenCode free-tier gateway'
    CompanyName      = 'zen-direct-local'
    FileDescription  = 'zen-direct OpenCode free-tier gateway'
    FileVersion      = $ver4
    InternalName     = 'zen-direct'
    LegalCopyright   = 'zen-direct-local'
    OriginalFilename = 'zen-direct.exe'
    ProductName      = 'zen-direct'
    ProductVersion   = $ver4
  }
  VarFileInfo = @{ Translation = @{ LangID = '0409'; CharsetID = '04B0' } }
}
$json = $vi | ConvertTo-Json -Depth 6
# No BOM: goversioninfo rejects UTF-8 BOM.
[System.IO.File]::WriteAllText($jsonPath, $json, (New-Object System.Text.UTF8Encoding $false))
Write-Host "goversioninfo -> $syso"
& go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest `
  -o $syso $jsonPath
if ($LASTEXITCODE -ne 0) { throw "goversioninfo failed (exit $LASTEXITCODE)" }

# --- 2) build with version stamp ---
$exe = Join-Path $root $Out
Write-Host "go build -> $exe"
& go build -trimpath -ldflags "-s -w -X main.appVersion=$tag" -o $exe .
if ($LASTEXITCODE -ne 0) { throw "go build failed (exit $LASTEXITCODE)" }

# --- 3) self-sign + trust (skipped on CI) ---
if (-not $NoSign) {
  Write-Host "self-sign..."
  powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'verify-windows.ps1') `
    -Exe $exe -SelfSign
  if ($LASTEXITCODE -ne 0) { throw "self-sign failed (exit $LASTEXITCODE)" }
}

# --- 4) report ---
$sig = Get-AuthenticodeSignature -LiteralPath $exe
$hash = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash
$fileVer = (Get-Item $exe).VersionInfo.FileVersion
Write-Host ""
Write-Host "version(tag) : $tag"
Write-Host "file version : $fileVer"
Write-Host "signature    : $($sig.Status)"
Write-Host "sha256       : $hash"

if (-not $SkipVerify) {
  Write-Host ""
  powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $root 'verify-windows.ps1') -Exe $exe
  if ($LASTEXITCODE -ne 0) { throw "verify failed (exit $LASTEXITCODE)" }
}

Write-Host ""
Write-Host "BUILD OK: $exe ($tag)"

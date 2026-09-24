# verify-windows.ps1 - local pre-flight for zen-direct.exe
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\verify-windows.ps1
#   ... -Unblock   # remove Mark of the Web
#   ... -SelfSign  # self-sign with CN=zen-direct-local
param(
  [string]$Exe = (Join-Path $PSScriptRoot 'zen-direct.exe'),
  [switch]$SelfSign,
  [switch]$Unblock
)
$ErrorActionPreference = 'Stop'
$script:fail = 0

function Ok($m)   { Write-Host "[OK]   $m" -ForegroundColor Green }
function Warn($m) { Write-Host "[WARN] $m" -ForegroundColor Yellow }
function Bad($m)  { Write-Host "[FAIL] $m" -ForegroundColor Red; $script:fail++ }

Write-Host "=== zen-direct Windows verify ==="
Write-Host "exe: $Exe"

if (-not (Test-Path -LiteralPath $Exe)) {
  Bad "exe not found"
  exit 1
}

# --- 1) Mark of the Web ---
$streams = @()
try {
  $streams = @(Get-Item -LiteralPath $Exe -Stream * -ErrorAction Stop | ForEach-Object { $_.Stream })
} catch { }
if ($streams -contains 'Zone.Identifier') {
  if ($Unblock) {
    Unblock-File -LiteralPath $Exe -ErrorAction Stop
    Ok "MOTW removed (Zone.Identifier)"
  } else {
    Warn "Mark of the Web present - rerun with -Unblock"
  }
} else {
  Ok "no Mark of the Web"
}

# --- 2) Authenticode ---
$sig = Get-AuthenticodeSignature -LiteralPath $Exe
Write-Host "Authenticode status: $($sig.Status)"
$doSign = $false
if ($sig.Status -eq 'Valid') {
  Ok "signature Valid: $($sig.SignerCertificate.Subject)"
} elseif ($sig.Status -eq 'NotSigned') {
  if ($SelfSign) {
    $doSign = $true
  } else {
    Warn "NotSigned - SmartScreen/AV may say unknown risk. Use -SelfSign or OV/EV cert."
  }
} elseif ($sig.Status -eq 'HashMismatch') {
  Bad "HashMismatch - corrupted or tampered"
} else {
  Warn "signature status: $($sig.Status)"
}

if ($doSign) {
  Warn "NotSigned - applying self-sign + trust..."
  # Prefer CurrentUser\My (always present). CodeSigning substore may be missing.
  $cert = $null
  try {
    $cert = Get-ChildItem Cert:\CurrentUser\My -CodeSigningCert -ErrorAction Stop |
      Where-Object { $_.Subject -eq 'CN=zen-direct-local' -and $_.NotAfter -gt (Get-Date).AddDays(30) } |
      Sort-Object NotAfter -Descending | Select-Object -First 1
  } catch { }
  if (-not $cert) {
    try {
      $cert = New-SelfSignedCertificate -Subject 'CN=zen-direct-local' `
        -Type CodeSigningCert -CertStoreLocation 'Cert:\CurrentUser\My' `
        -NotAfter (Get-Date).AddYears(2) -KeyExportPolicy Exportable `
        -HashAlgorithm SHA256 -ErrorAction Stop
    } catch {
      Bad "cannot create cert: $($_.Exception.Message)"
    }
  }
  if ($cert) {
    $r = $null
    try {
      $r = Set-AuthenticodeSignature -FilePath $Exe -Certificate $cert `
        -TimestampServer 'http://timestamp.digicert.com' -ErrorAction Stop
    } catch {
      try { $r = Set-AuthenticodeSignature -FilePath $Exe -Certificate $cert -ErrorAction Stop }
      catch { Bad "Set-AuthenticodeSignature failed: $($_.Exception.Message)" }
    }
    # Trust chain: export + import Root + TrustedPublisher so Status becomes Valid.
    if ($cert) {
      $cerPath = Join-Path $env:TEMP 'zen-direct-local.cer'
      try {
        Export-Certificate -Cert $cert -FilePath $cerPath -Force | Out-Null
        foreach ($store in @('Root', 'TrustedPublisher')) {
          $target = "Cert:\CurrentUser\$store"
          $have = Get-ChildItem $target -ErrorAction SilentlyContinue |
            Where-Object { $_.Thumbprint -eq $cert.Thumbprint }
          if (-not $have) {
            Import-Certificate -FilePath $cerPath -CertStoreLocation $target | Out-Null
            Ok "trusted cert imported into CurrentUser\$store"
          } else {
            Ok "cert already trusted in CurrentUser\$store"
          }
        }
      } catch {
        Warn "trust import failed: $($_.Exception.Message)"
      }
    }
    $sig2 = Get-AuthenticodeSignature -LiteralPath $Exe
    if ($sig2.Status -eq 'Valid') {
      Ok "signature now Valid: $($sig2.SignerCertificate.Subject)"
      Warn "SmartScreen reputation may still warn on first run of a new binary"
    } elseif ($sig2.Status -eq 'UnknownError') {
      Warn "signature UnknownError (timestamp offline?) - may still show unknown publisher"
    } else {
      Bad "signature after sign: $($sig2.Status)"
    }
  } else {
    Bad "no code-signing certificate available"
  }
}

# --- 3) PE header (first 2 bytes only) ---
$fs = [System.IO.File]::OpenRead($Exe)
try {
  $buf = New-Object byte[] 2
  $n = $fs.Read($buf, 0, 2)
  $len = $fs.Length
} finally {
  $fs.Close()
}
if ($n -lt 2 -or $buf[0] -ne 0x4D -or $buf[1] -ne 0x5A) {
  Bad "not MZ header (n=$n)"
} else {
  Ok ("PE/MZ ok ({0} MB)" -f [math]::Round($len / 1MB, 1))
}

# --- 4) SHA256 ---
$hash = (Get-FileHash -LiteralPath $Exe -Algorithm SHA256).Hash
Ok "SHA256 $hash"

# --- 5) Defender (optional) ---
try {
  $st = Get-MpComputerStatus -ErrorAction Stop
  Ok ("Defender AM={0} AV={1} RTP={2}" -f $st.AMServiceEnabled, $st.AntivirusEnabled, $st.RealTimeProtectionEnabled)
  $hits = @()
  try {
    $hits = @(Get-MpThreatDetection -ErrorAction Stop |
      Where-Object { "$($_.Resources)" -like "*zen-direct*" } |
      Select-Object -First 5)
  } catch { }
  if ($hits.Count -gt 0) {
    Bad "Defender detections reference zen-direct"
    $hits | Format-List InitialDetectionTime, Resources
  } else {
    Ok "no Defender detections for zen-direct"
  }
} catch {
  Warn "Defender query unavailable: $($_.Exception.Message)"
}

# --- 6) start/stop smoke on free port ---
# NOTE: do NOT use Start-Process -Hidden here. On this machine Defender holds
# hidden process creation of zen-direct.exe for ~2 min per launch (observed
# 15:33-15:45), while `cmd start /min` returns immediately. Bat uses cmd path.
Write-Host "=== start smoke ==="
$port = 18099
$busy = $null
try {
  $busy = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
} catch { }
if ($busy) {
  Warn "port $port busy - skip start smoke"
} else {
  $oldPort = $env:PORT
  $oldDiag = $env:DIRECT_DIAG
  $oldDbg = $env:DIRECT_DEBUG
  $oldBase = $env:OPENCODE_ZEN_BASE
  $oldPool = $env:SHIM_SESSION_FILE
  $oldLog = $env:LOG_FILE
  $smokePid = $null
  try {
    $env:PORT = "$port"
    $env:DIRECT_DIAG = '0'
    $env:DIRECT_DEBUG = '0'
    $env:OPENCODE_ZEN_BASE = 'http://127.0.0.1:1'
    $env:SHIM_SESSION_FILE = Join-Path $env:TEMP 'verify-pool.json'
    $env:LOG_FILE = Join-Path $env:TEMP 'verify-zen.log'
    cmd /c "start `"verify-zen`" /min `"$Exe`""
    # discover pid by listening port (up to ~10s)
    for ($i = 0; $i -lt 20 -and -not $smokePid; $i++) {
      Start-Sleep -Milliseconds 500
      $c = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue |
        Select-Object -First 1 -ExpandProperty OwningProcess
      if ($c) { $smokePid = $c }
    }
    $h = $null
    if ($smokePid) {
      Start-Sleep -Seconds 1
      try {
        $h = & curl.exe -s -m 3 "http://127.0.0.1:$port/health" 2>$null
        if (-not $h) {
          $h = (Invoke-WebRequest -Uri "http://127.0.0.1:$port/health" -TimeoutSec 3 -UseBasicParsing).Content
        }
      } catch {
        $h = $null
      }
    }
    if ("$h" -match '"status"\s*:\s*"ok"') {
      Ok "start smoke /health ok (pid $smokePid)"
    } else {
      Bad "start smoke health bad: $h"
    }
  } finally {
    if ($smokePid) {
      try { Stop-Process -Id $smokePid -Force -ErrorAction SilentlyContinue } catch { }
    }
    $env:PORT = $oldPort
    $env:DIRECT_DIAG = $oldDiag
    $env:DIRECT_DEBUG = $oldDbg
    $env:OPENCODE_ZEN_BASE = $oldBase
    $env:SHIM_SESSION_FILE = $oldPool
    $env:LOG_FILE = $oldLog
  }
}

Write-Host ""
if ($script:fail -gt 0) {
  Write-Host "RESULT: FAIL ($($script:fail))" -ForegroundColor Red
  exit 1
}
Write-Host "RESULT: PASS" -ForegroundColor Green
exit 0

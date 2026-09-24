# verify-windows.ps1 - local pre-flight for zen-direct.exe
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\verify-windows.ps1
#   ... -Unblock   # remove Mark of the Web
#   ... -SelfSign  # self-sign with CN=zen-direct-local (CurrentUser\CodeSigning)
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
if ($sig.Status -eq 'Valid') {
  Ok "signature Valid: $($sig.SignerCertificate.Subject)"
} elseif ($sig.Status -eq 'NotSigned') {
  if ($SelfSign) {
    Warn "NotSigned - applying self-sign..."
    $cert = $null
    try {
      $cert = Get-ChildItem Cert:\CurrentUser\CodeSigning -ErrorAction Stop |
        Where-Object { $_.Subject -eq 'CN=zen-direct-local' -and $_.NotAfter -gt (Get-Date) } |
        Sort-Object NotAfter -Descending | Select-Object -First 1
    } catch { }
    if (-not $cert) {
      $cert = New-SelfSignedCertificate -Subject 'CN=zen-direct-local' `
        -Type CodeSigningCert -CertStoreLocation 'Cert:\CurrentUser\CodeSigning' `
        -NotAfter (Get-Date).AddYears(2)
    }
    $r = $null
    try {
      $r = Set-AuthenticodeSignature -FilePath $Exe -Certificate $cert `
        -TimestampServer 'http://timestamp.digicert.com' -ErrorAction Stop
    } catch {
      $r = Set-AuthenticodeSignature -FilePath $Exe -Certificate $cert
    }
    if ($r -and ($r.Status -eq 'Valid' -or $r.Status -eq 'UnknownError')) {
      Ok "self-signed applied (status=$($r.Status))"
      Warn "SmartScreen may still warn until cert is trusted or OV/EV public cert is used"
    } else {
      Bad "self-sign failed: $(if ($r) { $r.Status } else { 'null' })"
    }
  } else {
    Warn "NotSigned - SmartScreen/AV may say unknown risk. Use -SelfSign or OV/EV cert."
  }
} elseif ($sig.Status -eq 'HashMismatch') {
  Bad "HashMismatch - corrupted or tampered"
} else {
  Warn "signature status: $($sig.Status)"
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
  Bad ("not MZ: n={0} b0=0x{1} b1=0x{2}" -f $n, $(if ($n -gt 0) { '{0:X2}' -f $buf[0] } else { '??' }), $(if ($n -gt 1) { '{0:X2}' -f $buf[1] } else { '??' }))
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

# --- 6) start/stop smoke on free port (no upstream needed) ---
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
  try {
    $env:PORT = "$port"
    $env:DIRECT_DIAG = '0'
    $env:DIRECT_DEBUG = '0'
    $env:OPENCODE_ZEN_BASE = 'http://127.0.0.1:1'
    $env:SHIM_SESSION_FILE = Join-Path $env:TEMP 'verify-pool.json'
    $env:LOG_FILE = Join-Path $env:TEMP 'verify-zen.log'
    $p = Start-Process -FilePath $Exe -WindowStyle Hidden -PassThru
    Start-Sleep -Seconds 2
    $h = $null
    try {
      # curl.exe -m caps total seconds; fall back to Invoke-WebRequest
      $h = & curl.exe -s -m 3 "http://127.0.0.1:$port/health" 2>$null
      if (-not $h) {
        $h = (Invoke-WebRequest -Uri "http://127.0.0.1:$port/health" -TimeoutSec 3 -UseBasicParsing).Content
      }
    } catch {
      $h = $null
    }
    if ("$h" -match '"status"\s*:\s*"ok"') {
      Ok "start smoke /health ok"
    } else {
      Bad "start smoke health bad: $h"
    }
  } finally {
    if ($p -and -not $p.HasExited) {
      try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch { }
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

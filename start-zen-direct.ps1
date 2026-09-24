# zen-direct gateway launcher（双击 / bat 调用均可）
# 可调变量集中在下方 CONFIG 区，改完重跑即生效。
$ErrorActionPreference = 'Stop'

# ===================== CONFIG（可调）=====================
$Port        = 8081          # 监听端口
$DirectDebug = '1'           # 1=详细请求日志（5-9MB/h） 0=关
$DirectDiag  = '1'           # 1=[STATS]/WARN/debug 0=静默
$Concurrency = '16'          # 上游并发 1-32
$LogMaxMB    = '200'         # 日志总量硬顶 MB
$LogKeepDays = 7             # 日志保留天数
$HideWindow  = $true         # $false=显示窗口（排查用）
$ZenBase     = 'https://opencode.ai/zen/v1'  # 上游 base
# 会话池；$null=exe 同目录 shim_sessions.json
$PoolFile    = $null
# =========================================================

$home_   = $PSScriptRoot
if (-not $home_) { $home_ = Split-Path -Parent $MyInvocation.MyCommand.Path }
$exe     = Join-Path $home_ 'zen-direct.exe'
$log     = Join-Path $home_ ('zen-direct-{0}.err.log' -f (Get-Date -Format 'yyyyMMdd'))
$pool    = if ($PoolFile) { $PoolFile } else { Join-Path $home_ 'shim_sessions.json' }

if (-not (Test-Path $exe)) { Write-Host "missing $exe"; exit 1 }

Get-CimInstance Win32_Process -Filter "Name='zen-direct.exe'" -ErrorAction SilentlyContinue |
  ForEach-Object { try { Stop-Process -Id $_.ProcessId -Force } catch {} }
Start-Sleep 2

Get-ChildItem (Join-Path $home_ 'zen-direct-*.err.log*') -ErrorAction SilentlyContinue |
  Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-$LogKeepDays) } |
  ForEach-Object { Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue }

$env:PORT              = "$Port"
$env:DIRECT_DEBUG      = $DirectDebug
$env:DIRECT_DIAG       = $DirectDiag
$env:SHIM_SESSION_FILE = $pool
$env:DIRECT_CONCURRENCY = $Concurrency
$env:LOG_FILE          = $log
$env:LOG_MAX_MB        = "$LogMaxMB"
$env:OPENCODE_ZEN_BASE = $ZenBase

$winStyle = if ($HideWindow) { 'Hidden' } else { 'Normal' }
Start-Process -FilePath $exe -WindowStyle $winStyle
Start-Sleep 2

# 日志总量兜底（与网关内轮转双保险）
$budget = [long]$LogMaxMB * 1MB
$sum = [long]0
Get-ChildItem (Join-Path $home_ 'zen-direct-*.err.log*') -ErrorAction SilentlyContinue |
  Sort-Object LastWriteTime -Descending |
  ForEach-Object {
    $sum += $_.Length
    if ($sum -gt $budget) { Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue }
  }

try {
  $h = curl.exe -s -m 5 "http://127.0.0.1:$Port/health"
  Write-Host "health: $h"
  if (Test-Path $log) { Get-Content $log | Select-Object -Last 2 }
} catch {
  Write-Host "start failed, see log: $log"
}

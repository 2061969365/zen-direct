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
$ZenBase     = 'https://opencode.ai/inference/v1'  # 上游 base（V2）
# 会话池；$null=exe 同目录 shim_sessions.json
# 换号策略：0=一对话一号（撞 429 才换） 1=每轮新号
$FreshPerTurn = '0'
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
$env:DIRECT_FRESH_PER_TURN = $FreshPerTurn

# Run-WithTimeout：外部命令硬超时（opencode CLI 在某些语境下会永久挂起，
# 双击快捷方式（-WindowStyle Hidden）下没有任何反馈，必须自己兜底）。
function Run-WithTimeout([string]$File, [string[]]$Args, [int]$TimeoutSec) {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $File
  $psi.Arguments = ($Args | ForEach-Object { '"' + $_ + '"' }) -join ' '
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.UseShellExecute = $false
  $psi.CreateNoWindow = $true
  $proc = [System.Diagnostics.Process]::Start($psi)
  $outTask = $proc.StandardOutput.ReadToEndAsync()
  $errTask = $proc.StandardError.ReadToEndAsync()
  if (-not $proc.WaitForExit($TimeoutSec * 1000)) {
    try { $proc.Kill() } catch {}
    try { $proc.WaitForExit(3000) } catch {}
    return $null
  }
  return $outTask.Result
}

# service 发现：本机后台 service 端口随机，问一次并缓存到环境。
# 用户 shell 里 `service status` 可靠（网关自己拉子进程在 hidden 语境下必死，
# 故发现只放 launcher）。失败/超时即跳过（service 没跑时网关退 harvest）。
try {
  if (-not $env:OPENCODE_SERVICE_URL) {
    $oc = (Get-Command opencode -ErrorAction SilentlyContinue).Source
    if ($oc) {
      $svcOut = Run-WithTimeout -File $oc -Args @('service', 'status') -TimeoutSec 8
      $svcUrl = ("$svcOut" -split "`r?`n" | Where-Object { $_.Trim() } | Select-Object -First 1)
      if ("$svcUrl".Trim() -match '^http://(127\.0\.0\.1|localhost):\d+$') {
        $env:OPENCODE_SERVICE_URL = "$svcUrl".Trim()
        Write-Host "service: $($env:OPENCODE_SERVICE_URL)"
      } else {
        Write-Host "service: not detected (gateway falls back)"
      }
    } else {
      Write-Host "service: opencode CLI not on PATH"
    }
  }
} catch {
  Write-Host "service: undiscovered ($($_.Exception.Message))"
}

# 真 session 收割（上游免费层只认 opencode 本体用过的 session，现签假 ses 必 403）。
# harvest_genuine.py 把本地 opencode.db 里调过 Zen 的 session 倒进 genuine_sessions.txt，
# 网关借池优先用它；收割失败也不挡启动（回退现签，免费层 403）。
try {
  $py = (Get-Command python -ErrorAction Stop).Source
  & $py (Join-Path $home_ 'harvest_genuine.py') (Join-Path $home_ 'genuine_sessions.txt') 2>&1 |
    ForEach-Object { Write-Host "harvest: $_" }
} catch {
  Write-Host "harvest: skip ($($_.Exception.Message))"
}

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

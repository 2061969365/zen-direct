@echo off
setlocal EnableExtensions
REM =========================================================================
REM  zen-direct launcher - edit variables below, then double-click
REM =========================================================================

REM --- tunable variables ---------------------------------------------------
set "PORT=8081"
REM diagnostics: 1 = [STATS]/WARN/debug lines ; 0 = silent
set "DIRECT_DIAG=1"
REM verbose request log: 1 = ~5-9MB/h ; 0 = off
set "DIRECT_DEBUG=1"
REM upstream concurrency (1-32)
set "DIRECT_CONCURRENCY=16"
REM session pool file (empty = shim_sessions.json next to exe)
set "SHIM_SESSION_FILE="
REM total log budget in MB
set "LOG_MAX_MB=200"
REM delete logs older than N days
set "LOG_KEEP_DAYS=7"
REM 1 = hide window ; 0 = show window (for debugging)
set "HIDE_WINDOW=1"
REM session policy: 0 = one session per conversation (rotate only on 429)
REM                 1 = a fresh upstream session on every turn
set "DIRECT_FRESH_PER_TURN=0"
REM upstream base URL (V2)
set "OPENCODE_ZEN_BASE=https://opencode.ai/inference/v1"
REM embedded sidecar (v0.8.0+): the exe spawns/manages bun itself.
REM auto = embedded (recommended) ; off = direct upstream (will 429)
set "DIRECT_SIDECAR=auto"
REM explicit bun.exe path (empty = auto-detect PATH / %USERPROFILE%\.bun)
set "ZEN_BUN="
REM ------------------------------------------------------------------------

set "HOME_DIR=%~dp0"
if "%HOME_DIR:~-1%"=="\" set "HOME_DIR=%HOME_DIR:~0,-1%"
set "EXE=%HOME_DIR%\zen-direct.exe"
if not exist "%EXE%" (
  echo missing %EXE%
  echo build: go build -o zen-direct.exe .
  pause
  exit /b 1
)

if "%SHIM_SESSION_FILE%"=="" set "SHIM_SESSION_FILE=%HOME_DIR%\shim_sessions.json"
for /f %%i in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMdd"') do set "DATESTAMP=%%i"
set "LOG_FILE=%HOME_DIR%\zen-direct-%DATESTAMP%.err.log"

REM kill previous instance
for /f "tokens=2" %%p in ('tasklist /fi "imagename eq zen-direct.exe" ^| findstr /i "zen-direct"') do (
  taskkill /f /pid %%p >nul 2>&1
)
timeout /t 2 /nobreak >nul

REM prune old / oversized logs
powershell -NoProfile -ExecutionPolicy Bypass -Command "$days=%LOG_KEEP_DAYS%; $budget=%LOG_MAX_MB%; $dir='%HOME_DIR%'; Get-ChildItem (Join-Path $dir 'zen-direct-*.err.log*') -EA SilentlyContinue | Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-$days) } | Remove-Item -Force -EA SilentlyContinue; $sum=[long]0; Get-ChildItem (Join-Path $dir 'zen-direct-*.err.log*') -EA SilentlyContinue | Sort-Object LastWriteTime -Descending | ForEach-Object { $sum+=$_.Length; if ($sum -gt ($budget * 1MB)) { Remove-Item $_.FullName -Force -EA SilentlyContinue } }"

REM service discovery: background service port is random; ask once here with a
REM hard 8s timeout (opencode CLI can hang in hidden/spawned contexts and the
REM old inline FOR loop would block the launcher forever). Failure = service
REM down; gateway falls back to harvest file. Manual override wins.
if not defined OPENCODE_SERVICE_URL (
  for /f "tokens=*" %%u in ('powershell -NoProfile -ExecutionPolicy Bypass -Command "$j = Start-Job { opencode service status 2>$null }; if (Wait-Job $j -Timeout 8) { Receive-Job $j | Select-Object -First 1 } else { Stop-Job $j }; Remove-Job $j -Force -EA SilentlyContinue" 2^>nul') do (
    echo %%u | findstr /b /c:"http://127.0.0.1:" >nul && set "OPENCODE_SERVICE_URL=%%u"
    echo %%u | findstr /b /c:"http://localhost:" >nul && set "OPENCODE_SERVICE_URL=%%u"
  )
)
if defined OPENCODE_SERVICE_URL echo service: %OPENCODE_SERVICE_URL%

set "PORT=%PORT%"
set "DIRECT_DIAG=%DIRECT_DIAG%"
set "DIRECT_DEBUG=%DIRECT_DEBUG%"
set "DIRECT_CONCURRENCY=%DIRECT_CONCURRENCY%"
set "SHIM_SESSION_FILE=%SHIM_SESSION_FILE%"
set "LOG_FILE=%LOG_FILE%"
set "LOG_MAX_MB=%LOG_MAX_MB%"
set "OPENCODE_ZEN_BASE=%OPENCODE_ZEN_BASE%"
set "DIRECT_FRESH_PER_TURN=%DIRECT_FRESH_PER_TURN%"

REM genuine session harvest (free tier only honors sessions seen from real
REM opencode; minted ses always 403). Best-effort, never blocks startup.
where python >nul 2>&1
if %ERRORLEVEL%==0 (
  python "%HOME_DIR%\harvest_genuine.py" "%HOME_DIR%\genuine_sessions.txt"
)

REM --- embedded upstream sidecar (v0.8.0+) ---
REM sidecar JS + official system prompt are embedded in zen-direct.exe; the
REM gateway spawns bun, waits for /health, restarts it and reaps it on exit.
if defined ZEN_BUN set "ZEN_BUN=%ZEN_BUN%"
set "DIRECT_SIDECAR=%DIRECT_SIDECAR%"
if /i not "%DIRECT_SIDECAR%"=="off" (
  powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='bun.exe'\" -EA SilentlyContinue | Where-Object { $_.CommandLine -like '*zen_sidecar.js*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -EA SilentlyContinue }" >nul 2>&1
  echo sidecar: embedded ^(gateway spawns bun^)
) else (
  echo sidecar: disabled -^> direct upstream ^(will 429^)
)

if "%HIDE_WINDOW%"=="1" (
  start "" /min "%EXE%"
) else (
  start "" "%EXE%"
)
timeout /t 2 /nobreak >nul

echo ==== zen-direct started ====
echo port=%PORT% diag=%DIRECT_DIAG% debug=%DIRECT_DEBUG% conc=%DIRECT_CONCURRENCY%
echo pool=%SHIM_SESSION_FILE%
echo log =%LOG_FILE%
curl.exe -s -m 5 "http://127.0.0.1:%PORT%/health"
echo(
if exist "%LOG_FILE%" powershell -NoProfile -Command "Get-Content -Tail 2 -LiteralPath '%LOG_FILE%'"
pause
endlocal

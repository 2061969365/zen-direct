# 启动

双击 `start-zen-direct.bat`（或 PowerShell 跑 `start-zen-direct.ps1`）。

先把 `zen-direct.exe` 放同目录：

```powershell
go build -o zen-direct.exe .
```

## Windows 校验（防「未知风险」误报）

```powershell
# 只检查：签名 / MOTW / PE / SHA256 / Defender / 本机起停
powershell -NoProfile -ExecutionPolicy Bypass -File .\verify-windows.ps1

# 清浏览器下载的 Mark of the Web
powershell -NoProfile -ExecutionPolicy Bypass -File .\verify-windows.ps1 -Unblock

# 本机自签（SmartScreen 仍可能提示；公网分发要 OV/EV 证书）
powershell -NoProfile -ExecutionPolicy Bypass -File .\verify-windows.ps1 -SelfSign
```

CI 每次 push 会在 `windows-latest` 真机：`go vet/test` → build exe → mock 上游 → `/health` → 非流 responses → `/ip` 路由，全绿才允许 build job。

## 可调变量

bat 顶部 / ps1 `CONFIG` 区：

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORT` | 8081 | 监听端口 |
| `DIRECT_DIAG` | 1 | 1=`[STATS]`/WARN/debug；0=静默 |
| `DIRECT_DEBUG` | 1 | 1=详细请求日志（约 5-9MB/h） |
| `DIRECT_CONCURRENCY` | 16 | 上游并发（1-32） |
| `SHIM_SESSION_FILE` | 同目录 `shim_sessions.json` | 会话池路径 |
| `LOG_MAX_MB` | 200 | 日志总量硬顶 |
| `LOG_KEEP_DAYS` | 7 | 过期日志自动删 |
| `HIDE_WINDOW` | 1 | 0=显示窗口（排查） |
| `OPENCODE_ZEN_BASE` | `https://opencode.ai/zen/v1` | 上游 base |

改完保存，重新双击即生效（先自动杀旧进程）。

## 行为

1. 杀掉已有 `zen-direct.exe`
2. 清理过期/超量日志
3. 注入环境变量后隐藏启动
4. `GET /health` 探活并打印日志尾部

日志：`zen-direct-YYYYMMDD.err.log`（按天分文件，网关内 50MB×4 轮转 + 脚本按总量兜底）。

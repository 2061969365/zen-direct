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

## 会话池（v0.5.0+：service 现铸真 session）

上游免费层只认 opencode 本体建的 `ses_`（白名单制，现签假 session 必 403
`FreeTierError`）。池失配/401-403 轮换时，网关先调本机后台 service
（`POST /api/session`，HTTP Basic，毫秒级）现铸一对一真 session；service
不可用退 harvest 名单，再退 ACP 子进程现铸，最后现签。
401/403 烧号有 2048 上限（超了整表清零防风暴）；烧过的共享 session 别家
fp 命中视同失配，不再复用。

- service 发现：`OPENCODE_SERVICE_URL` 直钉（仅回环，否则拒收防密码出内网）；
  否则解析 `opencode service status` 一次并缓存（service 重启换端口后连接
  失败自动重发现）。
  注意：发现只许放 launcher（用户 shell 可靠）；网关 hidden 语境下拉子进程
  必死（实测：同二进制同参，shell 全活、网关全灭），故网关内不做发现重试，
  失败即退 harvest。
- 认证：`OPENCODE_SERVER_PASSWORD`，缺省读 `~/.config/opencode/service.json`；
  用户名 `OPENCODE_SERVER_USERNAME`（缺省 `opencode`）。
- `SVC_OFF=1` 关 service 路径；`OPENCODE_BIN` 定 opencode 二进制位置。

## 会话池（v0.4.x：ACP 现铸，真 session 兜底）

上游免费层只认 opencode 本体建的 `ses_`（白名单制，现签假 session 必 403
`FreeTierError`）。池失配/401-403 轮换时，网关先调本地 `opencode acp`
（NDJSON，`initialize` + `session/new`，约 1.2s/次，仅新对话首请求付一次）
现铸一对一真 session；失败才退 harvest 名单，最后现签。

- 二进制发现：`OPENCODE_BIN` → PATH → npm 全局默认路径；找不到直接走兜底。
- `ACP_OFF=1` 关 ACP（纯 harvest 行为）；`ACP_CWD` 定新 session 归属目录
  （缺省网关所在目录）；`ACP_TIMEOUT_SEC` 超时（缺省 25s）。
- 并发现铸串行排队（防 N 个 200MB 子进程齐爆）；子进程 spawn-per-mint、
  用完即杀，无孤儿、无 pidfile。

## 会话池（v0.3.3：harvest 兜底）

上游免费层只认 opencode 本体用过的 `ses_`（白名单制，现签假 session 必 403
`FreeTierError`）。启动器先跑 `harvest_genuine.py`，把本地 `opencode.db`
里调过 Zen 的 session 倒进 `genuine_sessions.txt`；网关借池/401-403 轮换
一律优先用它，烧号的记坏跳过。`GENUINE_SESSION_FILE` 可改名单路径，
缺省与会话池同目录。名单缺失时回退老行为（现签，免费层 403）。

## 超时（长上下文必看）

网关分两段计时：**首字节等待**（prefill）与**传输间隔**分开，避免慢 prefill 被误杀。
启动日志打印生效值（`timeouts: header=.. first_byte=.. idle=..`）。

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZEN_HEADER_TIMEOUT_SEC` | `120` | 上游响应头等待（原硬编码 60） |
| `ZEN_FIRST_BYTE_TIMEOUT_SEC` | `300` | 首字节等待；300k prefill 实测 15–40s，慢出口放宽 |
| `STREAM_IDLE_TIMEOUT_SEC` | `120` | 首字节到达后的传输间隔 |

首字节超时 → `upstream_first_byte_timeout` failed 帧；中途 stall → `upstream_timeout`。
harness 见 failed 帧可安全重试（本轮内容由重试无损拿回）。

## 模型协议表（自动获取）

网关启动时抓官方 Endpoints 表（`zen.mdx`：模型 ID → 上游路径 responses/chat/messages），
失败则用内置快照；后台每 6h 刷新。`/health` 的 `table_models` 即当前表规模。

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZEN_TABLE_URL` | 官方 raw 地址 | 换自建镜像 |
| `ZEN_TABLE_REFRESH` | 开 | `0`/`false`/`off` 关闭刷新（纯快照，零外联启动） |
| `ZEN_TABLE_INTERVAL` | `6h` | 后台刷新间隔（`30m`/`1h` 等） |

网关不支持的上游端点（如 jev 的 `/systemone`）直接 400 明拒，不再透传撞上游 500。

改完保存，重新双击即生效（先自动杀旧进程）。

## 行为

1. 杀掉已有 `zen-direct.exe`
2. 清理过期/超量日志
3. 注入环境变量后隐藏启动
4. `GET /health` 探活并打印日志尾部

日志：`zen-direct-YYYYMMDD.err.log`（按天分文件，网关内 50MB×4 轮转 + 脚本按总量兜底）。

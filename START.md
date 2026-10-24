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
| `OPENCODE_ZEN_BASE` | `https://opencode.ai/inference/v1` | 上游 base |
| `DIRECT_FRESH_PER_TURN` | 0（启动器）/ 1（代码） | 1=每轮换新上游 session；0=一对话一号（撞 429 才换） |
| `DIRECT_FREELIMIT_COOLDOWN` | 0（默认关） | 免费额度 429 后的冷却秒数；轮换代理下默认不冷却 |

## 会话与门禁（v0.7.0+：V2 配方）

上游迁移到 `https://opencode.ai/inference/v1`（对照 oc2api v2.6.0 + 真机抓包），
门禁同步收紧，网关侧对应（缺一即 403）：
`opencode/latest/2.0.29/cli` UA、三元会话头（session/session-id/affinity，
废 `x-opencode-request`）、`x-opencode-client: cli`、`project: global`、
门禁桩换 `shell/read` 官方描述 + 真 schema、永不发送 `tool_choice: none`
（toolless 直接删键）、`stream_options.include_usage`。

## 保留名别名（v0.7.1+：harness 真流量）

上游对 `bash/edit/read/write/glob/grep/shell` 配非官方定义一律 403
（名实相符才放行；未知名随便过；history 旧调用名同样计入）。
网关进上游前改 neutral 别名（`zd_` 前缀，描述+参数原样保留，模型行为不变），
回客户端时映射回去，harness 原样执行。`tool_choice` 具名同样映射。
官方 `shell/read` 桩不受影响（harness 自带的同名会被改名让位）。

## 会话池（v0.6.0+：descending 现签，零依赖）

根因（probe_dir/probe_rule/实测 8/8+2/2）：上游 FreeTier 门禁只看 session
16 进制低 12 位——大计数值放行，ascending 小计数（1、2、3…）一律 403，
与高位/IP/登记无关。网关现签已改为精确 descending（`~（ts*0x1000+ctr）`，
与 identifier.ts 一致），本地现铸即过门禁，无需任何外部依赖。

铸造链（各档现均可用）：service 现铸 → harvest 名单 → ACP 现铸 →
本地 descending 现签。`Gen` 标记、烧号表、重启过滤保留。

毒名单（`poisoned_sessions.txt`）按**额度窗口**管理：打满的 session 要到
北京时间次日 07:59 才解禁，所以名单也在 07:59 整表作废（运行时清表+删文件，
启动时发现文件早于上次重置同样丢弃）。启动还会扫当天日志
（`zen-direct-YYYYMMDD.err.log`，含 `.1`–`.3` 轮转片）里的
`free-usage 429 ses=…`，把 id 收回毒库——进程被杀或名单被手工改过都不丢证据。
窗口重置清表之前，当天那批号会先追加进 `poison_archive.txt`（带
`# window ended …` 日期头），日志留 7 天但归档常驻，每天毒了哪些号可查。
默认**一对话一号**（启动器 `$FreshPerTurn = '0'`，代码默认 `1`）：吃到 429 时
记毒 + 换号，不做当次重试；429 日志里带 `turns=N`（这条号在本次窗口内已经成功
服务了几轮），用来量真实额度。

**会话键（`fp`）**：下游自带 `X-Session-Id`/`x-session-affinity` 就用它；没有头
就取**第一条 user 消息**（+ instructions/system 作盐）。绝不能取 `messages[0]`
——chat 客户端第一条几乎总是 system 提示词，那样所有对话会算成同一个 fp。
实测（2026-10-23 夜）：DSH 的 262 个请求只落成 **2 个 fp**，等于所有“新会话”
共用同一条上游 session，一条打满就全体 429；opencode 自带 `X-Session-Id`，
所以它一直正常。改成首条 user 消息后立刻一会话一条 session。

**冷却默认关闭**：429 有两个来源——session 打满、出口 IP 被连坐。后者与“此刻
这条线路”绑定，而轮换代理每条连接换 IP，网关探到的 IP 和实际出流 IP 不是同一
条连接。封到 07:59 只会让新会话第一发被本地短路（实测 00:06–00:09 DSH 全灭，
一秒后换 IP 的请求 200）。默认不冷却后每个请求各自去试，该记毒照样记毒；
需要时可 `DIRECT_FREELIMIT_COOLDOWN=<秒>` 显式打开。

429 兜底：认得出 `FreeUsageLimitError` → 记毒（换号）；明确是限流
（`too many requests` / `rate limit`）→ 原样透传，不记毒；**认不出类型** →
记毒 + 换号但不进冷却（上游改措辞时不再赌）。

旧注：此前误判为“白名单登记制”；实测证明纯结构判定（低 12 位），
本地 descending 现签与本体无异。service/harvest/ACP 链保留作冗余，
裸机（Railway/容器）单二进制即满血。

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

## 上游传输 sidecar（2026-10-24）

上游免费层现在按**客户端指纹**发额度，不是按 session：

- 传输层必须是 Bun `fetch`（opencode CLI 的运行时）；Go / Python / Node / Bun 的 `node:https` 一律 `FreeUsageLimitError`。
- 请求体 `messages[0]` 必须是 OpenCode 官方 system 提示词；否则同样 429。
- 满足这两条后，工具集、用户提示词、session id 都可以保持原样。

### v0.8.0+：内嵌 sidecar（一个 exe）

`zen_sidecar.js` 与官方提示词用 `//go:embed` 编进 `zen-direct.exe`，启动时释放到
`%LOCALAPPDATA%\zen-direct\sidecar\`，由网关自己 `spawn bun`、等 `/health`、把上游
指过去。发布物只有一个 exe，唯一外部依赖是本机装了 Bun。

    客户端 :8081 -> zen-direct.exe -> 127.0.0.1:<随机端口> (bun zen_sidecar.js) -> opencode.ai

- 端口：默认每次启动挑空闲端口；`ZEN_SIDECAR_PORT` 可固定。
- 连接：**默认每请求新建连接**（`keepalive: false`）。因为出口走轮换代理时，只有新连接
  才会换出口 IP；Bun 默认把连接池住 300s，切了节点也不换 —— 429 之后最需要换 IP 的那几十秒
  恰恰换不掉。代价是每请求多一次 TLS/CONNECT 握手（走代理约 +50~150ms）。
  `ZEN_SIDECAR_KEEPALIVE=1` 可恢复连接复用。
- 托管：bun 退出后指数退避重启（1s→30s 封顶），网关退出时收掉；网关被强杀时
  bun 靠 120s 无心跳自杀，不留孤儿。
- 观测：`GET /health` 里的 `sidecar` 字段（state/pid/port/restarts/last_error）；
  bun 的输出并入网关日志，行首 `bun|`，可看到 `inject=1 ... -> 200`。

| 变量 | 默认 | 说明 |
|---|---|---|
| `DIRECT_SIDECAR` | `auto` | `off` = 直连（仅排障，会 429） |
| `ZEN_BUN` | 空 | 空=自动找 PATH / `%USERPROFILE%\.bun` / npm 的 `bun.exe` |
| `ZEN_SIDECAR_PORT` | 空 | 空=自动挑空闲端口 |
| `ZEN_SIDECAR_DIR` | `%LOCALAPPDATA%\zen-direct\sidecar` | 释放目录（不可写时回退 exe 同目录 → TEMP） |
| `ZEN_SIDECAR_KEEPALIVE` | 空 | `1` = 复用连接；默认每请求新建（换代理节点立即生效） |
| `OPENCODE_ZEN_BASE` | `https://opencode.ai/inference/v1` | 远端上游；指向 loopback 时视为外部 sidecar，不再内嵌 |
| `DIRECT_SIDECAR_STRIP_PROMPT` | 空 | `1` = 不注入官方提示词（A/B 复现） |

### v0.7.x：外挂 sidecar（历史）

早期由启动器先拉起 `zen_sidecar.js`（Bun，默认 127.0.0.1:19200），再把
`OPENCODE_ZEN_BASE` 指向它。行为等价，只是多一个进程要自己管；启动器已改成交给 exe，
旧版启动器留下的 `bun.exe` 会在下次启动时被清掉。

| 变量 | 默认 | 说明 |
|---|---|---|
| `SidecarPort` / `SIDECAR_PORT` | 19200 | `off` = 直连（仅排障，会 429） |
| `BunPath` | 空 | 空=自动找 PATH / npm 的 bun.exe |
| `UpstreamProxy` | 空 | 空=继承环境；可填 `http://127.0.0.1:10809` |

sidecar 日志（外挂模式）：`sidecar-YYYYMMDD.out.log` / `.err.log`，每行含 `inject=1` 与上游状态。

复现实验：`.github/workflows/sidecar-e2e.yml`、`shape-final.yml`、`transport-fingerprint.yml`。

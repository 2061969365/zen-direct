// zen-direct sidecar：用 Bun 的 fetch 转发上游请求。
//
// 背景（2026-10-24 云端实测）：上游免费层现在按客户端指纹发放额度。
//   - 传输层必须是 Bun fetch 的 TLS 指纹；Go/Python/Node 一律 FreeUsageLimitError。
//   - 请求体 messages[0] 必须是 OpenCode 官方 system 提示词；否则同样 429。
// 只要满足这两点，工具集、用户提示词、session id 都可以保持网关原样。
//
// 用法：
//   bun zen_sidecar.js                 # 默认 127.0.0.1:19200
//   ZEN_SIDECAR_PORT=19200 bun zen_sidecar.js
//   网关侧：OPENCODE_ZEN_BASE=http://127.0.0.1:19200/inference/v1
//
// 环境变量：
//   ZEN_SIDECAR_PORT          监听端口，默认 19200
//   ZEN_UPSTREAM              上游，默认 https://opencode.ai
//   ZEN_OFFICIAL_PROMPT_FILE  官方提示词文件，默认脚本同目录 official_system_prompt.txt
//   ZEN_SIDECAR_STRIP_PROMPT  1 = 不注入官方提示词（仅用于 A/B 复现问题）
//   ZEN_SIDECAR_VERBOSE       1 = 每个请求打一行日志（默认 1）
//   ZEN_SIDECAR_PREFIX        允许转发的路径前缀，逗号分隔，默认 "/inference/,/zen/"
//   ZEN_SIDECAR_KEEPALIVE     1 = 复用连接（默认 0：每个请求新建连接）
//                             默认关是有意的：出口是轮换代理时，只有新连接才会
//                             换出口 IP；Bun 默认把连接池住 300s，切了节点也不换。
//   ZEN_IDLE_EXIT_MS          >0 时，这么久没有任何请求或 /health 心跳就自杀
//                             （网关内嵌启动时会设成 120000，防强杀留孤儿）

const fs = require("fs");
const path = require("path");

const PORT = Number(process.env.ZEN_SIDECAR_PORT || 19200);
const UPSTREAM = (process.env.ZEN_UPSTREAM || "https://opencode.ai").replace(/\/+$/, "");
const VERBOSE = process.env.ZEN_SIDECAR_VERBOSE !== "0";
const STRIP = process.env.ZEN_SIDECAR_STRIP_PROMPT === "1";
const KEEPALIVE = process.env.ZEN_SIDECAR_KEEPALIVE === "1";
const IDLE_EXIT_MS = Number(process.env.ZEN_IDLE_EXIT_MS || 0);
const PREFIXES = (process.env.ZEN_SIDECAR_PREFIX || "/inference/,/zen/")
  .split(",").map((s) => s.trim()).filter(Boolean);
let lastSeen = Date.now();

const promptFile = process.env.ZEN_OFFICIAL_PROMPT_FILE ||
  path.join(import.meta.dir, "official_system_prompt.txt");
let OFFICIAL = "";
try {
  OFFICIAL = fs.readFileSync(promptFile, "utf8").trim();
} catch (e) {
  console.error("[sidecar] cannot read official prompt: " + promptFile + " (" + e.message + ")");
}
if (!OFFICIAL) {
  console.error("[sidecar] WARNING: official prompt empty; upstream will answer 429");
}

// 上游只要看到“官方提示词在 messages[0]”就放行；用前 80 字符做去重判断。
const PROMPT_HEAD = OFFICIAL.slice(0, 80);

function log(line) {
  if (VERBOSE) console.log("[sidecar] " + new Date().toISOString() + " " + line);
}

function injectPrompt(bodyText) {
  let body;
  try {
    body = JSON.parse(bodyText);
  } catch {
    return { text: bodyText, injected: false, tools: -1, msgs: -1 };
  }
  if (!Array.isArray(body.messages)) {
    return { text: bodyText, injected: false, tools: (body.tools || []).length, msgs: -1 };
  }
  const first = body.messages[0];
  const already = first && first.role === "system" &&
    typeof first.content === "string" && first.content.startsWith(PROMPT_HEAD);
  let injected = false;
  if (!STRIP && !already && OFFICIAL) {
    body.messages.unshift({ role: "system", content: OFFICIAL });
    injected = true;
  }
  return {
    text: JSON.stringify(body),
    injected,
    tools: (body.tools || []).length,
    msgs: body.messages.length,
  };
}

Bun.serve({
  port: PORT,
  hostname: "127.0.0.1",
  idleTimeout: 180,
  async fetch(req) {
    lastSeen = Date.now();
    const url = new URL(req.url);
    if (url.pathname === "/health") {
      return Response.json({ status: "ok", upstream: UPSTREAM, prompt: OFFICIAL.length, strip: STRIP, keepalive: KEEPALIVE });
    }
    if (!PREFIXES.some((p) => url.pathname.startsWith(p))) {
      return new Response("not found", { status: 404 });
    }

    const started = Date.now();
    const headers = new Headers(req.headers);
    for (const h of ["host", "content-length", "connection", "accept-encoding", "transfer-encoding"]) {
      headers.delete(h);
    }

    let body;
    let meta = { injected: false, tools: -1, msgs: -1 };
    if (req.method !== "GET" && req.method !== "HEAD") {
      const raw = await req.text();
      if (url.pathname.endsWith("/chat/completions")) {
        const r = injectPrompt(raw);
        body = r.text;
        meta = r;
      } else {
        body = raw;
      }
    }

    try {
      const r = await fetch(UPSTREAM + url.pathname + url.search, {
        method: req.method,
        headers,
        body,
        redirect: "manual",
        // 默认每请求新建连接：出口是轮换代理时，连接池会把出口 IP 钉死 300s，
        // 429 之后想换 IP 换不掉。ZEN_SIDECAR_KEEPALIVE=1 可恢复复用。
        keepalive: KEEPALIVE,
      });
      const out = new Headers(r.headers);
      for (const h of ["content-encoding", "content-length", "transfer-encoding", "connection"]) {
        out.delete(h);
      }
      log(`${req.method} ${url.pathname} inject=${meta.injected ? 1 : 0} tools=${meta.tools} ` +
          `msgs=${meta.msgs} -> ${r.status} ${Date.now() - started}ms`);
      return new Response(r.body, { status: r.status, headers: out });
    } catch (e) {
      log(`${req.method} ${url.pathname} -> ERR ${e.message}`);
      return Response.json({ error: { message: String(e) } }, { status: 502 });
    }
  },
});

console.log(`[sidecar] listening on http://127.0.0.1:${PORT} -> ${UPSTREAM}`);
console.log(`[sidecar] official prompt: ${OFFICIAL.length} chars${STRIP ? " (STRIPPED)" : ""}`);
console.log(`[sidecar] connection: ${KEEPALIVE ? "keep-alive pool" : "per-request (no reuse) -> new egress ip"}`);

// 内嵌模式（网关启动时注入）：网关每 15s 打一次 /health。心跳断了说明网关
// 已经被强杀（Stop-Process -Force 不会跑 defer），自己走人，别留孤儿占端口。
if (IDLE_EXIT_MS > 0) {
  const tick = Math.max(1000, Math.min(15000, Math.floor(IDLE_EXIT_MS / 3)));
  setInterval(() => {
    const idle = Date.now() - lastSeen;
    if (idle > IDLE_EXIT_MS) {
      console.log(`[sidecar] no gateway contact for ${idle}ms; exiting`);
      process.exit(0);
    }
  }, tick).unref?.();
}

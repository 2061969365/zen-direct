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

const fs = require("fs");
const path = require("path");

const PORT = Number(process.env.ZEN_SIDECAR_PORT || 19200);
const UPSTREAM = (process.env.ZEN_UPSTREAM || "https://opencode.ai").replace(/\/+$/, "");
const VERBOSE = process.env.ZEN_SIDECAR_VERBOSE !== "0";
const STRIP = process.env.ZEN_SIDECAR_STRIP_PROMPT === "1";

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
    const url = new URL(req.url);
    if (url.pathname === "/health") {
      return Response.json({ status: "ok", upstream: UPSTREAM, prompt: OFFICIAL.length, strip: STRIP });
    }
    if (!url.pathname.startsWith("/inference/") && !url.pathname.startsWith("/zen/")) {
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

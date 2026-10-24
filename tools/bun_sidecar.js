// Bun sidecar: 用 Bun 的 fetch 转发网关的上游请求。
// 上游免费层现在只认 opencode CLI（Bun fetch）的 TLS 指纹；Go/Python/Node 一律
// 返回 FreeUsageLimitError。把它放在 zen-direct 前面，上游看到的就是 CLI 的传输层。
//
//   bun tools/bun_sidecar.js 19200
//   OPENCODE_ZEN_BASE=http://127.0.0.1:19200/inference/v1
//
// 只转发 /inference/* 与 /zen/*，其他路径直接 404。

const port = Number(process.argv[2] || 19200);
const UPSTREAM = "https://opencode.ai";

Bun.serve({
  port,
  idleTimeout: 120,
  async fetch(req) {
    const url = new URL(req.url);
    if (!url.pathname.startsWith("/inference/") && !url.pathname.startsWith("/zen/")) {
      return new Response("not found", { status: 404 });
    }
    const target = UPSTREAM + url.pathname + url.search;
    const body = req.method === "GET" || req.method === "HEAD" ? undefined : await req.arrayBuffer();
    const headers = new Headers(req.headers);
    headers.delete("host");
    headers.delete("content-length");
    headers.delete("connection");
    try {
      const r = await fetch(target, { method: req.method, headers, body, redirect: "manual" });
      const out = new Headers(r.headers);
      out.delete("content-encoding");
      out.delete("content-length");
      out.delete("transfer-encoding");
      return new Response(r.body, { status: r.status, headers: out });
    } catch (e) {
      return new Response(JSON.stringify({ error: { message: String(e) } }), {
        status: 502, headers: { "content-type": "application/json" },
      });
    }
  },
});

console.log("bun sidecar on http://127.0.0.1:" + port + " -> " + UPSTREAM);

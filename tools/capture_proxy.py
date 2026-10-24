"""Capture proxy:记录 opencode CLI / 网关发往上游的真实请求。

用途：同一个出口 IP、同一个模型，CLI 能用而网关 429。要定位差异，就得把两边
真正发出去的 HTTP 抓下来对比——头、路径、body 结构。

用法（本地起一个代理，把客户端指过来）：

    python scripts/capture_proxy.py --port 19100 --out captures

    # 终端 A：CLI 走代理（不碰你的 opencode.json）
    $env:OPENCODE_CONFIG_CONTENT = '{"provider":{"opencode":{"options":{"baseURL":"http://127.0.0.1:19100/inference/v1"}}}}'
    opencode run --model opencode/deepseek-v4.1-flash-free "say ok"

    # 终端 B：网关走代理（另起一个实例，生产不动）
    $env:OPENCODE_ZEN_BASE = 'http://127.0.0.1:19100/inference/v1'
    $env:PORT = '18090'; .\zen-direct.exe

每个请求落一份 JSON：method / path / headers / body / 上游响应状态与开头。
注意：会把响应整体缓冲后再回给客户端（SSE 会一次性到达），只用于排查。
"""

import argparse
import json
import os
import re
import sys
import time
from http.client import HTTPSConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM_HOST = "opencode.ai"
HOP_BY_HOP = {
    "host", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailer", "transfer-encoding", "upgrade", "content-length",
}
# 这些头会影响上游压缩/编码，抓包时去掉，方便直读 body
DROP_REQ = HOP_BY_HOP | {"accept-encoding"}

state = {"n": 0, "out": "captures"}


def _slug(path: str) -> str:
    s = re.sub(r"[^A-Za-z0-9]+", "_", path).strip("_")
    return s[:60] or "root"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _read_body(self) -> bytes:
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            return self.rfile.read(n)
        if (self.headers.get("Transfer-Encoding") or "").lower() == "chunked":
            buf = b""
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    break
                size = int(line.split(b";")[0], 16)
                if size == 0:
                    self.rfile.readline()
                    break
                buf += self.rfile.read(size)
                self.rfile.read(2)
            return buf
        return b""

    def _handle(self):
        body = self._read_body()
        state["n"] += 1
        seq = state["n"]
        ts = time.strftime("%H%M%S")

        headers = {k: v for k, v in self.headers.items()}
        rec = {
            "seq": seq,
            "ts": ts,
            "method": self.command,
            "path": self.path,
            "headers": headers,
            "body_len": len(body),
            "body": body.decode("utf-8", "replace"),
        }

        # 转发到真上游
        status, resp_headers, resp_body = None, {}, b""
        try:
            conn = HTTPSConnection(UPSTREAM_HOST, timeout=180)
            conn.putrequest(self.command, self.path, skip_host=True, skip_accept_encoding=True)
            for k, v in headers.items():
                if k.lower() in DROP_REQ:
                    continue
                conn.putheader(k, v)
            # skip_host=True 关掉了自动 Host，必须自己补，否则上游 400。
            conn.putheader("Host", UPSTREAM_HOST)
            conn.putheader("Content-Length", str(len(body)))
            conn.endheaders()
            if body:
                conn.send(body)
            resp = conn.getresponse()
            status = resp.status
            resp_headers = dict(resp.getheaders())
            resp_body = resp.read()
            conn.close()
        except Exception as e:  # 网络问题也要留证据
            rec["error"] = f"{type(e).__name__}: {e}"

        rec["upstream_status"] = status
        rec["upstream_headers"] = resp_headers
        rec["upstream_body"] = resp_body.decode("utf-8", "replace")[:8000]

        name = f"{ts}-{seq:03d}-{_slug(self.path)}.json"
        out = os.path.join(state["out"], name)
        os.makedirs(state["out"], exist_ok=True)
        with open(out, "w", encoding="utf-8") as f:
            json.dump(rec, f, ensure_ascii=False, indent=2)

        ua = headers.get("User-Agent") or headers.get("user-agent") or "-"
        model = ""
        try:
            model = json.loads(rec["body"]).get("model", "")
        except Exception:
            pass
        print(f"[{ts}] #{seq} {self.command} {self.path} model={model} "
              f"ua={ua} -> {status}", flush=True)

        if status is None:
            payload = json.dumps({"error": {"message": "capture proxy upstream error"}}).encode()
            self.send_response(502)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return

        self.send_response(status)
        for k, v in resp_headers.items():
            if k.lower() in HOP_BY_HOP:
                continue
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(resp_body)))
        self.end_headers()
        if resp_body:
            self.wfile.write(resp_body)

    do_GET = _handle
    do_POST = _handle
    do_PUT = _handle
    do_DELETE = _handle

    def log_message(self, *a):
        pass


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=19100)
    ap.add_argument("--out", default="captures")
    a = ap.parse_args()
    state["out"] = a.out
    os.makedirs(a.out, exist_ok=True)
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    print(f"capture proxy on http://127.0.0.1:{a.port} -> https://{UPSTREAM_HOST}  (out={a.out})",
          flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    sys.exit(main())

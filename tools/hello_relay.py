"""记录 TLS ClientHello 的 TCP 中继。

用途：把某个域名的 A 记录临时指到 127.0.0.1（/etc/hosts），让客户端连到这里；
本脚本把 ClientHello 明文抄下来，再把字节原样转给真实 IP。TLS 仍是端到端，
不涉及证书，也不需要 MITM CA。

用法（Linux，需要 root 才能监听 443）：

    sudo python3 tools/hello_relay.py <real_ip> [port]

输出每个连接一行：

    HELLO ciphers=1301,1302,... exts=0,10,11,... alpn=h2,http/1.1 versions=0304,0303
"""

import binascii
import socket
import sys
import threading

REAL_IP = sys.argv[1] if len(sys.argv) > 1 else ""
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 443


def parse_hello(b: bytes) -> str:
    if len(b) < 6 or b[0] != 0x16:
        return "not-a-handshake first=%s" % binascii.hexlify(b[:8]).decode()
    rec_len = int.from_bytes(b[3:5], "big")
    hs = b[5:5 + rec_len]
    if len(hs) < 4 or hs[0] != 1:
        return "not-clienthello type=%d" % (hs[0] if hs else -1)
    p = 4 + 2 + 32  # handshake header + legacy version + random
    if p >= len(hs):
        return "truncated"
    sid_len = hs[p]
    p += 1 + sid_len
    if p + 2 > len(hs):
        return "truncated-cs"
    cs_len = int.from_bytes(hs[p:p + 2], "big")
    p += 2
    ciphers = [hs[p + i:p + i + 2].hex() for i in range(0, cs_len, 2)]
    p += cs_len
    if p >= len(hs):
        return "ciphers=%s" % ",".join(ciphers)
    comp_len = hs[p]
    p += 1 + comp_len
    if p + 2 > len(hs):
        return "ciphers=%s" % ",".join(ciphers)
    ext_len = int.from_bytes(hs[p:p + 2], "big")
    p += 2
    end = min(p + ext_len, len(hs))
    exts, alpn, versions, groups = [], [], [], []
    while p + 4 <= end:
        et = int.from_bytes(hs[p:p + 2], "big")
        el = int.from_bytes(hs[p + 2:p + 4], "big")
        data = hs[p + 4:p + 4 + el]
        exts.append(et)
        if et == 16 and len(data) >= 2:  # ALPN
            q = 2
            while q < len(data):
                ln = data[q]
                alpn.append(data[q + 1:q + 1 + ln].decode("latin1"))
                q += 1 + ln
        elif et == 43 and len(data) >= 1:  # supported_versions
            q = 1
            while q + 1 < len(data):
                versions.append(data[q:q + 2].hex())
                q += 2
        elif et == 10 and len(data) >= 2:  # supported_groups
            q = 2
            while q + 1 < len(data):
                groups.append(data[q:q + 2].hex())
                q += 2
        p += 4 + el
    return "ciphers=%s exts=%s alpn=%s versions=%s groups=%s" % (
        ",".join(ciphers), ",".join(map(str, exts)),
        ",".join(alpn), ",".join(versions), ",".join(groups))


def pipe(src, dst):
    try:
        while True:
            d = src.recv(65536)
            if not d:
                break
            dst.sendall(d)
    except Exception:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except Exception:
            pass


def handle(c):
    s = None
    try:
        c.settimeout(15)
        data = b""
        while len(data) < 5:
            d = c.recv(4096)
            if not d:
                return
            data += d
        need = 5 + int.from_bytes(data[3:5], "big")
        while len(data) < need:
            d = c.recv(4096)
            if not d:
                break
            data += d
        print("HELLO " + parse_hello(data), flush=True)
        print("HELLO_HEX " + binascii.hexlify(data).decode(), flush=True)
        s = socket.create_connection((REAL_IP, 443), timeout=15)
        s.sendall(data)
        threading.Thread(target=pipe, args=(c, s), daemon=True).start()
        pipe(s, c)
    except Exception as e:
        print("ERR %s" % e, flush=True)
    finally:
        for x in (c, s):
            try:
                if x:
                    x.close()
            except Exception:
                pass


def main():
    if not REAL_IP:
        print("usage: hello_relay.py <real_ip> [port]", flush=True)
        return
    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("0.0.0.0", PORT))
    srv.listen(64)
    print("relay ready on :%d -> %s:443" % (PORT, REAL_IP), flush=True)
    while True:
        c, _ = srv.accept()
        threading.Thread(target=handle, args=(c,), daemon=True).start()


if __name__ == "__main__":
    main()

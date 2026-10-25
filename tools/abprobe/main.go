//go:build abprobe

// abprobe: 把同一份「真实 CLI 抓包请求」分别用不同传输层发出去，判定
// 上游免费层门禁到底看的是 TLS ClientHello 还是别的东西。
//
// 这个文件必须放进 opencode2api 模块树里编译（它 import 了该模块的
// internal/httpx），构建标签 abprobe 让 zen-direct 自己的 go build ./...
// 忽略它。CI 里由 bun-vs-utls.yml 复制到 /tmp/o2a/cmd/abprobe/。
//
// 用法（在 opencode2api 根目录）：
//
//	go run -tags abprobe ./cmd/abprobe \
//	  -mode utls -body ../zen-direct/tools/cli_capture_body.json \
//	  -session ses_xxx
//
// 变体：
//
//	utls    opencode2api 的 BunTransport：uTLS 复刻 Bun 的 ClientHello +
//	        fetch 的头顺序/大小写 + HTTP/1.1 only
//	stdlib  Go 标准库 TLS（crypto/tls），阴性对照
package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"opencode2api/internal/httpx"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func main() {
	mode := flag.String("mode", "utls", "utls|stdlib")
	url := flag.String("url", "https://opencode.ai/inference/v1/chat/completions", "upstream url")
	bodyPath := flag.String("body", "", "request body file")
	session := flag.String("session", os.Getenv("SES"), "X-Opencode-Session value")
	agent := flag.String("ua", "opencode/latest/2.0.29/cli", "User-Agent")
	flag.Parse()

	if *bodyPath == "" {
		die("abprobe: -body is required")
	}
	if *session == "" {
		die("abprobe: -session (or $SES) is required")
	}
	body, err := os.ReadFile(*bodyPath)
	if err != nil {
		die("abprobe: read body: %v", err)
	}

	var rt http.RoundTripper
	switch *mode {
	case "utls":
		// 与 opencode2api 生产配置同源：uTLS 握手 + 自带连接池。
		t, err := httpx.NewBunTransport(httpx.BunOptions{
			ConnectTimeout:        20 * time.Second,
			ResponseHeaderTimeout: 90 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		})
		if err != nil {
			die("abprobe: bun transport: %v", err)
		}
		rt = t
	case "stdlib":
		rt = &http.Transport{
			ForceAttemptHTTP2: false,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		}
	default:
		die("abprobe: unknown -mode %q", *mode)
	}

	req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
	if err != nil {
		die("abprobe: new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", *agent)
	req.Header.Set("X-Opencode-Client", "cli")
	req.Header.Set("X-Opencode-Project", "global")
	for _, name := range []string{
		"X-Opencode-Session", "X-Opencode-Session-Id", "X-Session-Affinity", "X-Session-Id",
	} {
		req.Header.Set(name, *session)
	}

	start := time.Now()
	resp, err := (&http.Client{Transport: rt, Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		fmt.Printf("AB %s ses=%s bytes=%d -> ERR %v (%dms)\n",
			*mode, *session, len(body), err, time.Since(start).Milliseconds())
		os.Exit(1)
	}
	defer resp.Body.Close()

	head := make([]byte, 240)
	n, _ := io.ReadFull(resp.Body, head)
	head = head[:n]
	text := strings.ToValidUTF8(string(head), "?")
	text = strings.ReplaceAll(text, "\n", " ")
	fmt.Printf("AB %s ses=%s bytes=%d -> %d %q retry-after=%q %dms\n",
		*mode, *session, len(body), resp.StatusCode, text,
		resp.Header.Get("Retry-After"), time.Since(start).Milliseconds())
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

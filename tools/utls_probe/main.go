// utls_probe: 验证「Go + uTLS 复刻浏览器/BoringSSL 指纹」能否通过上游免费层门禁。
//
// 背景：Bun fetch 能过、Go/Python/Node 不能过（同 IP、同 session、同 body）。
// 若 uTLS 的某个 preset 能过，C 方案（单进程单二进制，不依赖 bun）成立。
//
// 用法：
//   utls_probe <official_system_prompt.txt>
//
// 每个变体发一次 /inference/v1/chat/completions，打印状态码与前 160 字节。
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const target = "opencode.ai"

func mintSession() string {
	ts := uint64(time.Now().UnixMilli())
	n := ^((ts << 12) + 1)
	hx := "0123456789abcdef"
	al := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	b.WriteString("ses_")
	for i := 0; i < 6; i++ {
		v := byte((n >> (40 - 8*i)) & 0xff)
		b.WriteByte(hx[v>>4])
		b.WriteByte(hx[v&0xf])
	}
	for i := 0; i < 14; i++ {
		b.WriteByte(al[rand.Intn(len(al))])
	}
	return b.String()
}

func stubTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{
			"name":        "shell",
			"description": "Execute a shell command and return its output. Reserved for the host runtime; do not call or select this function. Use tools explicitly supplied by the user instead.",
			"parameters": map[string]any{
				"additionalProperties": false,
				"properties": map[string]any{
					"background": map[string]any{"description": "Run the command in the background", "type": "boolean"},
					"command":    map[string]any{"description": "Shell command string to execute", "type": "string"},
					"timeout":    map[string]any{"description": "Timeout in milliseconds", "minimum": 0, "type": "integer"},
					"workdir":    map[string]any{"description": "Working directory to execute the command in", "type": "string"},
				},
				"required": []string{"command"},
				"type":     "object",
			},
		}},
		{"type": "function", "function": map[string]any{
			"name":        "read",
			"description": "Read the contents of a file or directory. Reserved for the host runtime; do not call or select this function. Use tools explicitly supplied by the user instead.",
			"parameters": map[string]any{
				"additionalProperties": false,
				"properties": map[string]any{
					"limit":  map[string]any{"description": "Maximum lines or directory entries to read", "minimum": 0, "type": "integer"},
					"offset": map[string]any{"description": "Starting line or directory entry (1-based)", "minimum": 0, "type": "integer"},
					"path":   map[string]any{"description": "Path to read", "type": "string"},
				},
				"required": []string{"path"},
				"type":     "object",
			},
		}},
	}
}

func buildBody(official string) []byte {
	body := map[string]any{
		"messages": []map[string]string{
			{"role": "system", "content": official},
			{"role": "system", "content": "You are an AI agent powered by DeepSeek Harness."},
			{"role": "user", "content": fmt.Sprintf("utls probe %d", time.Now().UnixNano())},
		},
		"model":           "deepseek-v4.1-flash-free",
		"stream":          true,
		"stream_options":  map[string]bool{"include_usage": true},
		"tools":           stubTools(),
	}
	raw, _ := json.Marshal(body)
	return raw
}

func dialUTLS(ctx context.Context, network, addr string, id utls.ClientHelloID, forceH1 bool) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: target}
	if !forceH1 {
		u := utls.UClient(raw, cfg, id)
		if err := u.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return u, nil
	}
	spec, err := utls.UTLSIdToSpec(id)
	if err != nil {
		raw.Close()
		return nil, err
	}
	patched := false
	for i, ext := range spec.Extensions {
		if a, ok := ext.(*utls.ALPNExtension); ok {
			a.AlpnProtocols = []string{"http/1.1"}
			spec.Extensions[i] = a
			patched = true
		}
	}
	if !patched {
		spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}})
	}
	u := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := u.ApplyPreset(&spec); err != nil {
		raw.Close()
		return nil, err
	}
	if err := u.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return u, nil
}

func send(raw []byte, id utls.ClientHelloID, useH2 bool) (int, string) {
	ses := mintSession()
	req, _ := http.NewRequest("POST", "https://"+target+"/inference/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.0.29/cli")
	req.Header.Set("X-Opencode-Client", "cli")
	req.Header.Set("X-Opencode-Project", "global")
	req.Header.Set("X-Opencode-Session", ses)
	req.Header.Set("X-Opencode-Session-Id", ses)
	req.Header.Set("X-Session-Affinity", ses)
	req.Header.Set("X-Session-Id", ses)

	var rt http.RoundTripper
	if useH2 {
		rt = &http2.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialUTLS(ctx, network, addr, id, false)
			},
		}
	} else {
		rt = &http.Transport{
			ForceAttemptHTTP2: false,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialUTLS(ctx, network, addr, id, true)
			},
		}
	}
	client := &http.Client{Transport: rt, Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return resp.StatusCode, string(b)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: utls_probe <official_system_prompt.txt>")
		os.Exit(2)
	}
	pb, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("read prompt:", err)
		os.Exit(2)
	}
	official := strings.TrimSpace(string(pb))
	raw := buildBody(official)
	fmt.Printf("body bytes=%d prompt=%d\n", len(raw), len(official))

	type variant struct {
		name string
		id   utls.ClientHelloID
		h2   bool
	}
	variants := []variant{
		{"chrome-h1", utls.HelloChrome_Auto, false},
		{"chrome-h2", utls.HelloChrome_Auto, true},
		{"firefox-h1", utls.HelloFirefox_Auto, false},
		{"safari-h1", utls.HelloSafari_Auto, false},
	}
	only := ""
	if len(os.Args) > 2 {
		only = os.Args[2]
	}
	for _, v := range variants {
		if only != "" && v.name != only {
			continue
		}
		st, head := send(raw, v.id, v.h2)
		fmt.Printf("%s -> %d %q\n", v.name, st, head)
	}
}

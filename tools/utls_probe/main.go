// utls_probe: 验证「Go + uTLS 复刻上游认的 TLS 指纹」能否通过免费层门禁。
//
// 背景：同 IP、同 session、同 body 下，Bun fetch 200，Go/Python/Node 429。
// 第一轮用 uTLS 内置 preset（Chrome/Firefox/Safari）全部 429——Bun 的 ClientHello
// 与这些 preset 都不同。第二轮按抓到的字节手工拼 spec（bun-h1 / bun-h2）。
//
// 用法：
//   utls_probe <official_system_prompt.txt> [variant]
//
// 变体：bun-h2 / bun-h1 / chrome-h1 / chrome-h2 / firefox-h1 / safari-h1
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
		"model":          "deepseek-v4.1-flash-free",
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
		"tools":          stubTools(),
	}
	raw, _ := json.Marshal(body)
	return raw
}

// bunSpec 复刻 2026-10-24 抓到的 Bun fetch ClientHello（BoringSSL）：
//
//	ciphers 1302,1303,1301,c02c,c030,009f,cca9,cca8,ccaa,c02b,c02f,009e,c024,c028,
//	        006b,c023,c027,0067,c00a,c014,0039,c009,c013,0033,009d,009c,003d,003c,
//	        0035,002f,00ff
//	exts    0,11,10,16,22,23,49,13,43,45,51,21
//	alpn    h2,http/1.1
//	groups  001d,0017,001e,0019,0018,0100..0104
//	padding 补齐到 512 字节
func bunSpec(alpn []string) *utls.ClientHelloSpec {
	curves := []utls.CurveID{
		utls.X25519, utls.CurveP256, utls.CurveID(0x001e), utls.CurveP521, utls.CurveP384,
		utls.CurveID(0x0100), utls.CurveID(0x0101), utls.CurveID(0x0102),
		utls.CurveID(0x0103), utls.CurveID(0x0104),
	}
	sigs := []utls.SignatureScheme{
		0x0403, 0x0503, 0x0603, 0x0807, 0x0808, 0x0809, 0x080a, 0x080b,
		0x0804, 0x0805, 0x0806, 0x0401, 0x0501, 0x0601, 0x0303, 0x0301,
		0x0302, 0x0402, 0x0502, 0x0602,
	}
	return &utls.ClientHelloSpec{
		TLSVersMin: 0x0303,
		TLSVersMax: 0x0304,
		CipherSuites: []uint16{
			0x1302, 0x1303, 0x1301, 0xc02c, 0xc030, 0x009f, 0xcca9, 0xcca8,
			0xccaa, 0xc02b, 0xc02f, 0x009e, 0xc024, 0xc028, 0x006b, 0xc023,
			0xc027, 0x0067, 0xc00a, 0xc014, 0x0039, 0xc009, 0xc013, 0x0033,
			0x009d, 0x009c, 0x003d, 0x003c, 0x0035, 0x002f, 0x00ff,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{ServerName: target},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SupportedCurvesExtension{Curves: curves},
			&utls.ALPNExtension{AlpnProtocols: alpn},
			&utls.GenericExtension{Id: 22}, // encrypt_then_mac
			&utls.UtlsExtendedMasterSecretExtension{},
			&utls.GenericExtension{Id: 49}, // post_handshake_auth
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: sigs},
			&utls.SupportedVersionsExtension{Versions: []uint16{0x0304, 0x0303}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// presetH1 把某个 preset 的 ALPN 改成 http/1.1，避免协商出 h2 却用 h1 说话。
func presetH1(id utls.ClientHelloID) func() *utls.ClientHelloSpec {
	return func() *utls.ClientHelloSpec {
		spec, err := utls.UTLSIdToSpec(id)
		if err != nil {
			return nil
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
		return &spec
	}
}

func dialWith(ctx context.Context, network, addr string, id utls.ClientHelloID, specFn func() *utls.ClientHelloSpec) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: target}
	var u *utls.UConn
	if specFn != nil {
		spec := specFn()
		if spec == nil {
			raw.Close()
			return nil, fmt.Errorf("empty spec")
		}
		u = utls.UClient(raw, cfg, utls.HelloCustom)
		if err := u.ApplyPreset(spec); err != nil {
			raw.Close()
			return nil, err
		}
	} else {
		u = utls.UClient(raw, cfg, id)
	}
	if err := u.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return u, nil
}

type variant struct {
	name        string
	id          utls.ClientHelloID
	specFn      func() *utls.ClientHelloSpec
	h2          bool
	fullHeaders bool
}

func send(raw []byte, v variant) (int, string) {
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
	if v.fullHeaders {
		// Bun fetch 自动加的三个头；门禁可能做头一致性检查。
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
		if !v.h2 {
			req.Header.Set("Connection", "keep-alive")
		}
	}

	var rt http.RoundTripper
	if v.h2 {
		rt = &http2.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialWith(ctx, network, addr, v.id, v.specFn)
			},
		}
	} else {
		rt = &http.Transport{
			ForceAttemptHTTP2: false,
			DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialWith(ctx, network, addr, v.id, v.specFn)
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
		fmt.Println("usage: utls_probe <official_system_prompt.txt> [variant]")
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

	variants := []variant{
		{"bun-h2", utls.HelloCustom, func() *utls.ClientHelloSpec { return bunSpec([]string{"h2", "http/1.1"}) }, true, false},
		{"bun-h1", utls.HelloCustom, func() *utls.ClientHelloSpec { return bunSpec([]string{"http/1.1"}) }, false, false},
		{"bun-h1-full", utls.HelloCustom, func() *utls.ClientHelloSpec { return bunSpec([]string{"http/1.1"}) }, false, true},
		{"bun-h2-full", utls.HelloCustom, func() *utls.ClientHelloSpec { return bunSpec([]string{"h2", "http/1.1"}) }, true, true},
		{"chrome-h1", utls.HelloChrome_Auto, presetH1(utls.HelloChrome_Auto), false, false},
		{"chrome-h2", utls.HelloChrome_Auto, nil, true, false},
		{"firefox-h1", utls.HelloFirefox_Auto, presetH1(utls.HelloFirefox_Auto), false, false},
		{"safari-h1", utls.HelloSafari_Auto, presetH1(utls.HelloSafari_Auto), false, false},
	}
	only := ""
	if len(os.Args) > 2 {
		only = os.Args[2]
	}
	for _, v := range variants {
		if only != "" && v.name != only {
			continue
		}
		st, head := send(raw, v)
		fmt.Printf("%s -> %d %q\n", v.name, st, head)
	}
}

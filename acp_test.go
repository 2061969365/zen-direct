package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeAgent 读客户端两帧，按剧本回包。
func fakeAgent(t *testing.T, r io.Reader, w io.Writer, script func(id int64, method string) any) {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var m acpEnvelope
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Errorf("agent 收到非法帧: %v", err)
			return
		}
		out := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": script(m.ID, m.Method)}
		if err := enc.Encode(out); err != nil {
			return
		}
	}
}

func pipePair(t *testing.T) (io.Writer, *bufio.Scanner) {
	t.Helper()
	cr, cw := io.Pipe()
	ar, aw := io.Pipe()
	t.Cleanup(func() { cw.Close(); aw.Close() })
	go fakeAgent(t, cr, aw, func(id int64, method string) any {
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}}
		case "session/new":
			return map[string]any{"sessionId": "ses_test_fake001"}
		}
		return map[string]any{}
	})
	sc := bufio.NewScanner(ar)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	return cw, sc
}

// 握手 + session/new 走通，返回真 session。
func TestAcpExchangeMint(t *testing.T) {
	w, sc := pipePair(t)
	sid, err := acpExchange(w, sc, 5*time.Second, `C:\tmp`)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if sid != "ses_test_fake001" {
		t.Fatalf("应得假 session，得 %q", sid)
	}
}

// agent 通知帧穿插在回包前：跳过不认错。
func TestAcpExchangeSkipsNoise(t *testing.T) {
	cr, cw := io.Pipe()
	ar, aw := io.Pipe()
	defer cw.Close()
	defer aw.Close()
	go func() {
		sc := bufio.NewScanner(cr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		enc := json.NewEncoder(aw)
		for sc.Scan() {
			var m acpEnvelope
			_ = json.Unmarshal(sc.Bytes(), &m)
			if m.Method == "initialize" {
				// 先塞一条通知 + 一行废话，再回正经包。
				_, _ = aw.Write([]byte(`{"jsonrpc":"2.0","method":"session/update","params":{}}` + "\n"))
				_, _ = aw.Write([]byte("some log line leaked to stdout\n"))
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"protocolVersion": 1}})
			} else {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"sessionId": "ses_noise_ok"}})
			}
		}
	}()
	sc := bufio.NewScanner(ar)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	sid, err := acpExchange(cw, sc, 5*time.Second, `C:\tmp`)
	if err != nil || sid != "ses_noise_ok" {
		t.Fatalf("噪声应跳过，得 %q err=%v", sid, err)
	}
}

// 回包无 sessionId：报错不吞。
func TestAcpExchangeRejectsBadSession(t *testing.T) {
	cr, cw := io.Pipe()
	ar, aw := io.Pipe()
	defer cw.Close()
	defer aw.Close()
	go func() {
		sc := bufio.NewScanner(cr)
		enc := json.NewEncoder(aw)
		for sc.Scan() {
			var m acpEnvelope
			_ = json.Unmarshal(sc.Bytes(), &m)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"result": map[string]any{"nope": 1}})
		}
	}()
	sc := bufio.NewScanner(ar)
	if _, err := acpExchange(cw, sc, 5*time.Second, `C:\tmp`); err == nil {
		t.Fatal("无 sessionId 应报错")
	}
}

// agent 永远不回：超时退出不 hanging。
func TestAcpExchangeTimeout(t *testing.T) {
	cr, cw := io.Pipe()
	defer cw.Close()
	ar, aw := io.Pipe()
	defer aw.Close()
	defer ar.Close()
	go func() {
		sc := bufio.NewScanner(cr)
		for sc.Scan() {
		}
	}()
	sc := bufio.NewScanner(ar)
	t0 := time.Now()
	if _, err := acpExchange(cw, sc, 300*time.Millisecond, `C:\tmp`); err == nil {
		t.Fatal("应超时报错")
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("超时拖太久: %v", d)
	}
}

// 二进制发现：空 PATH/APPDATA 下应返回空（不捏造路径）。
func TestAcpBinaryPathEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("OPENCODE_BIN", "")
	if got := acpBinaryPath(); got != "" {
		t.Fatalf("无货应返回空，得 %q", got)
	}
}

// PATH 上的真 .exe 收；Windows 下 .cmd 垫片拒收（Go 起不来）。
func TestAcpBinaryPathShim(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("OPENCODE_BIN", "")
	name := "opencode"
	if runtime.GOOS == "windows" {
		name = "opencode.exe"
	}
	exe := filepath.Join(dir, name)
	if err := os.WriteFile(exe, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := acpBinaryPath(); got != exe {
		t.Fatalf("应收真 exe，得 %q", got)
	}
	if runtime.GOOS != "windows" {
		return
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.cmd"), []byte("@echo off"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := acpBinaryPath(); got != "" {
		t.Fatalf(".cmd 垫片应拒收，得 %q", got)
	}
}
// 非 ses_ 前缀拒收（上游只认 ses_，错值入库等于埋雷）。
func TestAcpExchangeRejectsPrefix(t *testing.T) {
	cr, cw := io.Pipe()
	ar, aw := io.Pipe()
	defer cw.Close()
	defer aw.Close()
	go func() {
		sc := bufio.NewScanner(cr)
		enc := json.NewEncoder(aw)
		for sc.Scan() {
			var m acpEnvelope
			_ = json.Unmarshal(sc.Bytes(), &m)
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"result": map[string]any{"sessionId": "msg_not_a_session"}})
		}
	}()
	sc := bufio.NewScanner(ar)
	if sid, err := acpExchange(cw, sc, 5*time.Second, `C:\tmp`); err == nil || sid != "" {
		t.Fatalf("错前缀应报错清零，得 %q err=%v", sid, err)
	}
}

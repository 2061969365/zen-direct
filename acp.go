package main

// acp.go: genuine session minter via local `opencode acp` child (NDJSON JSON-RPC).
//
// Upstream FreeTier gate only honors sessions created by a genuine opencode
// client (backend allowlist; minted ses_ IDs get 403). Spawning `opencode acp`
// + session/new mints a backend-known session with zero prior zen traffic.
//
// Spawning costs ~1.2s cold and is paid once per unseen downstream fp, so we
// use spawn-per-mint: no supervision state, no pidfiles, no orphaned children,
// no risk of killing the user's own ACP processes. Concurrent mints serialize
// on acpMu (bursts queue instead of spawning N x 200MB children).
//
// Chain in pool Borrow/Rotate: service API -> harvest file -> ACP spawn ->
// mintID fallback. Canonical order: service (ms, 1:1 fresh) first, harvest
// (instant, shared) second, ACP (slow spawn) third, mint (fake, 403) last.
// ACP_OFF=1 disables ACP entirely (pure harvest behavior).

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// acpMintFunc indirection: production mints via child, tests stub it.
// resetPoolForTest installs the "" stub so unit tests never spawn opencode.
var acpMintFunc = acpMintSession

var acpMu sync.Mutex

// acpFailLogUnix last ACP failure log time (throttle: at most one line/min).
var acpFailLogUnix int64

func acpDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ACP_OFF"))) {
	case "1", "true", "yes", "off":
		return true
	}
	return false
}

func acpTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ACP_TIMEOUT_SEC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 3 && n <= 120 {
			return time.Duration(n) * time.Second
		}
	}
	return 25 * time.Second
}

// acpBinaryPath locates the genuine opencode binary:
// OPENCODE_BIN env -> PATH -> npm-global default (Windows).
func acpBinaryPath() string {
	if v := strings.TrimSpace(os.Getenv("OPENCODE_BIN")); v != "" {
		return v
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		// Windows 上 PATH 里多半是 opencode.cmd/.ps1 垫片，Go 不能直接
		// CreateProcess，必须是真 .exe 才收；否则继续找 npm 全局真 exe。
		if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(p), ".exe") {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			p := filepath.Join(appdata, "npm", "node_modules",
				"@opencode", "cli", "bin", "opencode.exe")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// acpCwd: new sessions land under this directory's project.
// Default groups them under the gateway's own dir, away from work projects.
func acpCwd() string {
	if v := strings.TrimSpace(os.Getenv("ACP_CWD")); v != "" {
		_ = os.MkdirAll(v, 0755)
		return v
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return os.TempDir()
}

func acpLogFail(format string, args ...any) {
	now := time.Now().Unix()
	last := atomic.LoadInt64(&acpFailLogUnix)
	if now-last < 60 {
		return
	}
	atomic.StoreInt64(&acpFailLogUnix, now)
	log.Printf(format, args...)
}

// cappedBuffer 并发安全截断缓冲（收子进程 stderr 诊断用）。
type cappedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) < c.max {
		c.buf = append(c.buf, p...)
		if len(c.buf) > c.max {
			c.buf = c.buf[:c.max]
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}
type acpEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method,omitempty"`
	Params  any    `json:"params,omitempty"`
	Result  struct {
		SessionID string `json:"sessionId"`
	} `json:"result,omitempty"`
}

// acpExchange runs initialize + session/new over an NDJSON stream.
// Returns the fresh genuine session ID or an error. No process management.
// Scanner runs in a feeder goroutine so deadline always fires, even when the
// agent goes silent mid-handshake.
func acpExchange(w io.Writer, sc *bufio.Scanner, timeout time.Duration, cwd string) (string, error) {
	type lineRes struct {
		line []byte
		ok   bool
	}
	lines := make(chan lineRes, 64)
	go func() {
		defer close(lines)
		for sc.Scan() {
			cp := append([]byte(nil), sc.Bytes()...)
			lines <- lineRes{line: cp, ok: true}
		}
	}()
	deadline := time.Now().Add(timeout)
	send := func(id int64, method string, params any) error {
		b, err := json.Marshal(acpEnvelope{JSONRPC: "2.0", ID: id, Method: method, Params: params})
		if err != nil {
			return err
		}
		b = append(b, '\n')
		_, err = w.Write(b)
		return err
	}
	call := func(id int64, method string, params any) (acpEnvelope, error) {
		var out acpEnvelope
		if err := send(id, method, params); err != nil {
			return out, err
		}
		for {
			rest := time.Until(deadline)
			if rest <= 0 {
				return out, io.ErrUnexpectedEOF
			}
			t := time.NewTimer(rest)
			select {
			case lr, ok := <-lines:
				t.Stop()
				if !ok {
					return out, io.ErrUnexpectedEOF
				}
				if !lr.ok || len(bytesTrimSpace(lr.line)) == 0 {
					continue
				}
				var m acpEnvelope
				if err := json.Unmarshal(lr.line, &m); err != nil {
					continue // agent log line leaked to stdout: skip
				}
				if m.ID != id {
					continue // notification or unrelated response: skip
				}
				return m, nil
			case <-t.C:
				return out, io.ErrUnexpectedEOF
			}
		}
	}
	if _, err := call(1, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
	}); err != nil {
		return "", err
	}
	m, err := call(2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(m.Result.SessionID, "ses_") {
		return "", io.ErrUnexpectedEOF
	}
	return m.Result.SessionID, nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// acpMintSession spawns `opencode acp`, mints one genuine session, kills the
// child. "" on any failure (disabled / binary missing / timeout / protocol).
func acpMintSession() string {
	if acpDisabled() {
		return ""
	}
	bin := acpBinaryPath()
	if bin == "" {
		acpLogFail("[acp] binary not found (OPENCODE_BIN/PATH/npm-global)")
		return ""
	}
	acpMu.Lock()
	defer acpMu.Unlock()
	t0 := time.Now()
	cmd := exec.Command(bin, "acp")
	cmd.Dir = acpCwd()
	cmd.Env = os.Environ()
	hideAcpWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		acpLogFail("[acp] stdin pipe: %v", err)
		return ""
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		acpLogFail("[acp] stdout pipe: %v", err)
		return ""
	}
	stderr := &cappedBuffer{max: 8 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		acpLogFail("[acp] start %s: %v", bin, err)
		return ""
	}
	// Guaranteed reaping: timeout kill + Wait on every exit path.
	type acpRes struct {
		sid string
		err error
	}
	done := make(chan acpRes, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		sid, err := acpExchange(stdin, sc, acpTimeout(), cmd.Dir)
		done <- acpRes{sid: sid, err: err}
	}()
	var res acpRes
	timedOut := false
	select {
	case res = <-done:
	case <-time.After(acpTimeout() + 5*time.Second):
		res = acpRes{err: io.ErrUnexpectedEOF}
		timedOut = true
	}
	_ = cmd.Process.Kill()
	waitErr := cmd.Wait()
	if res.sid == "" {
		acpLogFail("[acp] mint failed: %v timeout=%v exit=%v stderr=%.300s",
			res.err, timedOut, waitErr, stderr.String())
		return ""
	}
	// 同 svc.go：日志里的 session id 一律全长，便于直接抄进毒名单。
	log.Printf("[acp] minted %s in %dms", res.sid, time.Since(t0).Milliseconds())
	return res.sid
}

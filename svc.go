package main

// svc.go: genuine session minter via the RUNNING opencode background service.
//
// POST {service}/api/session {"title":"zen-direct pool"} with HTTP Basic auth
// (user opencode / password from service.json or OPENCODE_SERVER_PASSWORD)
// returns a backend-known session in milliseconds: no child process, no log
// lock contention, no orphans. First choice in the pool chain:
// service API -> harvest file -> ACP spawn -> mintID fallback.
//
// Discovery: OPENCODE_SERVICE_URL env pins the address; otherwise parse
// `opencode service status` once and cache (re-run on connection failure).
// SVC_OFF=1 disables the service path entirely.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// svcMintFunc indirection: production calls the service, tests stub it.
var svcMintFunc = svcMintSession

var (
	svcMu       sync.Mutex
	svcCached   string
	svcFailUnix int64
)

func svcDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SVC_OFF"))) {
	case "1", "true", "yes", "off":
		return true
	}
	return false
}

func svcLogFail(format string, args ...any) {
	now := time.Now().Unix()
	if now-atomic.LoadInt64(&svcFailUnix) < 60 {
		return
	}
	atomic.StoreInt64(&svcFailUnix, now)
	log.Printf(format, args...)
}

// svcPassword mirrors server auth.ts: OPENCODE_SERVER_PASSWORD env first,
// then ~/.config/opencode/service.json {"password": ...}.
func svcPassword() string {
	if v := strings.TrimSpace(os.Getenv("OPENCODE_SERVER_PASSWORD")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "service.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	pw, _ := m["password"].(string)
	return strings.TrimSpace(pw)
}

func svcUsername() string {
	if v := strings.TrimSpace(os.Getenv("OPENCODE_SERVER_USERNAME")); v != "" {
		return v
	}
	return "opencode"
}

func svcBasicAuth() string {
	pw := svcPassword()
	if pw == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(svcUsername()+":"+pw))
}

// svcDiscover runs `opencode service status` and parses the first http URL.
func svcDiscover() string {
	bin := acpBinaryPath()
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "service", "status")
	hideAcpWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "http://127.0.0.1:") || strings.HasPrefix(f, "http://localhost:") {
			return strings.TrimRight(f, "/")
		}
	}
	return ""
}

// svcBase returns the cached service address, discovering once on demand.
// OPENCODE_SERVICE_URL pins it but must stay loopback: the Basic password
// would otherwise travel off-host in cleartext over plain HTTP.
func svcBase() string {
	if v := strings.TrimSpace(os.Getenv("OPENCODE_SERVICE_URL")); v != "" {
		u := strings.TrimRight(v, "/")
		if strings.HasPrefix(u, "http://127.0.0.1:") ||
			strings.HasPrefix(u, "http://localhost:") ||
			strings.HasPrefix(u, "http://[::1]:") {
			return u
		}
		svcLogFail("[svc] refuse non-loopback OPENCODE_SERVICE_URL")
		return ""
	}
	svcMu.Lock()
	defer svcMu.Unlock()
	if svcCached == "" {
		svcCached = svcDiscover()
		if svcCached == "" {
			svcLogFail("[svc] discover: no service address (service down?)")
		}
	}
	return svcCached
}

// svcInvalidate drops the cached address (next call re-discovers).
func svcInvalidate() {
	svcMu.Lock()
	svcCached = ""
	svcMu.Unlock()
}

var svcHTTP = &http.Client{Timeout: 15 * time.Second}

// svcMintSession creates one genuine session via the background service.
// "" on any failure (disabled / undiscoverable / unauthenticated / timeout).
func svcMintSession() string {
	if svcDisabled() {
		return ""
	}
	base := svcBase()
	auth := svcBasicAuth()
	if base == "" || auth == "" {
		svcLogFail("[svc] no credentials: base=%q auth=%v", shortStr(base, 40), auth != "")
		return ""
	}
	t0 := time.Now()
	body, _ := json.Marshal(map[string]string{"title": "zen-direct pool"})
	req, err := http.NewRequest("POST", base+"/api/session", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := svcHTTP.Do(req)
	if err != nil {
		svcInvalidate() // port may have rotated with a service restart
		svcLogFail("[svc] POST %s/api/session: %v", base, err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		svcLogFail("[svc] POST %s/api/session: http %d", base, resp.StatusCode)
		return ""
	}
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		svcLogFail("[svc] decode: %v", err)
		return ""
	}
	if !strings.HasPrefix(out.Data.ID, "ses_") {
		svcLogFail("[svc] bad session id %q", out.Data.ID)
		return ""
	}
	// 打全长 id：截断日志是毒名单抄错的源头（2026-10-23 事故），日志里的
	// session id 必须能直接抄进 poisoned_sessions.txt。
	log.Printf("[svc] minted %s in %dms", out.Data.ID, time.Since(t0).Milliseconds())
	return out.Data.ID
}

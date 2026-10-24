package main

// sidecar_test.go：内嵌 sidecar 的资产、路径决策、真实 spawn 与提示词注入。
// 需要 bun 的用例在没有 bun 的机器/CI 上跳过（不碰上游，不烧额度）。

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEmbeddedSidecarAssets(t *testing.T) {
	if len(embeddedSidecarJS) < 1000 {
		t.Fatalf("内嵌 JS 太小: %d bytes", len(embeddedSidecarJS))
	}
	js := string(embeddedSidecarJS)
	for _, want := range []string{"Bun.serve", "ZEN_IDLE_EXIT_MS", "official_system_prompt"} {
		if !strings.Contains(js, want) {
			t.Errorf("内嵌 JS 缺 %q", want)
		}
	}
	prompt := strings.TrimSpace(string(embeddedSidecarPrompt))
	if len(prompt) < 5000 {
		t.Fatalf("官方提示词太小: %d chars", len(prompt))
	}
	if !strings.HasPrefix(prompt, "You are") {
		t.Errorf("官方提示词开头不对: %q", shortStr(prompt, 40))
	}
}

func TestSidecarBaseURL(t *testing.T) {
	cases := []struct {
		port       int
		path, want string
	}{
		{19200, "/inference/v1", "http://127.0.0.1:19200/inference/v1"},
		{19200, "/inference/v1/", "http://127.0.0.1:19200/inference/v1"},
		{19200, "", "http://127.0.0.1:19200/inference/v1"},
		{19200, "/v1", "http://127.0.0.1:19200/v1"},
	}
	for _, c := range cases {
		if got := sidecarBaseURL(c.port, c.path); got != c.want {
			t.Errorf("sidecarBaseURL(%d,%q)=%q want %q", c.port, c.path, got, c.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	yes := []string{"127.0.0.1", "127.0.0.5", "localhost", "LOCALHOST", "::1", "[::1]", "app.localhost"}
	no := []string{"", "opencode.ai", "10.0.0.1", "127.0.0.1.nip.io"}
	for _, h := range yes {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q)=false, want true", h)
		}
	}
	for _, h := range no {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q)=true, want false", h)
		}
	}
}

// 升级后第一次启动要重写，内容没变时不能反复落盘（bun 可能正读着）。
func TestWriteIfChangedSkipsIdentical(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.js")
	changed, err := writeIfChanged(p, []byte("v1"))
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, past, past); err != nil {
		t.Fatal(err)
	}
	changed, err = writeIfChanged(p, []byte("v1"))
	if err != nil || changed {
		t.Fatalf("identical write: changed=%v err=%v", changed, err)
	}
	if st, _ := os.Stat(p); !st.ModTime().Equal(past) {
		t.Errorf("内容相同却重写了: mtime=%v want %v", st.ModTime(), past)
	}
	changed, err = writeIfChanged(p, []byte("v2"))
	if err != nil || !changed {
		t.Fatalf("changed write: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "v2" {
		t.Errorf("内容=%q want v2", b)
	}
}

func TestSidecarDirsFallback(t *testing.T) {
	t.Setenv("ZEN_SIDECAR_DIR", `C:\explicit\dir`)
	if got := sidecarDirs(); len(got) != 1 || got[0] != `C:\explicit\dir` {
		t.Errorf("显式目录应独占候选: %v", got)
	}
	t.Setenv("ZEN_SIDECAR_DIR", "")
	dirs := sidecarDirs()
	if len(dirs) < 2 {
		t.Fatalf("候选太少: %v", dirs)
	}
	if !strings.HasSuffix(dirs[len(dirs)-1], "zen-direct-sidecar") {
		t.Errorf("最后一个候选应是 TEMP 兜底: %v", dirs)
	}
}

// 路径被普通文件占住时必须判为不可写（MkdirAll 会报错）。
func TestWritableDirRejectsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if writableDir(p) {
		t.Error("文件路径被判成可写目录")
	}
	if !writableDir(filepath.Join(t.TempDir(), "ok")) {
		t.Error("临时目录应可写")
	}
}

func TestPrepareSidecarFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZEN_SIDECAR_DIR", dir)
	got, js, prompt, err := prepareSidecarFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("dir=%q want %q", got, dir)
	}
	for _, p := range []string{js, prompt} {
		if st, err := os.Stat(p); err != nil || st.Size() == 0 {
			t.Fatalf("%s 没落盘: %v", p, err)
		}
	}
	b, _ := os.ReadFile(js)
	if string(b) != string(embeddedSidecarJS) {
		t.Error("JS 内容与内嵌不一致")
	}
}

func TestBunBinaryEnvOverride(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "bun.exe")
	if err := os.WriteFile(fake, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZEN_BUN", fake)
	if got := bunBinary(); got != fake {
		t.Errorf("bunBinary()=%q want %q", got, fake)
	}
	// 指向不存在的路径时不能原样返回（要看后续探测），至少不能等于假路径。
	t.Setenv("ZEN_BUN", filepath.Join(t.TempDir(), "nope.exe"))
	if got := bunBinary(); got == fake {
		t.Errorf("bunBinary() 仍返回不存在的 ZEN_BUN: %q", got)
	}
}

// 三种上游决策：off 直连、loopback 视为外部 sidecar、远端才内嵌。
func TestSidecarUpstreamDecisions(t *testing.T) {
	const remote = "https://opencode.ai/inference/v1"
	t.Setenv("OPENCODE_ZEN_BASE", remote)

	t.Setenv("DIRECT_SIDECAR", "off")
	if got := sidecarUpstream(remote); got != remote {
		t.Errorf("off: got %q want %q", got, remote)
	}

	t.Setenv("DIRECT_SIDECAR", "on")
	// 外部 sidecar 地址：原样返回，且不会 spawn 内嵌进程。
	external := "http://127.0.0.1:19200/inference/v1"
	if got := sidecarUpstream(external); got != external {
		t.Errorf("loopback: got %q want %q", got, external)
	}
	if sidecar.pidNow() != 0 {
		t.Error("loopback base 不该启动内嵌 sidecar")
	}
}

// 默认每请求新建连接：连续两发在（假）上游看来必须是两个不同的客户端端口。
// 这是"429 后换代理节点能立刻换出口 IP"的前提。
func TestSidecarNewConnPerRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if bunBinary() == "" {
		t.Skip("no bun on this machine")
	}

	var mu sync.Mutex
	var ports []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, port, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			mu.Lock()
			ports = append(ports, port)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	// 先建临时目录再注册停止：t.Cleanup 是 LIFO，目录清理必须先于 stop 注册，
	// 否则会在 bun 还活着的时候删目录（Windows 上文件被占用直接失败）。
	dir := t.TempDir()
	old := sidecar
	sidecar = newSidecarProc()
	t.Cleanup(func() {
		sidecar.stop()
		sidecar = old
	})
	t.Setenv("ZEN_SIDECAR_DIR", dir)
	t.Setenv("ZEN_SIDECAR_PORT", "")
	t.Setenv("ZEN_SIDECAR_KEEPALIVE", "") // 默认：不复用

	base, err := startSidecar(up.URL, "/inference/v1")
	if err != nil {
		t.Fatalf("startSidecar: %v", err)
	}
	for i := 0; i < 2; i++ {
		resp, err := sidecar.client.Post(base+"/chat/completions", "application/json",
			strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ports) != 2 {
		t.Fatalf("上游收到 %d 个请求，期望 2", len(ports))
	}
	if ports[0] == ports[1] {
		t.Fatalf("连接被复用（两次都是端口 %s），keepalive:false 没生效", ports[0])
	}
}

// 真 spawn：bun 起在随机端口 -> /health -> 注入官方提示词 -> 转发到假上游。
// 全程不出本机，不碰 opencode.ai。
func TestSidecarSpawnInjectsPrompt(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if bunBinary() == "" {
		t.Skip("no bun on this machine")
	}

	var gotBody []byte
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()

	old := sidecar
	sidecar = newSidecarProc()
	dir := t.TempDir() // 先建目录（LIFO 清理顺序，见上）
	t.Cleanup(func() {
		sidecar.stop()
		sidecar = old
	})
	t.Setenv("ZEN_SIDECAR_DIR", dir)
	t.Setenv("ZEN_SIDECAR_PORT", "")

	base, err := startSidecar(up.URL, "/inference/v1")
	if err != nil {
		t.Fatalf("startSidecar: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "127.0.0.1" || u.Port() == "" {
		t.Fatalf("base 不是本机地址: %s", base)
	}

	// /health 就绪
	resp, err := sidecar.client.Get("http://127.0.0.1:" + u.Port() + "/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var hm map[string]any
	json.NewDecoder(resp.Body).Decode(&hm)
	resp.Body.Close()
	if hm["status"] != "ok" {
		t.Fatalf("health=%v", hm)
	}
	if n, _ := hm["prompt"].(float64); n < 5000 {
		t.Errorf("sidecar 侧提示词长度=%v", hm["prompt"])
	}

	// 聊天请求：上游必须收到「官方提示词 + 原 messages」
	body := `{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err = sidecar.client.Post(base+"/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sidecar 转发 http %d", resp.StatusCode)
	}
	if gotPath != "/inference/v1/chat/completions" {
		t.Errorf("上游路径=%q", gotPath)
	}
	var fwd struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &fwd); err != nil {
		t.Fatalf("上游体不是 JSON: %v (%q)", err, shortStr(string(gotBody), 120))
	}
	if len(fwd.Messages) != 2 {
		t.Fatalf("上游 messages=%d want 2", len(fwd.Messages))
	}
	if fwd.Messages[0].Role != "system" ||
		!strings.HasPrefix(fwd.Messages[0].Content, strings.TrimSpace(string(embeddedSidecarPrompt))[:80]) {
		t.Errorf("messages[0] 不是官方提示词: role=%q head=%q", fwd.Messages[0].Role, shortStr(fwd.Messages[0].Content, 60))
	}
	if fwd.Messages[1].Content != "hi" {
		t.Errorf("原 user 消息被改: %q", fwd.Messages[1].Content)
	}

	// stop 之后端口必须关掉（进程真的没了）
	sidecar.stop()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sidecar.client.Get("http://127.0.0.1:" + u.Port() + "/health"); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("stop 之后 /health 还能通，bun 没被收掉")
}

package main

// sidecar.go：内嵌 Bun sidecar（A 方案，2026-10-24）。
//
// 为什么需要它：上游免费层按客户端特征发额度。云端实测（tools/utls_probe，
// 4 个干净 IP、多轮）Go/Python/Node 一律 FreeUsageLimitError，连字节级复刻
// Bun 的 ClientHello + Bun 默认头都过不了；能过的只有两件事同时成立：
//   1) 传输层是 Bun fetch（BoringSSL）
//   2) messages[0] 是 OpenCode 官方 system 提示词
// 这两件事 Go 侧都复刻不了，所以放进一个 Bun 子进程。
//
// 数据流（发布物仍然只有一个 exe）：
//
//	客户端 -> zen-direct.exe:8081 -> 127.0.0.1:<端口>(bun zen_sidecar.js) -> opencode.ai
//
// zen_sidecar.js 与官方提示词用 go:embed 内嵌，启动时释放到
// %LOCALAPPDATA%\zen-direct\sidecar\，然后 spawn bun、等 /health、把 upstream
// 指过去。bun 退出自动重启（指数退避，封顶 30s）；网关正常退出时收掉子进程，
// 网关被强杀时子进程靠 /health 心跳超时自杀（ZEN_IDLE_EXIT_MS）。
//
// 开关：
//
//	DIRECT_SIDECAR=off        不启用内嵌 sidecar（直连上游，免费层会 429）
//	ZEN_BUN=<path>            指定 bun（默认 PATH / ~/.bun / APPDATA npm 里找）
//	ZEN_SIDECAR_PORT=<port>   固定端口（默认每次启动挑一个空闲端口）
//	ZEN_SIDECAR_DIR=<dir>     释放目录（默认 %LOCALAPPDATA%\zen-direct\sidecar）
//	OPENCODE_ZEN_BASE=<url>   远端上游；指向 loopback 时视为外部 sidecar，不内嵌
//	DIRECT_SIDECAR_STRIP_PROMPT=1  不注入官方提示词（A/B 复现用）

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed" // go:embed
)

//go:embed tools/zen_sidecar.js
var embeddedSidecarJS []byte

//go:embed tools/official_system_prompt.txt
var embeddedSidecarPrompt []byte

// sidecar 状态机的取值（只用于 /health 与日志，不参与控制流）。
const (
	sidecarDisabled  = "disabled"
	sidecarStarting  = "starting"
	sidecarRunning   = "running"
	sidecarRestart   = "restarting"
	sidecarStopped   = "stopped"
	sidecarStartWait = 15 * time.Second
	sidecarReWait    = 15 * time.Second
	sidecarMaxBackof = 30 * time.Second
	sidecarKeepAlive = 15 * time.Second
	// 子进程在这么久没有任何请求/心跳后自杀。网关被 Stop-Process -Force 干掉
	// 时没人收尸，靠它兜底；正常运行由 keepAlive 每 15s 续命。
	sidecarIdleExitMS = 120000
)

// sidecarProc 是唯一的 sidecar 实例（一个网关一个 bun）。
type sidecarProc struct {
	mu       sync.Mutex
	state    string
	enabled  bool
	bun      string
	dir      string
	port     int
	origin   string
	basePath string
	pid      int
	restarts int
	lastErr  string
	cmd      *exec.Cmd
	exitCh   chan struct{} // 当前子进程退出信号（supervise 关闭）
	stopCh   chan struct{}
	stopOnce sync.Once
	client   *http.Client
}

// newSidecarProc 造一个干净的实例（测试里替换全局用）。
func newSidecarProc() *sidecarProc {
	return &sidecarProc{
		state:  sidecarStopped,
		stopCh: make(chan struct{}),
		// 直连 127.0.0.1：显式绕开 HTTP(S)_PROXY，别让探测请求跑到代理上。
		client: &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

var sidecar = newSidecarProc()

// sidecarOff 读 DIRECT_SIDECAR；默认开启（免费层只有这条路能过）。
func sidecarOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIRECT_SIDECAR"))) {
	case "0", "off", "false", "no", "disable", "disabled":
		return true
	}
	return false
}

// isLoopbackHost 判断 upstream 是否指向本机。
// 指向本机 = 外部 sidecar（旧启动器）或手工代理，此时不再内嵌一份。
func isLoopbackHost(host string) bool {
	h := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// sidecarDirs 释放目录候选，按顺序取第一个可写的：
//  1. ZEN_SIDECAR_DIR（显式指定，不猜）
//  2. %LOCALAPPDATA%\zen-direct\sidecar（常规；Program Files 只读时也能用）
//  3. exe 同目录\sidecar（绿色/便携部署）
//  4. %TEMP%\zen-direct-sidecar（兜底；受限环境里 LOCALAPPDATA 可能写不进去）
func sidecarDirs() []string {
	if v := strings.TrimSpace(os.Getenv("ZEN_SIDECAR_DIR")); v != "" {
		return []string{v}
	}
	var out []string
	if runtime.GOOS == "windows" {
		if la := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); la != "" {
			out = append(out, filepath.Join(la, "zen-direct", "sidecar"))
		}
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		out = append(out, filepath.Join(d, "zen-direct", "sidecar"))
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		out = append(out, filepath.Join(filepath.Dir(exe), "sidecar"))
	}
	out = append(out, filepath.Join(os.TempDir(), "zen-direct-sidecar"))
	return out
}

// writableDir 真写一个探针文件再删：MkdirAll 成功不代表能落盘（ACL/只读卷）。
func writableDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// writeIfChanged 只在内容不同时落盘（升级后第一次启动会重写，之后不动）。
// 先写临时文件再 rename：避免半截 JS 被 bun 读进去。
func writeIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// bunBinary 找 bun 可执行文件。Windows 上 PATH 里通常是 bun.cmd/bun.ps1
// （npm shim），CreateProcess 不能直接跑，必须找到真正的 bun.exe。
func bunBinary() string {
	if v := strings.TrimSpace(os.Getenv("ZEN_BUN")); v != "" {
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			return v
		}
	}
	if p, err := exec.LookPath("bun"); err == nil {
		if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(p), ".exe") {
			return p
		}
	}
	var cands []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		name := "bun"
		if runtime.GOOS == "windows" {
			name = "bun.exe"
		}
		cands = append(cands, filepath.Join(home, ".bun", "bin", name))
	}
	if runtime.GOOS == "windows" {
		if appdata := strings.TrimSpace(os.Getenv("APPDATA")); appdata != "" {
			// npm i -g bun 的真身（bun.cmd 只是转发到这）。
			cands = append(cands, filepath.Join(appdata, "npm", "node_modules", "bun", "bin", "bun.exe"))
		}
		if la := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); la != "" {
			cands = append(cands, filepath.Join(la, "bun", "bun.exe"))
		}
	}
	for _, p := range cands {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// sidecarPort 选监听端口：ZEN_SIDECAR_PORT 指定优先，否则让内核给一个空闲的。
// 先 Listen 再 Close 有极小的竞争窗口，但只绑 127.0.0.1，实际不会撞。
func sidecarPort() (int, error) {
	if v := strings.TrimSpace(os.Getenv("ZEN_SIDECAR_PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return 0, fmt.Errorf("ZEN_SIDECAR_PORT 非法: %q", v)
		}
		return p, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("挑空闲端口失败: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("挑空闲端口失败: 非 TCP 地址")
	}
	return addr.Port, nil
}

// sidecarBaseURL 拼网关要用的 upstream 基址。路径沿用原 base 的路径，
// 这样 sidecar 转发出去仍是 https://opencode.ai/inference/v1/...
func sidecarBaseURL(port int, basePath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if basePath == "" {
		basePath = "/inference/v1"
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, basePath)
}

// prepareSidecarFiles 释放内嵌资产，返回 (目录, js 路径, 提示词路径)。
func prepareSidecarFiles() (string, string, string, error) {
	if len(embeddedSidecarJS) == 0 || len(embeddedSidecarPrompt) == 0 {
		return "", "", "", errors.New("内嵌 sidecar 资产为空（构建时缺 tools/）")
	}
	var dir string
	var tried []string
	for _, cand := range sidecarDirs() {
		if !writableDir(cand) {
			tried = append(tried, cand)
			continue
		}
		dir = cand
		break
	}
	if dir == "" {
		return "", "", "", fmt.Errorf("没有可写目录（试过: %s）", strings.Join(tried, "; "))
	}
	jsPath := filepath.Join(dir, "zen_sidecar.js")
	if _, err := writeIfChanged(jsPath, embeddedSidecarJS); err != nil {
		return "", "", "", fmt.Errorf("写 %s 失败: %w", jsPath, err)
	}
	promptPath := filepath.Join(dir, "official_system_prompt.txt")
	if _, err := writeIfChanged(promptPath, embeddedSidecarPrompt); err != nil {
		return "", "", "", fmt.Errorf("写 %s 失败: %w", promptPath, err)
	}
	return dir, jsPath, promptPath, nil
}

// startSidecar 释放资产、拉起 bun、等 /health，返回可以当 upstream 用的基址。
// origin 形如 https://opencode.ai；basePath 是原 base 的路径（/inference/v1）。
func startSidecar(origin, basePath string) (string, error) {
	if sidecarOff() {
		return "", errors.New("DIRECT_SIDECAR=off")
	}
	bun := bunBinary()
	if bun == "" {
		return "", errors.New("找不到 bun（装 Bun 或设 ZEN_BUN 指向 bun.exe）")
	}
	dir, jsPath, promptPath, err := prepareSidecarFiles()
	if err != nil {
		return "", err
	}
	port, err := sidecarPort()
	if err != nil {
		return "", err
	}

	sidecar.mu.Lock()
	sidecar.enabled = true
	sidecar.bun = bun
	sidecar.dir = dir
	sidecar.port = port
	sidecar.origin = origin
	sidecar.basePath = basePath
	sidecar.state = sidecarStarting
	sidecar.lastErr = ""
	sidecar.mu.Unlock()

	if err := sidecar.launch(jsPath, promptPath); err != nil {
		sidecar.setErr(err)
		return "", err
	}
	if err := sidecar.waitHealthy(sidecarStartWait); err != nil {
		sidecar.reapAndClose()
		sidecar.setErr(err)
		return "", fmt.Errorf("sidecar /health 未就绪: %w", err)
	}
	base := sidecarBaseURL(port, basePath)
	sidecar.mu.Lock()
	sidecar.state = sidecarRunning
	p := sidecar.pid
	sidecar.mu.Unlock()
	log.Printf("sidecar ready: %s (pid=%d bun=%s)", base, p, bun)
	go sidecar.supervise(jsPath, promptPath)
	go sidecar.keepAlive()
	return base, nil
}

// sidecarIdleMS 子进程无心跳自杀阈值；DIRECT_SIDECAR_IDLE_MS 可调（调试/省电）。
func sidecarIdleMS() int {
	if v := strings.TrimSpace(os.Getenv("DIRECT_SIDECAR_IDLE_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1000 && n <= 3600_000 {
			return n
		}
	}
	return sidecarIdleExitMS
}

// launch 起一个 bun 进程，不等退出（Wait 由 supervise 负责）。
func (s *sidecarProc) launch(jsPath, promptPath string) error {
	s.mu.Lock()
	port, origin, basePath := s.port, s.origin, s.basePath
	s.mu.Unlock()
	if bp := strings.TrimRight(basePath, "/"); bp != "" {
		basePath = bp
	} else {
		basePath = "/inference/v1"
	}

	cmd := exec.Command(s.bunPath(), jsPath)
	cmd.Dir = filepath.Dir(jsPath)
	env := []string{
		"ZEN_SIDECAR_PORT=" + strconv.Itoa(port),
		"ZEN_UPSTREAM=" + origin,
		"ZEN_OFFICIAL_PROMPT_FILE=" + promptPath,
		// base 路径随 OPENCODE_ZEN_BASE 走，sidecar 只转发这几个前缀。
		"ZEN_SIDECAR_PREFIX=" + basePath + ",/inference/,/zen/",
		// 网关心跳续命；心跳断了就自杀，别留孤儿进程占端口。
		"ZEN_IDLE_EXIT_MS=" + strconv.Itoa(sidecarIdleMS()),
	}
	if os.Getenv("DIRECT_SIDECAR_STRIP_PROMPT") == "1" {
		env = append(env, "ZEN_SIDECAR_STRIP_PROMPT=1")
	}
	cmd.Env = append(os.Environ(), env...)
	hideSidecarWindow(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 bun 失败: %w", err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.exitCh = make(chan struct{})
	s.pid = cmd.Process.Pid
	s.mu.Unlock()
	go s.pipeLogs(stdout)
	go s.pipeLogs(stderr)
	return nil
}

func (s *sidecarProc) bunPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bun
}

// pipeLogs 把子进程输出并入网关日志（LOG_FILE 里能看到 inject=1/429）。
func (s *sidecarProc) pipeLogs(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		log.Printf("bun| %s", strings.TrimRight(sc.Text(), "\r"))
	}
}

// supervise：bun 退出 -> 退避重启；只有 stop() 能终结这个循环。
func (s *sidecarProc) supervise(jsPath, promptPath string) {
	backoff := time.Second
	for {
		s.mu.Lock()
		cmd := s.cmd
		s.mu.Unlock()
		var werr error
		if cmd != nil {
			werr = cmd.Wait() // 每个进程只 Wait 一次
			s.closeExit()
		}
		select {
		case <-s.stopCh:
			return
		default:
		}
		msg := "sidecar 退出"
		if werr != nil {
			msg = fmt.Sprintf("sidecar 退出: %v", werr)
		}
		s.mu.Lock()
		s.state = sidecarRestart
		s.pid = 0
		s.cmd = nil
		s.lastErr = msg
		s.mu.Unlock()
		log.Printf("%s；%s 后重启", msg, backoff)

		select {
		case <-time.After(backoff):
		case <-s.stopCh:
			return
		}
		if backoff < sidecarMaxBackof {
			backoff *= 2
			if backoff > sidecarMaxBackof {
				backoff = sidecarMaxBackof
			}
		}

		if err := s.launch(jsPath, promptPath); err != nil {
			s.setErr(err)
			continue
		}
		if err := s.waitHealthy(sidecarReWait); err != nil {
			s.setErr(err)
			log.Printf("sidecar 重启后 /health 未就绪: %v", err)
			s.killCurrent()
			continue
		}
		s.mu.Lock()
		s.state = sidecarRunning
		s.restarts++
		s.pid = s.cmd.Process.Pid
		s.lastErr = ""
		n := s.restarts
		s.mu.Unlock()
		log.Printf("sidecar 已重启 (#%d) pid=%d", n, s.pidNow())
		backoff = time.Second
	}
}

// keepAlive 定时打 /health：既续子进程的命，也顺手暴露状态。
func (s *sidecarProc) keepAlive() {
	t := time.NewTicker(sidecarKeepAlive)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			if err := s.ping(); err != nil {
				s.mu.Lock()
				if s.state == sidecarRunning {
					s.state = sidecarRestart
				}
				s.lastErr = "health: " + err.Error()
				s.mu.Unlock()
			}
		}
	}
}

// waitHealthy 轮询 /health，直到就绪、超时或收到停止信号。
func (s *sidecarProc) waitHealthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error = errors.New("timeout")
	for time.Now().Before(deadline) {
		select {
		case <-s.stopCh:
			return errors.New("stopped")
		default:
		}
		if err := s.ping(); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

func (s *sidecarProc) ping() error {
	s.mu.Lock()
	port := s.port
	s.mu.Unlock()
	if port == 0 {
		return errors.New("no port")
	}
	resp, err := s.client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != 200 {
		return fmt.Errorf("health http %d", resp.StatusCode)
	}
	return nil
}

// stop 关掉子进程并终结 supervise 循环（可重复调用）。
// 杀掉之后等它真的退出：Windows 上进程没退干净会锁着释放目录里的 .js，
// 升级/测试里紧接着删目录会失败。
func (s *sidecarProc) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		log.Printf("sidecar stopped")
	})
	s.mu.Lock()
	ch := s.exitCh
	s.mu.Unlock()
	s.killCurrent()
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
		}
	}
	s.mu.Lock()
	s.state = sidecarStopped
	s.pid = 0
	s.mu.Unlock()
}

// closeExit 关闭当前子进程的退出信号（只关一次）。
func (s *sidecarProc) closeExit() {
	s.mu.Lock()
	if s.exitCh != nil {
		close(s.exitCh)
		s.exitCh = nil
	}
	s.mu.Unlock()
}

func (s *sidecarProc) killCurrent() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// reapAndClose 杀掉当前子进程并等它退出（supervise 尚未启动时就地收尸，
// 否则没人 Wait，exitCh 永远不关，stop() 要白等 3s）。
func (s *sidecarProc) reapAndClose() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	s.killCurrent()
	if cmd != nil {
		go func() {
			_ = cmd.Wait()
			s.closeExit()
		}()
	}
}

func (s *sidecarProc) setErr(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.lastErr = err.Error()
	s.state = sidecarRestart
	s.mu.Unlock()
}

func (s *sidecarProc) pidNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// statusJSON 给 /health 用：一眼看出内嵌 sidecar 在不在、重启过几次。
func (s *sidecarProc) statusJSON() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]any{
		"state":    s.state,
		"enabled":  s.enabled,
		"bun":      s.bun,
		"port":     s.port,
		"pid":      s.pid,
		"restarts": s.restarts,
		"dir":      s.dir,
	}
	if s.lastErr != "" {
		m["last_error"] = s.lastErr
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"state":"unknown"}`
	}
	return string(b)
}

// sidecarUpstream 按 OPENCODE_ZEN_BASE 决定上游：
//   - DIRECT_SIDECAR=off   -> 直连原 base
//   - base 指向 loopback   -> 外部 sidecar（旧启动器），原样使用
//   - 其余                 -> 起内嵌 sidecar，base 换成 127.0.0.1:<端口>
//
// 返回最终 upstream 字符串（调用方再 url.Parse）。
func sidecarUpstream(baseRaw string) string {
	u, err := url.Parse(baseRaw)
	if err != nil {
		return baseRaw
	}
	if sidecarOff() {
		log.Printf("sidecar: DIRECT_SIDECAR=off，直连 %s（免费层会 429）", baseRaw)
		return baseRaw
	}
	if isLoopbackHost(u.Hostname()) {
		log.Printf("sidecar: OPENCODE_ZEN_BASE 指向本机（%s），按外部 sidecar 处理，不再内嵌", u.Host)
		return baseRaw
	}
	origin := u.Scheme + "://" + u.Host
	base, err := startSidecar(origin, u.Path)
	if err != nil {
		log.Printf("sidecar 未启动，回退直连 %s（免费层会 429）：%v", baseRaw, err)
		return baseRaw
	}
	return base
}

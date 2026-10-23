package main

// 429 FreeUsageLimitError 语义（实测，见 GitHub Actions zen-remote-probe）：
//   - 额度按上游 session 计：打满的 session 一直 429 到窗口重置；
//   - 用一次这样的 session，出口 IP 会被连坐数分钟：同一 IP 上全新 session 也 429；
//   - 干净 IP 上全新 session 稳定 200。
// 网关因此必须：记毒（落盘，重启不复用）+ 换 session + 冷却期直接短路，不做重试。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const freeLimitBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`

// 免费额度 429：原样回给客户端、不重试、记毒、并把池里的 session 换掉。
func TestFreeUsageLimitPoisonsAndRotates(t *testing.T) {
	resetPoolForTest()
	t.Setenv("DIRECT_FREELIMIT_COOLDOWN", "600") // 本条验冷却行为；默认已不冷却
	freeLimitUntil.Store(0)
	var mu sync.Mutex
	var sessions []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		mu.Lock()
		sessions = append(sessions, req.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, freeLimitBody)
			return
		}
		_, _ = io.WriteString(w, `{"id":"r1","status":"completed","output":[]}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	ask := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"free limit test"}]}`
	code, body := postResponses(t, "http://x", ask)
	if code != 429 || !strings.Contains(body, "FreeUsageLimitError") {
		t.Fatalf("免费额度 429 应原样透传，得 %d %.200s", code, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("冷却/换号不该在请求内重试，得 %d 次上游调用", calls.Load())
	}
	ses1 := sessions[0]
	poolMu.Lock()
	poisoned := badSes[ses1]
	poolMu.Unlock()
	if !poisoned {
		t.Fatalf("打满额度的 session 应记毒：%q", ses1)
	}
	if b, err := os.ReadFile(badSesFilePath()); err != nil || !strings.Contains(string(b), ses1) {
		t.Fatalf("毒 session 应落盘（err=%v）：%.200s", err, string(b))
	}
	if _, blocked := freeLimitBlocked(); !blocked {
		t.Fatal("吃过免费额度 429 后应进入冷却")
	}

	// 冷却结束后同一 fp 应换新 session，且不得再捡回毒 session。
	freeLimitUntil.Store(0)
	if code, body := postResponses(t, "http://x", ask); code != 200 {
		t.Fatalf("冷却后应 200，得 %d %.200s", code, body)
	}
	if len(sessions) != 2 || sessions[1] == ses1 {
		t.Fatalf("应换新 session：%v", sessions)
	}
}

// 冷却期内直接短路：不打上游，给 Retry-After。
func TestFreeLimitCooldownShortCircuits(t *testing.T) {
	resetPoolForTest()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","status":"completed","output":[]}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()
	freeLimitUntil.Store(0)
	t.Cleanup(func() { freeLimitUntil.Store(0) })

	freeLimitUntil.Store(time.Now().Add(time.Minute).UnixMilli())
	req := httptest.NewRequest("POST", "http://x/v1/responses",
		strings.NewReader(`{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"cool"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	forwardResponses(rec, req, FmtResponses)

	if rec.Code != 429 {
		t.Fatalf("冷却期应 429，得 %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("冷却期 429 应带 Retry-After")
	}
	if !strings.Contains(rec.Body.String(), "free_limit_cooldown") {
		t.Fatalf("错误码应可辨识：%.200s", rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("冷却期不得打上游，得 %d 次", calls.Load())
	}
}

// 冷却默认关闭：轮换代理下“封到 07:59”只会把新会话挡在门外（2026-10-24 00:06
// 实测 DSH 新会话连续 429，而下一秒换 IP 的请求 200）。显式设置才启用。
func TestFreeLimitCooldownDefaultsOff(t *testing.T) {
	resetPoolForTest()
	if d := freeLimitCooldown(); d != 0 {
		t.Fatalf("默认应不冷却，得 %s", d)
	}
	if d := nextFreeLimitCooldown(); d != 0 {
		t.Fatalf("默认 next 也应为 0，得 %s", d)
	}
	t.Setenv("DIRECT_FREELIMIT_COOLDOWN", "120")
	if d := freeLimitCooldown(); d != 120*time.Second {
		t.Fatalf("显式设置应生效，得 %s", d)
	}
	if d := nextFreeLimitCooldown(); d != 120*time.Second {
		t.Fatalf("next 应沿用显式值，得 %s", d)
	}
	resetFreeLimitCooldown()
}

// 窗口重置点：北京 07:59，跨天也成立。
func TestNextFreeResetSchedule(t *testing.T) {
	cst := freeResetZone
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 10, 23, 6, 0, 0, 0, cst), time.Date(2026, 10, 23, 7, 59, 0, 0, cst)},
		{time.Date(2026, 10, 23, 7, 58, 59, 0, cst), time.Date(2026, 10, 23, 7, 59, 0, 0, cst)},
		{time.Date(2026, 10, 23, 7, 59, 0, 0, cst), time.Date(2026, 10, 24, 7, 59, 0, 0, cst)},
		{time.Date(2026, 10, 23, 23, 30, 0, 0, cst), time.Date(2026, 10, 24, 7, 59, 0, 0, cst)},
	}
	for _, c := range cases {
		if got := nextFreeReset(c.now); !got.Equal(c.want) {
			t.Fatalf("%s -> %s，期望 %s", c.now, got, c.want)
		}
		if got := lastFreeReset(c.now).AddDate(0, 0, 1); !got.Equal(nextFreeReset(c.now)) {
			t.Fatalf("lastFreeReset 与 nextFreeReset 不接续：%s <- %s", got, c.now)
		}
	}
}

// 毒名单过窗口重置自动作废：打满的 session 是“今天用完”，第二天解禁。
func TestPoisonExpiresAtReset(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	full := "ses_f88daf257ffeVeQTe3LMg59tzS"
	if err := os.WriteFile(badSesFilePath(), []byte(full+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSesLoad()
	poolMu.Lock()
	hit := isBadSesLocked(full)
	// 手动把重置点拨到过去：表应立即作废。
	poisonResetAt = time.Now().Add(-time.Second)
	after := isBadSesLocked(full)
	n := len(badSes)
	poolMu.Unlock()
	if !hit {
		t.Fatal("重置前应判毒")
	}
	if after || n != 0 {
		t.Fatalf("过重置点应整表作废：判毒=%v 条数=%d", after, n)
	}
	if _, err := os.Stat(badSesFilePath()); !os.IsNotExist(err) {
		t.Fatalf("重置后毒名单文件应删除，err=%v", err)
	}
}

// 冷却绑定出口 IP：换了干净 IP 立刻放行（人工换线路的用法）。
func TestCooldownClearedOnIPChange(t *testing.T) {
	resetPoolForTest()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","status":"completed","output":[]}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	freeLimitUntil.Store(time.Now().Add(10 * time.Minute).UnixMilli())
	freeLimitIP.Store("1.1.1.1")
	freeLimitIPCheck.Store(0)
	orig := egressIPFunc
	egressIPFunc = func() string { return "2.2.2.2" }
	defer func() { egressIPFunc = orig }()

	code, body := postResponses(t, "http://x",
		`{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"new ip"}]}`)
	if code != 200 {
		t.Fatalf("换 IP 后应放行，得 %d %.200s", code, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("换 IP 后应打上游一次，得 %d", calls.Load())
	}
	if _, blocked := freeLimitBlocked(); blocked {
		t.Fatal("换 IP 后冷却应被清掉")
	}
}

// 同一个 IP 上冷却照旧生效。
func TestCooldownHoldsOnSameIP(t *testing.T) {
	resetPoolForTest()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r1","status":"completed","output":[]}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	freeLimitUntil.Store(time.Now().Add(10 * time.Minute).UnixMilli())
	freeLimitIP.Store("1.1.1.1")
	freeLimitIPCheck.Store(0)
	orig := egressIPFunc
	egressIPFunc = func() string { return "1.1.1.1" }
	defer func() { egressIPFunc = orig }()

	code, _ := postResponses(t, "http://x",
		`{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"same ip"}]}`)
	if code != 429 {
		t.Fatalf("同 IP 冷却期应 429，得 %d", code)
	}
	if calls.Load() != 0 {
		t.Fatalf("同 IP 冷却期不得打上游，得 %d", calls.Load())
	}
}

// 非免费额度的 429（真限流）不记毒、不换号、不冷却，原样透传。
func TestNonFreeLimit429Passthru(t *testing.T) {
	resetPoolForTest()
	freeLimitUntil.Store(0)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	code, body := postResponses(t, "http://x", `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"plain throttle"}]}`)
	if code != 429 || !strings.Contains(body, "too many requests") {
		t.Fatalf("应透传 429，得 %d %.200s", code, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("非免费额度 429 不得重试，得 %d 次调用", calls.Load())
	}
	if _, blocked := freeLimitBlocked(); blocked {
		t.Fatal("非免费额度 429 不应触发冷却")
	}
}

// 毒名单抄进截断 id（2026-10-23 事故现场）：429 日志曾把 ses= 截成 20 位，
// 抄进名单后精确查表永远放不中，已打满额的 session 又被发出去，干净出口 IP
// 当场被连坐。残条必须按前缀命中，且快照里的毒号重启即丢。
func TestPoisonTruncatedEntryStillFilters(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	full := "ses_f88daf257ffeVeQTe3LMg59tzS"
	other := "ses_f70e48a05ffeb1MPMKVO3uLSPu"
	genuineSessions = []string{full, other}
	genuineSet = map[string]bool{full: true, other: true}

	const trunc = "ses_f88daf257ffeVeQT"
	if err := os.WriteFile(badSesFilePath(), []byte(trunc+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSesLoad()

	poolMu.Lock()
	hit := isBadSesLocked(full)
	miss := isBadSesLocked(other)
	poolMu.Unlock()
	if !hit {
		t.Fatalf("截断条目 %q 应前缀命中全长 id %q", trunc, full)
	}
	if miss {
		t.Fatalf("不得误伤无关 session %q", other)
	}
	// 各 salt 定点选号都不该再捡回毒号。
	for salt := uint64(0); salt < 8; salt++ {
		if s := pickGenuine("fp", salt); s == full {
			t.Fatalf("毒 session 又被选中（salt=%d）", salt)
		}
	}

	// 旧快照存的是全长 id：重启加载时同样得拦住。
	dir := t.TempDir()
	oldPoolFile := poolFile
	poolFile = filepath.Join(dir, "ses.json")
	defer func() { poolFile = oldPoolFile }()
	snap := `{"fp:bad":{"ses":"` + full + `","seen":"` + time.Now().Format(time.RFC3339Nano) + `","gen":true}}`
	if err := os.WriteFile(poolFile, []byte(snap), 0o600); err != nil {
		t.Fatal(err)
	}
	poolLoad()
	if got := poolLookup("fp:bad"); got != "" {
		t.Fatalf("毒号不得随快照复活，得 %q", got)
	}
}

// 只有 ses_ 这种脏条目不准参与前缀匹配：否则一行坏数据能把整个池判毒。
func TestPoisonTooShortIgnored(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	if err := os.WriteFile(badSesFilePath(), []byte("ses_\nses_f88daf257ffeVeQT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSesLoad()
	poolMu.Lock()
	shortHit := isBadSesLocked("ses_x")
	n := len(badSes)
	poolMu.Unlock()
	if shortHit || n != 1 {
		t.Fatalf("过短条目应被忽略：毒表 %d 条，ses_x 判毒=%v", n, shortHit)
	}
}

// 启动时把当天日志里 free-usage 429 的 id 收回毒库：进程被杀 / 名单被手工改过
// 之后，重启也不丢「今天已经打满」的证据。
func TestHarvestTodayLog(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	dir := t.TempDir()
	today := time.Now().In(freeResetZone).Format("20060102")
	logPath := filepath.Join(dir, "zen-direct-"+today+".err.log")
	hit := "ses_f7c52721affeC0Osw3mn0CAIT6"
	lines := []string{
		"2026/10/23 17:07:18 [REQ resp_dir_1] free-usage 429 ses=" + hit + ", poisoned+rotate, cooldown 13h30m (total=1)",
		"2026/10/23 17:07:19 [REQ resp_dir_2] free-usage 429 ses=ses_deadbeef00000000000000000, poisoned+rotate",
		"2026/10/23 17:07:20 [REQ resp_dir_3] in=chat model=x ses=ses_notapoison new=true",
		"2026/10/23 17:07:21 [REQ resp_dir_4] upstream 429 (not free-limit): too many requests",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FILE", logPath)
	badSesLoad()

	poolMu.Lock()
	a := isBadSesLocked(hit)
	b := isBadSesLocked("ses_deadbeef00000000000000000")
	c := isBadSesLocked("ses_notapoison")
	poolMu.Unlock()
	if !a || !b {
		t.Fatalf("应回收日志里的两条 429：428=%v deadbeef=%v", a, b)
	}
	if c {
		t.Fatal("普通请求行的 ses= 不应记毒")
	}
}

// 只认今天的日志：文件名是昨天的，todayLogPath 会换成今天，读不到即不回收。
func TestHarvestSkipsOtherDays(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	dir := t.TempDir()
	yesterday := time.Now().In(freeResetZone).AddDate(0, 0, -1).Format("20060102")
	logPath := filepath.Join(dir, "zen-direct-"+yesterday+".err.log")
	hit := "ses_f7c52721affeC0Osw3mn0CAIT6"
	if err := os.WriteFile(logPath, []byte("free-usage 429 ses="+hit+", poisoned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOG_FILE", logPath)
	badSesLoad()
	poolMu.Lock()
	poisoned := isBadSesLocked(hit)
	poolMu.Unlock()
	if poisoned {
		t.Fatal("昨天的日志不应参与回收")
	}
}

// 行解析：只认 free-usage 429 那条尾巴，截断/无标记/超短都要正确处理。
func TestFreeLimitSesFromLine(t *testing.T) {
	cases := []struct{ line, want string }{
		{"[REQ x] free-usage 429 ses=ses_aaaaaaaaaaaaaaaaaaaaaaaaaa, poisoned+rotate", "ses_aaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"[REQ x] free-usage 429 ses=ses_f88daf257ffeVeQT, poisoned+rotate, cooldown 45m0s", "ses_f88daf257ffeVeQT"},
		{"[REQ x] upstream 429 (not free-limit): too many requests", ""},
		{"[REQ x] free-usage 429 ses=bad_value,", ""},
		{"[REQ x] free-usage 429 ses=, poisoned", ""},
		{"[REQ x] in=chat ses=ses_fine", ""},
	}
	for _, c := range cases {
		if got := freeLimitSesFromLine(c.line); got != c.want {
			t.Fatalf("%q -> %q，期望 %q", c.line, got, c.want)
		}
	}
}

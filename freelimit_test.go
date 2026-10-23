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

// 冷却时长升级：连犯翻倍、封顶 40 分钟，成功一次归零。
func TestFreeLimitCooldownEscalates(t *testing.T) {
	resetPoolForTest()
	base := freeLimitCooldown()
	d1 := nextFreeLimitCooldown()
	if d1 != base {
		t.Fatalf("首次冷却应为基础值 %s，得 %s", base, d1)
	}
	if d2 := nextFreeLimitCooldown(); d2 != 2*base {
		t.Fatalf("第二次应翻倍为 %s，得 %s", 2*base, d2)
	}
	freeLimitDur.Store(int64(freeLimitDurCap))
	if d := nextFreeLimitCooldown(); d != freeLimitDurCap {
		t.Fatalf("应封顶 %s，得 %s", freeLimitDurCap, d)
	}
	resetFreeLimitCooldown()
	if d := nextFreeLimitCooldown(); d != base {
		t.Fatalf("成功后应回到基础值 %s，得 %s", base, d)
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

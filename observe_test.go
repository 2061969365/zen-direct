package main

// 观测与兜底：轮次计数（429 日志里的 turns=N）、认不出类型的 429 记毒、
// 窗口重置时归档当天毒号。与 freelimit_test.go 分开，避免动那堆老用例。

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

// 同一对话成功几轮就记几：这是量「一条号能吃多少轮」的唯一数据来源。
func TestPoolCountsTurns(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	t.Setenv("DIRECT_FRESH_PER_TURN", "0")
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

	ask := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"turn counter test"}]}`
	for i := 1; i <= 3; i++ {
		code, body := postResponses(t, "http://x", ask)
		if code != 200 {
			t.Fatalf("第 %d 轮应 200，得 %d %.200s", i, code, body)
		}
	}
	fp, _ := fpForEntry(FmtResponses, http.Header{}, map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "turn counter test"}},
	})
	poolMu.Lock()
	got := poolTurnsLocked(fp)
	poolMu.Unlock()
	if got != 3 {
		t.Fatalf("turns 应记 3 轮，得 %d", got)
	}
	if calls.Load() != 3 {
		t.Fatalf("上游应被调 3 次，得 %d", calls.Load())
	}
}

// 认不出类型的 429：上游改措辞时别赌，记毒 + 换号，但不封全天（分不清限流）。
func TestUnknown429PoisonsSession(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	t.Setenv("DIRECT_FRESH_PER_TURN", "0")
	freeLimitUntil.Store(0)
	var calls atomic.Int64
	var mu sync.Mutex
	var sessions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		mu.Lock()
		sessions = append(sessions, req.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"quota exhausted for this session"}}`)
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	ask := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"unknown 429 test"}]}`
	code, body := postResponses(t, "http://x", ask)
	if code != 429 {
		t.Fatalf("应原样透传 429，得 %d %.200s", code, body)
	}
	if _, blocked := freeLimitBlocked(); blocked {
		t.Fatal("认不出类型的 429 不该进全天冷却")
	}
	mu.Lock()
	ses1 := sessions[0]
	mu.Unlock()
	poolMu.Lock()
	poisoned := badSes[ses1]
	poolMu.Unlock()
	if !poisoned {
		t.Fatalf("认不出类型的 429 也应把那轮用的号记毒：%s", ses1)
	}

	// 下一次必须换一条号。
	code, body = postResponses(t, "http://x", ask)
	if code != 429 {
		t.Fatalf("第二次也应 429，得 %d %.200s", code, body)
	}
	mu.Lock()
	ses2 := sessions[1]
	mu.Unlock()
	if ses2 == ses1 {
		t.Fatalf("记毒后不该再用同一条号：%s", ses1)
	}
	if calls.Load() != 2 {
		t.Fatalf("不应重试上游，期望 2 次调用，得 %d", calls.Load())
	}
}

// 同一个 system 提示词、不同开场白 = 不同对话：fp 必须不同。
// 老实现取 messages[0]（system），DSH 的 256 个请求只落成 2 个 fp，等于所有
// “新会话”共用一条上游 session。
func TestFpSeparatesConversations(t *testing.T) {
	const sysPrompt = "You are an AI agent powered by Test Harness."
	mk := func(user string) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": sysPrompt},
			map[string]any{"role": "user", "content": user},
		}}
	}
	a, okA := fpForEntry(FmtChat, http.Header{}, mk("first prompt A"))
	b, okB := fpForEntry(FmtChat, http.Header{}, mk("first prompt B"))
	if !okA || !okB {
		t.Fatal("两条对话都应能定键")
	}
	if a == b {
		t.Fatalf("不同开场白不应共用 fp：%s", a)
	}

	// 续聊：历史只往后接，fp 必须保持稳定。
	growth := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": sysPrompt},
		map[string]any{"role": "user", "content": "first prompt A"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "second turn"},
	}}
	c, _ := fpForEntry(FmtChat, http.Header{}, growth)
	if c != a {
		t.Fatalf("同一对话续聊 fp 应稳定：%s != %s", c, a)
	}

	// 完全没有 user 消息时退回首条正文（不能返回“无键”让调用方现签）。
	onlySys := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": sysPrompt},
	}}
	if _, ok := fpForEntry(FmtChat, http.Header{}, onlySys); !ok {
		t.Fatal("只有 system 时也应能定键（退回首条正文）")
	}
}

// 无 X-Session-Id 的 chat 客户端：两个不同对话必须落到两条不同的上游 session。
func TestPoolSeparatesConversations(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	t.Setenv("DIRECT_FRESH_PER_TURN", "0")
	var mu sync.Mutex
	var sessions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"data: {\"id\":\"c1\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: [DONE]\n\n")
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	body := func(user string) string {
		return `{"model":"mimo-v2.5-free","stream":false,"messages":[` +
			`{"role":"system","content":"same system prompt"},` +
			`{"role":"user","content":"` + user + `"}]}`
	}
	if code, b := postChat(t, "http://x", body("conversation A")); code != 200 {
		t.Fatalf("A 应 200：%d %.200s", code, b)
	}
	if code, b := postChat(t, "http://x", body("conversation B")); code != 200 {
		t.Fatalf("B 应 200：%d %.200s", code, b)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 2 {
		t.Fatalf("上游应被调 2 次，得 %d", len(sessions))
	}
	if sessions[0] == sessions[1] {
		t.Fatalf("两个不同对话共用了一条上游 session：%s", sessions[0])
	}
}

// 窗口重置：先把当天毒号归档，再清表删文件。
func TestPoisonArchiveOnReset(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	dir := t.TempDir()
	badSesFile = filepath.Join(dir, "poisoned_sessions.txt")
	ids := []string{"ses_aaaaaaaaaaaaaaaaaaaaaaaaaa", "ses_bbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if err := os.WriteFile(badSesFile, []byte(strings.Join(ids, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badSesLoad()

	poolMu.Lock()
	poisonResetAt = time.Now().Add(-time.Second)
	_ = isBadSesLocked("ses_x")
	n := len(badSes)
	poolMu.Unlock()
	if n != 0 {
		t.Fatalf("过重置点应清空毒表，剩 %d 条", n)
	}
	if _, err := os.Stat(badSesFilePath()); !os.IsNotExist(err) {
		t.Fatalf("重置后毒名单文件应删除，err=%v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "poison_archive.txt"))
	if err != nil {
		t.Fatalf("应生成归档文件：%v", err)
	}
	arc := string(b)
	if !strings.Contains(arc, "# window ended") {
		t.Fatalf("归档缺窗口结束时间头：%.200s", arc)
	}
	for _, id := range ids {
		if !strings.Contains(arc, id) {
			t.Fatalf("归档缺 id %s：%.200s", id, arc)
		}
	}
}

package main

// v0.2.1 硬化单测：P0 并发门/413/隔离/直通/桩禁调，P1 模型归一/轮换/落盘/DONE，P2 透传。
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// 空格/[1M]/多段前缀归一；裸 id 不动。
func TestNormalizeModelExtended(t *testing.T) {
	cases := map[string]string{
		" test/muse-spark-1.3-contributor-free ": "muse-spark-1.3-contributor-free",
		"claude-opus-4-5[1M]":                    "claude-opus-4-5",
		"claude-opus-4-5[1m] ":                   "claude-opus-4-5",
		"a/b/c":                                  "c",
		"muse-spark-1.3-contributor-free":        "muse-spark-1.3-contributor-free",
	}
	for in, want := range cases {
		m := map[string]any{"model": in}
		normalizeModel(m)
		if m["model"] != want {
			t.Fatalf("normalize(%q)=%q want %q", in, m["model"], want)
		}
	}
}

// fpForEntry：有头 keyed；空体/首条非文本 unkeyed；正常文本 keyed。
func TestFpUnkeyed(t *testing.T) {
	h := http.Header{}
	if _, keyed := fpForEntry(FmtResponses, h, map[string]any{}); keyed {
		t.Fatal("空体应 unkeyed")
	}
	toolFirst := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "x", "output": "ok"},
	}}
	if _, keyed := fpForEntry(FmtResponses, h, toolFirst); keyed {
		t.Fatal("首条非文本应 unkeyed")
	}
	if _, keyed := fpForEntry(FmtChat, h, map[string]any{"messages": []any{}}); keyed {
		t.Fatal("空 messages 应 unkeyed")
	}
	textFirst := map[string]any{"input": []any{
		map[string]any{"type": "message", "role": "user", "content": "hi"},
	}}
	fp, keyed := fpForEntry(FmtResponses, h, textFirst)
	if !keyed || !strings.HasPrefix(fp, "fp:") {
		t.Fatalf("正常文本应 keyed fp:，得 %q,%v", fp, keyed)
	}
	hh := http.Header{"X-Session-Id": []string{"s1"}}
	fp, keyed = fpForEntry(FmtResponses, hh, map[string]any{})
	if !keyed || fp != "hdr:s1" {
		t.Fatalf("有头应 hdr:s1，得 %q,%v", fp, keyed)
	}
}

// 官方 UA 无头也直通：毒 blob 原样到上游（不剥离、不进池）。
func TestOfficialBypassNoStrip(t *testing.T) {
	withMockUpstream(t, `{"id":"r1","status":"completed","output":[]}`)
	body := `{"model":"m","input":[{"type":"reasoning","id":"rs_x","encrypted_content":"blob","summary":[]}]}`
	req := httptest.NewRequest("POST", "http://x/v1/responses", strings.NewReader(body))
	req.Header.Set("User-Agent", "opencode/1.18.31 test")
	rec := httptest.NewRecorder()
	forwardResponses(rec, req, FmtResponses)
	if rec.Code != 200 {
		t.Fatalf("http=%d body=%.200s", rec.Code, rec.Body.String())
	}
	_, upBody, _ := getLastUp()
	if !strings.Contains(upBody, "rs_x") || !strings.Contains(upBody, "blob") {
		t.Fatalf("官方流量不得剥离：%.300s", upBody)
	}
}

// 无键请求：两次不同上游 session（现签），且首见剥离生效。
func TestUnkeyedFreshSessionAndStrip(t *testing.T) {
	withMockUpstream(t, `{"id":"r1","status":"completed","output":[]}`)
	body := `{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"reasoning","id":"rs_p","encrypted_content":"poison","summary":[]}]}`
	post := func() string {
		req := httptest.NewRequest("POST", "http://x/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		forwardResponses(rec, req, FmtResponses)
		if rec.Code != 200 {
			t.Fatalf("http=%d", rec.Code)
		}
		_, upBody, sess := getLastUp()
		if strings.Contains(upBody, "poison") {
			t.Fatalf("无键也应首见剥离：%.300s", upBody)
		}
		return sess
	}
	s1, s2 := post(), post()
	if s1 == "" || s2 == "" || s1 == s2 {
		t.Fatalf("无键请求应每次现签不同 session：%q vs %q", s1, s2)
	}
}

// poolRotate：旧 session 失效，新签生效，二次借命中新值。
func TestPoolRotate(t *testing.T) {
	resetPoolForTest()
	old, isNew := poolBorrowEx("fp:rot")
	if !isNew || old == "" {
		t.Fatal("首借应新签")
	}
	nw, _ := poolRotate("fp:rot")
	if nw == "" || nw == old {
		t.Fatalf("轮换应换新：%q vs %q", old, nw)
	}
	hit, isNew := poolBorrowEx("fp:rot")
	if isNew || hit != nw {
		t.Fatalf("轮换后应命中新值：%q,%v", hit, isNew)
	}
}

// strip：summary 字符串/对象形态保留文本去 blob+id；纯外来 id 去 id；
// 池内 lineage 原样不动。
func TestStripSummaryForms(t *testing.T) {
	// 字符串 summary。
	in := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "id": "rs_s", "encrypted_content": "b", "summary": "可见结论"},
	}}
	stripForeignBlobs(in, true)
	items, _ := in["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("字符串 summary 应保留，得 %d 条", len(items))
	}
	m, _ := items[0].(map[string]any)
	if _, ok := m["encrypted_content"]; ok {
		t.Fatal("blob 应剥掉")
	}
	if _, ok := m["id"]; ok {
		t.Fatal("外来 id 应剥掉")
	}
	// 纯外来 id 无 blob 无文本：整条丢。
	in2 := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "id": "rs_only"},
	}}
	stripForeignBlobs(in2, true)
	if items, _ := in2["input"].([]any); len(items) != 0 {
		t.Fatalf("纯外来 id 应整条丢，得 %d 条", len(items))
	}
	// 池内 lineage：blob 照留。
	in3 := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "id": "rs_k", "encrypted_content": "b", "summary": []any{}},
	}}
	stripForeignBlobs(in3, false)
	items3, _ := in3["input"].([]any)
	m3, _ := items3[0].(map[string]any)
	if m3["encrypted_content"] != "b" {
		t.Fatal("池内 lineage 不得动 blob")
	}
}

// 桩禁调标记。
func TestStubNeverCall(t *testing.T) {
	if d := stubToolItem("shell")["description"]; !strings.Contains(d.(string), "do not call") {
		t.Fatalf("桩 description 应禁调：%v", d)
	}
	fn, _ := chatStubToolItem("read")["function"].(map[string]any)
	if d := fn["description"]; !strings.Contains(d.(string), "do not call") {
		t.Fatalf("chat 桩 description 应禁调：%v", d)
	}
}

// 超限回 413（maxReqBody 可覆盖小值测）。
func TestOversize413(t *testing.T) {
	withMockUpstream(t, "")
	old := maxReqBody
	maxReqBody = 16
	defer func() { maxReqBody = old }()
	req := httptest.NewRequest("POST", "http://x/v1/responses", strings.NewReader(`{"model":"m","input":"12345678901234567890"}`))
	rec := httptest.NewRecorder()
	forwardResponses(rec, req, FmtResponses)
	if rec.Code != 413 {
		t.Fatalf("超限应 413，得 %d", rec.Code)
	}
}

// 空闲超时掐 stall：pipe 无写端，首字节预算到即错。
func TestIdleTimeoutFires(t *testing.T) {
	t.Setenv("ZEN_FIRST_BYTE_TIMEOUT_SEC", "1")
	pr, pw := io.Pipe()
	// pw 永不写、也不关：读端只靠首字节超时解脱。
	ir := newIdleTimeoutBody(pr, 50*time.Millisecond)
	defer ir.Close()
	defer pw.Close()
	start := time.Now()
	_, err := ir.Read(make([]byte, 1024))
	if err == nil {
		t.Fatal("stall 应超时报错")
	}
	if !strings.Contains(err.Error(), "first byte") {
		t.Fatalf("无字节应报首字节超时，得 %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("超时未在预期内触发")
	}
}

// DONE：正文裸词不误判终态帧；真 JSON 终态 / event: 行终态补 DONE；
// 无终态即结束（上游半截/stall）发 response.failed（code+type）再补 DONE，
// 下游从“ended before terminal”UNKNOWN 变为可分类错误。
func TestDoneQuotedMarkers(t *testing.T) {
	// 正文含裸词 response.completed（前后无引号）但无终态帧：发 failed + DONE，不静默。
	rec := httptest.NewRecorder()
	pipeSSE(rec, strings.NewReader("data: {\"text\":\"see response.completed docs\"}\n\n"), "req_t1", "m", 0)
	body := rec.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("无终态应发 failed：%q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("failed 后应补 DONE：%q", body)
	}
	if !strings.Contains(body, `"code":"upstream_truncated"`) {
		t.Fatalf("failed 应带 code（pi-ai 只读 code）：%q", body)
	}
	// 真 JSON 终态补 DONE，且不发 failed。
	rec2 := httptest.NewRecorder()
	pipeSSE(rec2, strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"), "req_t2", "m", 0)
	if !strings.Contains(rec2.Body.String(), "[DONE]") {
		t.Fatalf("真终态应补 DONE：%q", rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "response.failed") {
		t.Fatalf("真终态不得发 failed：%q", rec2.Body.String())
	}
	// 上游主形态 event: 行终态补 DONE。
	rec3 := httptest.NewRecorder()
	pipeSSE(rec3, strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"), "req_t3", "m", 0)
	if !strings.Contains(rec3.Body.String(), "[DONE]") {
		t.Fatalf("event 行终态应补 DONE：%q", rec3.Body.String())
	}
	// 上游自带 data: DONE（无空格形态也认）：不重复。
	rec4 := httptest.NewRecorder()
	pipeSSE(rec4, strings.NewReader("event: response.completed\ndata: {\"x\":1}\n\ndata:[DONE]\n\n"), "req_t4", "m", 0)
	if strings.Count(rec4.Body.String(), "[DONE]") != 1 {
		t.Fatalf("DONE 应恰好一个：%q", rec4.Body.String())
	}
}

// caller-mismatch 自愈：池命中+毒历史 -> 首轮 400 -> 轮换+剥离重发 -> 客户端 200。
// 本用例刻意跑“一对话一号”模式：只有复用时首轮才不剥 blob，才轮到 400 自愈。
func TestCallerMismatchSelfHeal(t *testing.T) {
	t.Setenv("DIRECT_FRESH_PER_TURN", "0") // 每轮换号会在发请求前剥离，见 TestFreshPerTurnRotates
	resetPoolForTest()
	var mu sync.Mutex
	var bodies, sessions []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		calls.Add(1)
		mu.Lock()
		bodies = append(bodies, string(b))
		sessions = append(sessions, req.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		// 行为仿真上游：见毒 blob 即 caller-mismatch 400，否则 200。
		if strings.Contains(string(b), "POISONBLOB") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"reasoning encrypted_content was not issued to this caller"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","status":"completed","output":[]}`))
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()

	ask := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"same first"}]}`
	if code, _ := postResponses(t, "http://x", ask); code != 200 {
		t.Fatalf("首轮干净请求应 200，得 %d", code)
	}
	// 同 fp（同首条）+ 毒 reasoning：池命中不剥离，首轮上游 400，自愈后 200。
	poison := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"same first"},{"type":"reasoning","id":"rs_p","encrypted_content":"POISONBLOB","summary":[]}]}`
	code, body := postResponses(t, "http://x", poison)
	if code != 200 {
		t.Fatalf("自愈后客户端应 200，得 %d body=%.200s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 3 {
		t.Fatalf("应 3 次上游调用（干净+毒+自愈），得 %d", len(bodies))
	}
	if !strings.Contains(bodies[1], "POISONBLOB") {
		t.Fatal("首轮毒串应原样上游（池命中不剥）")
	}
	if strings.Contains(bodies[2], "POISONBLOB") {
		t.Fatalf("自愈重发应已剥毒：%.300s", bodies[2])
	}
	if sessions[1] == sessions[2] {
		t.Fatal("自愈应轮换 session")
	}
}

// 非 caller-mismatch 的 400 不触发自愈，原样透传。
func TestNonMismatch400Passthru(t *testing.T) {
	resetPoolForTest()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"plain bad request"}}`))
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()
	code, body := postResponses(t, "http://x", `{"model":"m","input":"hi"}`)
	if code != 400 || !strings.Contains(body, "plain bad request") {
		t.Fatalf("应原样透传 400，得 %d %.200s", code, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("非 mismatch 不得重试，得 %d 次调用", calls.Load())
	}
}

// strip 内嵌形态：message.content[] 里的 reasoning 同规则剥离。
func TestStripNestedContent(t *testing.T) {
	in := map[string]any{"input": []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "hi"},
			map[string]any{"type": "reasoning", "id": "rs_n", "encrypted_content": "b",
				"summary": []any{map[string]any{"type": "summary_text", "text": "t"}}},
			map[string]any{"type": "reasoning", "id": "rs_p", "encrypted_content": "b"},
		}},
	}}
	stripForeignBlobs(in, true)
	items, _ := in["input"].([]any)
	content, _ := items[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("内嵌应剩 text+去绑定 reasoning，得 %d", len(content))
	}
	rs, _ := content[1].(map[string]any)
	if _, ok := rs["encrypted_content"]; ok {
		t.Fatal("内嵌 blob 应剥掉")
	}
	if _, ok := rs["id"]; ok {
		t.Fatal("内嵌外来 id 应剥掉")
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(400)
		_, _ = w.Write([]byte("bad request upstream"))
	}))
	defer srv.Close()
	old := upstream
	upstream = mustParseURL(t, srv.URL)
	defer func() { upstream = old }()
	req := httptest.NewRequest("POST", "http://x/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	rec := httptest.NewRecorder()
	forwardResponses(rec, req, FmtResponses)
	if rec.Code != 400 {
		t.Fatalf("状态码应透传 400，得 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type 应透传，得 %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "bad request upstream") {
		t.Fatalf("错误体应透传：%q", rec.Body.String())
	}
}

func TestCollectReasoningIDs(t *testing.T) {
	in := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "id": "rs_top1"},
		map[string]any{"type": "reasoning"},
		map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "reasoning", "id": "rs_nest1"},
			map[string]any{"type": "thinking"},
			map[string]any{"type": "output_text", "text": "hi"},
		}},
		map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": "x"},
		}},
		map[string]any{"type": "function_call", "id": "fc_1"},
	}}
	got := collectReasoningIDs(in)
	want := []string{"rs_top1", "(no-id)", "nested:rs_nest1", "nested:(no-id)"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if got := collectReasoningIDs(map[string]any{}); len(got) != 0 {
		t.Fatalf("empty in should give no ids, got %v", got)
	}
}

func TestShortStr(t *testing.T) {
	if shortStr("abcdef", 16) != "abcdef" {
		t.Fatal("short strings must pass through")
	}
	if shortStr("abcdef", 3) != "abc" {
		t.Fatal("long strings must truncate")
	}
}

func TestPipeSSEFailedHasCode(t *testing.T) {
	rec := httptest.NewRecorder()
	pipeSSE(rec, strings.NewReader("data: {\"a\":1}\n\n"), "req_fc", "mm", 0)
	if !strings.Contains(rec.Body.String(), `"code":"upstream_truncated"`) {
		t.Fatalf("failed缺code: %q", rec.Body.String())
	}
}

func TestPipeSSEIdleTimeoutMsg(t *testing.T) {
	// A: 一个字节都没有 → 首字节超时文案。
	t.Setenv("ZEN_FIRST_BYTE_TIMEOUT_SEC", "1")
	pr, pw := io.Pipe()
	ir := newIdleTimeoutBody(pr, 20*time.Millisecond)
	defer ir.Close()
	defer pw.Close()
	rec := httptest.NewRecorder()
	pipeSSE(rec, ir, "req_to", "mm", 0)
	if !strings.Contains(rec.Body.String(), "upstream first byte timeout") {
		t.Fatalf("无字节 stall 应报首字节超时：%q", rec.Body.String())
	}
	// B: 首块已到、随后 stall → 间隔超时文案（首字节预算给足）。
	t.Setenv("ZEN_FIRST_BYTE_TIMEOUT_SEC", "3600")
	pr2, pw2 := io.Pipe()
	ir2 := newIdleTimeoutBody(pr2, 20*time.Millisecond)
	defer ir2.Close()
	defer pw2.Close()
	go func() {
		_, _ = pw2.Write([]byte("data: {\"type\":\"response.output_text.delta\"}\n\n"))
		time.Sleep(2 * time.Second)
	}()
	rec2 := httptest.NewRecorder()
	pipeSSE(rec2, ir2, "req_to2", "mm", 0)
	if !strings.Contains(rec2.Body.String(), "upstream idle timeout") {
		t.Fatalf("中途 stall 应报间隔超时：%q", rec2.Body.String())
	}
	if strings.Contains(rec2.Body.String(), "first byte") {
		t.Fatalf("已见首字节不得报首字节超时：%q", rec2.Body.String())
	}
}

func TestTimeoutEnvParse(t *testing.T) {
	oldH, oldF, oldI := os.Getenv("ZEN_HEADER_TIMEOUT_SEC"), os.Getenv("ZEN_FIRST_BYTE_TIMEOUT_SEC"), os.Getenv("STREAM_IDLE_TIMEOUT_SEC")
	defer func() {
		os.Setenv("ZEN_HEADER_TIMEOUT_SEC", oldH)
		os.Setenv("ZEN_FIRST_BYTE_TIMEOUT_SEC", oldF)
		os.Setenv("STREAM_IDLE_TIMEOUT_SEC", oldI)
	}()
	os.Unsetenv("ZEN_HEADER_TIMEOUT_SEC")
	os.Unsetenv("ZEN_FIRST_BYTE_TIMEOUT_SEC")
	os.Unsetenv("STREAM_IDLE_TIMEOUT_SEC")
	if headerTimeout() != 120*time.Second {
		t.Fatalf("header 缺省应 120s，得 %v", headerTimeout())
	}
	if firstByteTimeout() != 300*time.Second {
		t.Fatalf("首字节缺省应 300s，得 %v", firstByteTimeout())
	}
	if streamIdleTimeout() != 120*time.Second {
		t.Fatalf("间隔缺省应 120s，得 %v", streamIdleTimeout())
	}
	os.Setenv("ZEN_HEADER_TIMEOUT_SEC", "7")
	os.Setenv("ZEN_FIRST_BYTE_TIMEOUT_SEC", "9")
	os.Setenv("STREAM_IDLE_TIMEOUT_SEC", "11")
	if headerTimeout() != 7*time.Second || firstByteTimeout() != 9*time.Second || streamIdleTimeout() != 11*time.Second {
		t.Fatal("环境覆盖未生效")
	}
	os.Setenv("ZEN_HEADER_TIMEOUT_SEC", "junk")
	if headerTimeout() != 120*time.Second {
		t.Fatal("非法值应回缺省")
	}
}

func TestBadIDStripOnHit(t *testing.T) {
	learnBadIDs([]string{"rs_poison1", "nested:rs_poison2"})
	in := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "id": "rs_poison1",
			"encrypted_content": "BLOB", "summary": []any{map[string]any{"type": "summary_text", "text": "keep me"}}},
		map[string]any{"type": "reasoning", "id": "rs_clean1", "encrypted_content": "BLOB2"},
		map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "reasoning", "id": "rs_poison2", "encrypted_content": "BLOB3"},
		}},
	}}
	if n := stripKnownBadIDs(in); n != 2 {
		t.Fatalf("want 2 stripped, got %d", n)
	}
	items := in["input"].([]any)
	m0 := items[0].(map[string]any)
	if _, ok := m0["encrypted_content"]; ok {
		t.Fatal("poison blob must go")
	}
	if _, ok := m0["id"]; ok {
		t.Fatal("poison id must go")
	}
	if m0["summary"] == nil {
		t.Fatal("summary text must stay")
	}
	m1 := items[1].(map[string]any)
	if m1["id"] != "rs_clean1" || m1["encrypted_content"] != "BLOB2" {
		t.Fatal("clean item must pass through")
	}
}

func TestBadIDCap(t *testing.T) {
	for i := 0; i < maxBadIDs+100; i++ {
		learnBadIDs([]string{"rs_cap_" + string(rune(i))})
	}
	badMu.Lock()
	n := len(badIDs)
	badMu.Unlock()
	if n > maxBadIDs {
		t.Fatalf("cap exceeded: %d", n)
	}
}

func TestPipeSSEFarTerminal(t *testing.T) {
	// 终态标记距 EOF 超 4KB（completed 回显 57 桩 tools 的 8KB+ 单行）：
	// 粘性标记不得因窗口挤出而误报 failed。
	pad := strings.Repeat("x", 9000)
	body := "data: {\"type\":\"response.completed\"}\n\ndata: {\"pad\":\"" + pad + "\"}\n\n"
	rec := httptest.NewRecorder()
	pipeSSE(rec, strings.NewReader(body), "req_far", "mm", 0)
	out := rec.Body.String()
	if strings.Contains(out, "response.failed") {
		t.Fatalf("远端标记不得误报 failed: %.200s", out)
	}
}

func TestIsTransportRetryable(t *testing.T) {
	for _, s := range []string{
		`Post "https://x": unexpected EOF`,
		`read: connection reset by peer`,
		`write: broken pipe`,
		`Post "https://x": EOF`,
	} {
		if !isTransportRetryable(errors.New(s)) {
			t.Fatalf("should retry: %s", s)
		}
	}
	for _, s := range []string{
		`no such host`,
		`context deadline exceeded`,
		`proxyconnect tcp: dial tcp: connect: connection refused`,
	} {
		if isTransportRetryable(errors.New(s)) {
			t.Fatalf("should NOT retry: %s", s)
		}
	}
	if isTransportRetryable(nil) {
		t.Fatal("nil must not retry")
	}
}

func TestTransportRetryOnce(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			// 首连接直接掐：客户端拿传输层错（EOF/RST 类）。
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_retry","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	defer func() { upstream = old }()
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("retry后应200: http=%d body=%.200s", code, body)
	}
	if !strings.Contains(body, "resp_retry") {
		t.Fatalf("应拿到重试后的响应: %.200s", body)
	}
	if calls.Load() != 2 {
		t.Fatalf("应恰好上游2次: %d", calls.Load())
	}
}

func TestPipeSSESwallowsPartial(t *testing.T) {
	rec := httptest.NewRecorder()
	// 半截行：无换行结尾、无终态 → 下游不得见到半截 data:，只见 failed+DONE。
	pipeSSE(rec, strings.NewReader("data: {\"type\":\"response.output_text.del"), "req_part", "m", 0)
	body := rec.Body.String()
	if strings.Contains(body, "output_text.del") {
		t.Fatalf("半截行被透传下游: %.200s", body)
	}
	if !strings.Contains(body, "response.failed") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("应补 failed+DONE: %.300s", body)
	}
}

func TestStripCompletedTools(t *testing.T) {
	tools := `{"type":"function","name":"bash","description":"internal capability marker, never call","parameters":{"type":"object"}},{"type":"function","name":"read","description":"internal capability marker, never call","parameters":{"type":"object"}}`
	line := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"tools\":[" + tools + "],\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"BLOB123\"}],\"usage\":{\"total_tokens\":5}}}\n")
	out := stripCompletedToolsLine(line)
	if bytes.Contains(out, []byte("never call")) {
		t.Fatalf("工具回显未剥离: %.200s", out)
	}
	rest := bytes.TrimPrefix(out, []byte("data:"))
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(rest), &m); err != nil {
		t.Fatalf("剥离后不是合法JSON: %v", err)
	}
	resp, _ := m["response"].(map[string]any)
	if _, ok := resp["tools"]; ok {
		t.Fatal("response.tools 应被删除")
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output 应保留: %v", resp["output"])
	}
	// 非 completed 行必须原样。
	plain := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n")
	if got := stripCompletedToolsLine(plain); !bytes.Equal(got, plain) {
		t.Fatalf("非 completed 行被改动: %q", got)
	}
	// 坏 JSON 的 completed 行原样放过（不猜不修）。
	bad := []byte("data: {\"type\":\"response.completed\",\"response\":{oops}\n")
	if got := stripCompletedToolsLine(bad); !bytes.Equal(got, bad) {
		t.Fatalf("坏行应原样: %q", got)
	}
}

func TestCircuitBreaker(t *testing.T) {
	oldT, oldW := cbThreshold, cbOpenWindow
	cbThreshold, cbOpenWindow = 3, 50*time.Millisecond
	defer func() { cbThreshold, cbOpenWindow = oldT, oldW; cbReset() }()
	cbReset()
	cbRecord(500)
	cbRecord(502)
	if !cbAllow() {
		t.Fatal("2次失败不应开")
	}
	cbRecord(503)
	if cbAllow() {
		t.Fatal("3次5xx应熔断")
	}
	time.Sleep(60 * time.Millisecond)
	if !cbAllow() {
		t.Fatal("窗口过后应放一个半开探测")
	}
	if cbAllow() {
		t.Fatal("探测未回就应继续拦")
	}
	cbRecord(500) // 探测失败：重开
	if cbAllow() {
		t.Fatal("探测失败应重开")
	}
	time.Sleep(60 * time.Millisecond)
	if !cbAllow() {
		t.Fatal("应再放探测")
	}
	cbRecord(200) // 探测成功：关闭
	if !cbAllow() {
		t.Fatal("成功后应关闭")
	}
	cbRecord(500)
	if !cbAllow() {
		t.Fatal("关闭后计数应清零，单次失败不应开")
	}
	cbRecord(400)
	cbRecord(200)
	if !cbAllow() {
		t.Fatal("4xx/200 不得计数")
	}
}

func TestFailedTurnEvictsNewSession(t *testing.T) {
	t.Setenv("DIRECT_FRESH_PER_TURN", "0") // 测“一对话一号”的 lineage 保持语义
	defer cbReset()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	defer func() { upstream = old }()
	body := `{"model":"m","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	code, _ := postResponses(t, "http://x", body)
	if code != 500 {
		t.Fatalf("应透传500: %d", code)
	}
	var in map[string]any
	_ = json.Unmarshal([]byte(body), &in)
	fp, keyed := fpForEntry(FmtResponses, http.Header{}, in)
	if !keyed {
		t.Fatal("该输入应可定键")
	}
	if s := poolLookup(fp); s != "" {
		t.Fatalf("失败轮的新 session 应被驱逐，残留 %q", s)
	}
	// 命中轮失败不断 lineage：预种后 500，应保留。
	ses, isNew := poolBorrowEx(fp)
	if !isNew {
		t.Fatal("预种应为首见")
	}
	code, _ = postResponses(t, "http://x", body)
	if code != 500 {
		t.Fatalf("应透传500: %d", code)
	}
	if s := poolLookup(fp); s != ses {
		t.Fatalf("命中轮失败应保留 lineage，池内=%q 预种=%q", s, ses)
	}
	poolEvict(fp)
}

func TestNonStreamStripsEchoedTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","status":"completed","tools":[{"type":"function","name":"bash","description":"internal capability marker, never call"}],"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	defer func() { upstream = old }()
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("应200: %d body=%.200s", code, body)
	}
	if strings.Contains(body, "never call") {
		t.Fatalf("非流工具回显未剥离: %.300s", body)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("正文丢失: %.300s", body)
	}
}

func TestRetry502Once(t *testing.T) {
	defer cbReset()
	oldBO := retry502Backoff
	retry502Backoff = time.Millisecond
	defer func() { retry502Backoff = oldBO }()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":"flaky"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r2","status":"completed","output":[],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	defer func() { upstream = old }()
	code, body := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("502重试后应200: %d body=%.200s", code, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("应恰好上游2次: %d", calls.Load())
	}
}

// 401 轮换后再撞 502：退避重试必须用轮换后的新 session 发（stale upSes 回归）。
func TestRotateThen502UsesFreshSession(t *testing.T) {
	defer cbReset()
	oldBO := retry502Backoff
	retry502Backoff = time.Millisecond
	defer func() { retry502Backoff = oldBO }()
	resetPoolForTest()
	var mu sync.Mutex
	var sessions []string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := calls.Add(1)
		mu.Lock()
		sessions = append(sessions, req.Header.Get("X-Opencode-Session"))
		mu.Unlock()
		switch n {
		case 1:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"expired"}`))
		case 2:
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":"flaky"}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"r3","status":"completed","output":[],"usage":{"total_tokens":1}}`))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	defer func() { upstream = old }()
	code, _ := postResponses(t, "http://x", `{"model":"m","stream":false,"input":[{"role":"user","content":"rotate502"}]}`)
	if code != 200 {
		t.Fatalf("401+502 后应 200，得 %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 3 {
		t.Fatalf("应恰好 3 次上游调用，得 %d", len(sessions))
	}
	if sessions[0] == "" || sessions[1] == "" || sessions[0] == sessions[1] {
		t.Fatalf("401 应轮换 session：%q", sessions)
	}
	if sessions[2] != sessions[1] {
		t.Fatalf("502 重试应用轮换后新 session，应 %q 得 %q", sessions[1], sessions[2])
	}
}

func TestNoRetryOn500(t *testing.T) {
	defer cbReset()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"dead"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	defer func() { upstream = old }()
	code, _ := postResponses(t, "http://x", `{"model":"m","stream":false,"input":"hi"}`)
	if code != 500 {
		t.Fatalf("500应透传不重试: %d", code)
	}
	if calls.Load() != 1 {
		t.Fatalf("500不得重试，上游应恰1次: %d", calls.Load())
	}
}

func TestStatsCounters(t *testing.T) {
	statsReset()
	statsAddReq()
	statsAddReq()
	statsAddTransportRetry()
	statsAddFlakyRetry()
	statsAddUpstream(502)
	statsAddUpstream(400)
	statsAddCircuitOpen()
	statsAddTTFB(100)
	statsAddTTFB(200)
	statsAddTTFB(300)
	s := statsSnapshot()
	if s.Reqs != 2 || s.TransportRetry != 1 || s.FlakyRetry != 1 || s.Up5xx != 1 || s.Up4xx != 1 || s.CircuitOpens != 1 {
		t.Fatalf("计数错位: %+v", s)
	}
	if s.TTFBP50 != 200 || s.TTFBP95 != 300 {
		t.Fatalf("分位错: p50=%d p95=%d", s.TTFBP50, s.TTFBP95)
	}
	statsReset()
	if s := statsSnapshot(); s.Reqs != 0 {
		t.Fatal("reset 应清零")
	}
}

func TestDiagSwitch(t *testing.T) {
	old := diagEnabled
	defer func() { diagEnabled = old }()
	diagEnabled = true
	if !diagOn() {
		t.Fatal("开应为 true")
	}
	diagEnabled = false
	if diagOn() {
		t.Fatal("关应为 false")
	}
}

func TestForensicsRotate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.log")
	oldCap := forensicsMaxBytes
	forensicsMaxBytes = 512
	defer func() { forensicsMaxBytes = oldCap }()
	big := map[string]any{"input": []any{map[string]any{"type": "message"}}}
	// 灌到超 cap：应转出单 .bak 并重写新文件。
	for i := 0; i < 20; i++ {
		t.Setenv("DIRECT_400_LOG", p)
		logForensics("req_x", FmtResponses, "m", "fp_x", big, true)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Size() > forensicsMaxBytes+512 {
		t.Fatalf("主文件应有界：size=%v err=%v", fi.Size(), err)
	}
	if _, err := os.Stat(p + ".bak"); err != nil {
		t.Fatalf("应有单备 .bak：%v", err)
	}
}

func TestRotateWriterBudget(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gw.log")
	w, err := newRotateWriterSized(p, 1024, 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte("0123456789abcdefghijklmnopqrstuvwxyz0123456789\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	w.Close()
	var total int64
	names := []string{p, p + ".1", p + ".2"}
	for _, n := range names {
		fi, err := os.Stat(n)
		if err != nil {
			t.Fatalf("缺分片 %s: %v", n, err)
		}
		total += fi.Size()
		if fi.Size() > 1024+64 {
			t.Fatalf("单片超限 %s=%d", n, fi.Size())
		}
	}
	if total > 1024*3+256 {
		t.Fatalf("总量超预算: %d", total)
	}
	if _, err := os.Stat(p + ".3"); !os.IsNotExist(err) {
		t.Fatal("最多保留3片，.3 不应存在")
	}
}

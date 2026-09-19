package main

// v0.2.1 硬化单测：P0 并发门/413/隔离/直通/桩禁调，P1 模型归一/轮换/落盘/DONE，P2 透传。
import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	if d := stubToolItem("bash")["description"]; !strings.Contains(d.(string), "never call") {
		t.Fatalf("桩 description 应禁调：%v", d)
	}
	fn, _ := chatStubToolItem("bash")["function"].(map[string]any)
	if d := fn["description"]; !strings.Contains(d.(string), "never call") {
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

// 空闲超时掐 stall：pipe 无写端，idle 到即错。
func TestIdleTimeoutFires(t *testing.T) {
	pr, pw := io.Pipe()
	// pw 永不写、也不关：读端只靠 idle 超时解脱。
	ir := newIdleTimeoutBody(pr, 50*time.Millisecond)
	defer ir.Close()
	defer pw.Close()
	start := time.Now()
	_, err := ir.Read(make([]byte, 1024))
	if err == nil {
		t.Fatal("stall 应超时报错")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("超时未在预期内触发")
	}
}

// DONE：正文裸词不误判；真 JSON 终态 / event: 行终态补 DONE。
func TestDoneQuotedMarkers(t *testing.T) {
	// 正文含裸词 response.completed（前后无引号）但无终态帧：不补。
	rec := httptest.NewRecorder()
	pipeSSE(rec, strings.NewReader("data: {\"text\":\"see response.completed docs\"}\n\n"))
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("裸词不得误判终态：%q", rec.Body.String())
	}
	// 真 JSON 终态补 DONE。
	rec2 := httptest.NewRecorder()
	pipeSSE(rec2, strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
	if !strings.Contains(rec2.Body.String(), "[DONE]") {
		t.Fatalf("真终态应补 DONE：%q", rec2.Body.String())
	}
	// 上游主形态 event: 行终态补 DONE。
	rec3 := httptest.NewRecorder()
	pipeSSE(rec3, strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	if !strings.Contains(rec3.Body.String(), "[DONE]") {
		t.Fatalf("event 行终态应补 DONE：%q", rec3.Body.String())
	}
	// 上游自带 data: DONE（无空格形态也认）：不重复。
	rec4 := httptest.NewRecorder()
	pipeSSE(rec4, strings.NewReader("event: response.completed\ndata: {\"x\":1}\n\ndata:[DONE]\n\n"))
	if strings.Count(rec4.Body.String(), "[DONE]") != 1 {
		t.Fatalf("DONE 应恰好一个：%q", rec4.Body.String())
	}
}

// 上游错误：状态码直透 + Content-Type 透传。
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

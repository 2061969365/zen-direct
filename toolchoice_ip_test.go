package main

// tool_choice 全路径禁 none（V2 实测）+ /ip 端点单测。
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 无用户工具：tool_choice 必须缺席（V2 拒 none），桩仍须在场（门禁）。
func TestToolChoiceOmittedWhenNoUserTools(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	body := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	code, _ := postResponses(t, "http://x", body)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	_, upBody, _ := getLastUp()
	var up map[string]any
	if err := json.Unmarshal([]byte(upBody), &up); err != nil {
		t.Fatalf("上游体非法: %v", err)
	}
	if _, ok := up["tool_choice"]; ok {
		t.Fatalf("无用户工具不得发 tool_choice: %v", up["tool_choice"])
	}
	// 桩仍须在场（门禁）。
	tools, _ := up["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("桩 tools 不得缺席")
	}
}

// 客户端无 tool_choice 但有真工具：不得强加 none。
func TestToolChoicePreservedWithUserTools(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	body := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"my_tool","description":"x","parameters":{"type":"object"}}]}`
	code, _ := postResponses(t, "http://x", body)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	_, upBody, _ := getLastUp()
	var up map[string]any
	if err := json.Unmarshal([]byte(upBody), &up); err != nil {
		t.Fatalf("上游体非法: %v", err)
	}
	if tc, ok := up["tool_choice"]; ok && tc == "none" {
		t.Fatalf("有真工具不得强加 none: %v", tc)
	}
}

// chat 入口同样：无 messages 外工具 → tool_choice 缺席。
func TestToolChoiceOmittedChatEntry(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	code, _ := postChat(t, "http://x", `{"model":"big-pickle","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	_, upBody, _ := getLastUp()
	var up map[string]any
	if err := json.Unmarshal([]byte(upBody), &up); err != nil {
		t.Fatalf("上游体非法: %v", err)
	}
	// chat entry + big-pickle → target responses（chatToResponsesReq），tool_choice 缺席。
	if _, ok := up["tool_choice"]; ok {
		t.Fatalf("chat 无工具不得发 tool_choice: %v", up["tool_choice"])
	}
}

// muse-spark 也不再需要 auto 特例：V2 下缺席即 auto 语义（200 实测）。
func TestToolChoiceOmittedForMuseSpark(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	body := `{"model":"muse-spark-1.3-contributor-free","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	code, _ := postResponses(t, "http://x", body)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	_, upBody, _ := getLastUp()
	var up map[string]any
	if err := json.Unmarshal([]byte(upBody), &up); err != nil {
		t.Fatalf("上游体非法: %v", err)
	}
	if _, ok := up["tool_choice"]; ok {
		t.Fatalf("muse-spark 无工具不得发 tool_choice: %v", up["tool_choice"])
	}
}

// /ip：mock 两源，先到先得。
func TestIPEndpoint(t *testing.T) {
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"203.0.113.7"}`))
	}))
	defer srvOK.Close()
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srvFail.Close()

	old := ipProviders
	ipProviders = []string{srvFail.URL, srvOK.URL}
	defer func() { ipProviders = old }()

	mux := http.NewServeMux()
	mux.HandleFunc("/ip", handleIP)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ip", nil))
	if rec.Code != 200 {
		t.Fatalf("http=%d body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("非法 JSON: %v", err)
	}
	if m["ip"] != "203.0.113.7" {
		t.Fatalf("ip=%q", m["ip"])
	}
}

// /ip：全源失败 → 502。
func TestIPEndpointAllFail(t *testing.T) {
	srvFail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srvFail.Close()
	old := ipProviders
	ipProviders = []string{srvFail.URL}
	defer func() { ipProviders = old }()

	mux := http.NewServeMux()
	mux.HandleFunc("/ip", handleIP)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ip", nil))
	if rec.Code != 502 {
		t.Fatalf("全挂应502: %d %s", rec.Code, rec.Body.String())
	}
}

// validIPv4 边界。
func TestValidIPv4(t *testing.T) {
	ok := []string{"1.2.3.4", "255.255.255.255", "10.0.0.1"}
	bad := []string{"", "1.2.3", "1.2.3.256", "a.b.c.d", "1.2.3.4.5", "999.1.1.1"}
	for _, s := range ok {
		if !validIPv4(s) {
			t.Errorf("%q 应合法", s)
		}
	}
	for _, s := range bad {
		if validIPv4(s) {
			t.Errorf("%q 应非法", s)
		}
	}
}

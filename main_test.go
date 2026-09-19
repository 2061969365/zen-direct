package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// 上游抓包全局量：mock handler（独立 goroutine）与断言 goroutine 并发读写，
// 必须加锁，否则 go test -race 必报（生产无此路径）。
var (
	lastUpMu   sync.Mutex
	lastUpPath string
	lastUpBody string
	lastUpSess string
)

func setLastUp(path, body, sess string) {
	lastUpMu.Lock()
	defer lastUpMu.Unlock()
	lastUpPath, lastUpBody, lastUpSess = path, body, sess
}

func getLastUp() (path, body, sess string) {
	lastUpMu.Lock()
	defer lastUpMu.Unlock()
	return lastUpPath, lastUpBody, lastUpSess
}

func withMockUpstream(t *testing.T, sse string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b := make([]byte, 0, 4096)
		buf := make([]byte, 1024)
		for {
			n, err := req.Body.Read(buf)
			b = append(b, buf[:n]...)
			if err != nil || len(b) > 64<<10 {
				break
			}
		}
		setLastUp(req.URL.Path, string(b), req.Header.Get("X-Opencode-Session"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	old := upstream
	upstream = u
	t.Cleanup(func() { upstream = old })
}

func postResponses(t *testing.T, srvURL, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", srvURL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, req *http.Request) {
		forwardResponses(w, req, FmtResponses)
	})
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func postChat(t *testing.T, srvURL, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", srvURL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, req *http.Request) {
		forwardResponses(w, req, FmtChat)
	})
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// 有终态无 DONE：网关补 DONE。
func TestStreamAppendsDone(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\"}\n\n")
	code, body := postResponses(t, "http://x", `{"model":"m","stream":true,"input":"hi"}`)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	if !strings.Contains(body, "response.completed") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("应补 DONE：%q", body)
	}
}

// 自带 DONE：不重复。
func TestStreamKeepsSingleDone(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n")
	_, body := postResponses(t, "http://x", `{"model":"m","stream":true,"input":"hi"}`)
	if strings.Count(body, "[DONE]") != 1 {
		t.Fatalf("DONE 应恰好一个：%q", body)
	}
}

// 半截流无终态：发 response.failed（可分类）再补 DONE，不静默断流。
func TestStreamTruncatedNoDone(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.output_text.delta\"}\n\n")
	_, body := postResponses(t, "http://x", `{"model":"m","stream":true,"input":"hi"}`)
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("半截流应发 failed：%q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("failed 后应补 DONE：%q", body)
	}
}

// chat 入口 + mimo 模型：上游走 /chat/completions，请求体为 chat 形。
func TestChatEntryRoutesChat(t *testing.T) {
	withMockUpstream(t, "data: ok\n\n")
	code, _ := postChat(t, "http://x", `{"model":"mimo-v2.5-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("http=%d", code)
	}
	upPath, upBody, _ := getLastUp()
	if upPath != "/chat/completions" {
		t.Fatalf("上游路径错误：%s", upPath)
	}
	if !strings.Contains(upBody, `"messages"`) {
		t.Fatalf("上游体应为 chat 形：%.200s", upBody)
	}
}

// messages 入口：400 明拒。
func TestMessagesEntryRejected(t *testing.T) {
	withMockUpstream(t, "")
	req := httptest.NewRequest("POST", "http://x/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	forwardResponses(rec, req, FmtMessages)
	if rec.Code != 400 {
		t.Fatalf("messages 应 400，得 %d", rec.Code)
	}
}

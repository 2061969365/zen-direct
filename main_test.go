package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func withMockUpstream(t *testing.T, sse string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
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
	mux.HandleFunc("/v1/responses", forwardResponses)
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

// 半截流无终态：绝不补 DONE。
func TestStreamTruncatedNoDone(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.output_text.delta\"}\n\n")
	_, body := postResponses(t, "http://x", `{"model":"m","stream":true,"input":"hi"}`)
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("半截流不应补 DONE：%q", body)
	}
}

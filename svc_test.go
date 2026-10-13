package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestSvcMintLive 经本机真 service 铸一个 session（SVC_LIVE=1 才跑）。
// CI 跳过；铸出的 session 真实但零调用（本测试不发 zen 流量）。
func TestSvcMintLive(t *testing.T) {
	if os.Getenv("SVC_LIVE") == "" {
		t.Skip("set SVC_LIVE=1 with a running opencode service")
	}
	svcInvalidate()
	sid := svcMintSession()
	if sid == "" {
		t.Fatal("live service mint failed")
	}
	if !strings.HasPrefix(sid, "ses_") {
		t.Fatalf("bad session id %q", sid)
	}
	t.Logf("minted %s", sid)
	svcInvalidate()
}
func fakeSvc(t *testing.T, id string, status int) *httptest.Server {
	t.Helper()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:testpw"))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session" || r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"data":{"id":"` + id + `"}}`))
	}))
}

func TestSvcMintOK(t *testing.T) {
	ts := fakeSvc(t, "ses_svc_test001", 200)
	defer ts.Close()
	t.Setenv("OPENCODE_SERVICE_URL", ts.URL)
	t.Setenv("OPENCODE_SERVER_PASSWORD", "testpw")
	t.Setenv("SVC_OFF", "")
	svcInvalidate()
	if got := svcMintSession(); got != "ses_svc_test001" {
		t.Fatalf("应铸出假 service 的 session，得 %q", got)
	}
	svcInvalidate()
}

func TestSvcMintAuthFail(t *testing.T) {
	ts := fakeSvc(t, "ses_svc_test001", 200)
	defer ts.Close()
	t.Setenv("OPENCODE_SERVICE_URL", ts.URL)
	t.Setenv("OPENCODE_SERVER_PASSWORD", "wrong")
	svcInvalidate()
	if got := svcMintSession(); got != "" {
		t.Fatalf("401 应回空，得 %q", got)
	}
	svcInvalidate()
}

func TestSvcMintDisabled(t *testing.T) {
	t.Setenv("SVC_OFF", "1")
	t.Setenv("OPENCODE_SERVICE_URL", "http://127.0.0.1:1")
	if got := svcMintSession(); got != "" {
		t.Fatalf("SVC_OFF 应回空，得 %q", got)
	}
}

func TestSvcMintBadID(t *testing.T) {
	ts := fakeSvc(t, "msg_not_a_session", 200)
	defer ts.Close()
	t.Setenv("OPENCODE_SERVICE_URL", ts.URL)
	t.Setenv("OPENCODE_SERVER_PASSWORD", "testpw")
	svcInvalidate()
	if got := svcMintSession(); got != "" {
		t.Fatalf("错前缀应回空，得 %q", got)
	}
	svcInvalidate()
}

// 非回环 OPENCODE_SERVICE_URL 拒收：Basic 密码不许出本机明文 HTTP。
// 拒后退发现；发现用坏二进制必然失败，全链回空（不碰任何远端）。
func TestSvcRefusesRemoteURL(t *testing.T) {
	t.Setenv("OPENCODE_SERVICE_URL", "http://example.com:4096")
	t.Setenv("OPENCODE_BIN", `C:\definitely\not\here\opencode.exe`)
	t.Setenv("OPENCODE_SERVER_PASSWORD", "testpw")
	svcInvalidate()
	defer svcInvalidate()
	if got := svcMintSession(); got != "" {
		t.Fatalf("远端 URL 应回空，得 %q", got)
	}
}

func TestSvcPasswordTrim(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "  testpw  ")
	if got := svcPassword(); got != "testpw" {
		t.Fatalf("密码应 trim，得 %q", got)
	}
	if !strings.HasPrefix(svcBasicAuth(), "Basic ") {
		t.Fatalf("Basic 头形状不对: %q", svcBasicAuth())
	}
}

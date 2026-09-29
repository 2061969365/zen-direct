package main

// table_refresh_test.go: 官方 Endpoints 表解析 + 合并单测（纯离线，用内嵌样本文本）。
import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const sampleEndpointsMD = `## Endpoints

| Model | Model ID | Endpoint | AI SDK Package |
| ----- | -------- | -------- | -------------- |
| GPT 5.5 | gpt-5.5 | ` + "`https://opencode.ai/zen/v1/responses`" + ` | @ai-sdk/openai |
| Big Pickle | big-pickle | ` + "`https://opencode.ai/zen/v1/chat/completions`" + ` | @ai-sdk/openai-compatible |
| Claude Sonnet 5 | claude-sonnet-5 | ` + "`https://opencode.ai/zen/v1/messages`" + ` | @ai-sdk/anthropic |
| Gemini 3 Flash | gemini-3-flash | ` + "`https://opencode.ai/zen/v1/models/gemini-3-flash`" + ` | @ai-sdk/google |
| Jev 1.13 Free | jev-1.13-free | ` + "`https://opencode.ai/zen/v1/systemone`" + ` | - |
| Space Bunny Free | space-bunny-free | ` + "`https://opencode.ai/zen/v1/chat/completions`" + ` | @ai-sdk/openai-compatible |

## Pricing

| Model | Input | Output |
| ----- | ----- | ------ |
| Big Pickle | Free | Free |

## Deprecated models

| Model | Deprecation date |
| ----- | ---------------- |
| GPT 5 Codex | July 23, 2026 |

noise line without pipes
`

func TestParseEndpointsTable(t *testing.T) {
	formats, unsupported := parseEndpointsTable(sampleEndpointsMD)

	want := map[string]Format{
		"gpt-5.5":          FmtResponses,
		"big-pickle":       FmtChat,
		"claude-sonnet-5":  FmtMessages,
		"gemini-3-flash":   FmtGemini,
		"space-bunny-free": FmtChat,
	}
	for id, f := range want {
		if formats[id] != f {
			t.Errorf("%s: got %q want %q", id, formats[id], f)
		}
	}
	if len(formats) != len(want) {
		t.Errorf("formats 应 %d 条，得 %d: %v", len(want), len(formats), formats)
	}
	// systemone 进 unsupported，不进 formats。
	if _, ok := formats["jev-1.13-free"]; ok {
		t.Error("jev 不应进 formats")
	}
	if ep := unsupported["jev-1.13-free"]; !strings.Contains(ep, "systemone") {
		t.Errorf("jev 应进 unsupported，ep=%q", ep)
	}
	// 价格表/下线表/表头不得污染。
	for _, bad := range []string{"model", "model id", "free", "gpt 5 codex"} {
		if _, ok := formats[bad]; ok {
			t.Errorf("噪声 %q 不应进表", bad)
		}
	}
}

func TestEndpointToFormat(t *testing.T) {
	cases := map[string]Format{
		"https://opencode.ai/zen/v1/responses":        FmtResponses,
		"https://opencode.ai/zen/v1/chat/completions": FmtChat,
		"https://opencode.ai/zen/v1/messages":         FmtMessages,
		"https://opencode.ai/zen/v1/models/gemini-3x": FmtGemini,
	}
	for ep, want := range cases {
		if f, ok := endpointToFormat(ep); !ok || f != want {
			t.Errorf("%s: got %q,%v want %q", ep, f, ok, want)
		}
	}
	if _, ok := endpointToFormat("https://opencode.ai/zen/v1/systemone"); ok {
		t.Error("systemone 应不支持")
	}
}

// refreshTableFromOfficial 走 httptest mock（不碰外网），合并进全局表后清理。
func TestRefreshMergesTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(sampleEndpointsMD))
	}))
	defer srv.Close()

	oldURL := os.Getenv("ZEN_TABLE_URL")
	defer os.Setenv("ZEN_TABLE_URL", oldURL)
	os.Setenv("ZEN_TABLE_URL", srv.URL)

	seedBuiltinTable()
	before := tableModelCount()
	n := refreshTableFromOfficial()
	if n < 0 {
		t.Fatal("mock 刷新应成功")
	}
	// space-bunny-free 本就在快照 chat 不变；big-pickle 前缀 chat 一致；
	// 至少 gemini/claude 等官方行应存在。
	for _, id := range []string{"gemini-3-flash", "claude-sonnet-5", "gpt-5.5"} {
		if _, ok := lookupFormat(id); !ok {
			t.Errorf("刷新后 %s 应可查", id)
		}
	}
	if _, ok := lookupUnsupported("jev-1.13-free"); !ok {
		t.Error("刷新后 jev 应进 unsupported")
	}
	after := tableModelCount()
	if after < before {
		t.Errorf("合并不得缩表：%d -> %d", before, after)
	}
	// 清理测试污染。
	tableMu.Lock()
	delete(unsupportedModels, "jev-1.13-free")
	tableMu.Unlock()
}

// 404 上游 → -1，快照不动。
func TestRefreshFailureKeepsSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer srv.Close()

	oldURL := os.Getenv("ZEN_TABLE_URL")
	defer os.Setenv("ZEN_TABLE_URL", oldURL)
	os.Setenv("ZEN_TABLE_URL", srv.URL)

	seedBuiltinTable()
	before := tableModelCount()
	if n := refreshTableFromOfficial(); n != -1 {
		t.Errorf("404 应返回 -1，得 %d", n)
	}
	if tableModelCount() != before {
		t.Error("失败时快照不得动")
	}
}

func TestTableRefreshDisabled(t *testing.T) {
	old := os.Getenv("ZEN_TABLE_REFRESH")
	defer os.Setenv("ZEN_TABLE_REFRESH", old)
	for _, v := range []string{"0", "false", "off", "no"} {
		os.Setenv("ZEN_TABLE_REFRESH", v)
		if !tableRefreshDisabled() {
			t.Errorf("%q 应禁用", v)
		}
	}
	os.Setenv("ZEN_TABLE_REFRESH", "1")
	if tableRefreshDisabled() {
		t.Error("1 不应禁用")
	}
}

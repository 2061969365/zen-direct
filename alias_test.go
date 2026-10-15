package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 别名往返：保留名进出一致，非保留名原样，字面量 zd_ 碰撞行为锁定。
func TestAliasRoundTrip(t *testing.T) {
	for _, n := range aliasReserved {
		if got := aliasFor(n); got != "zd_"+n {
			t.Fatalf("%s 应别名为 zd_%s，得 %q", n, n, got)
		}
		if got := unaliasToolName("zd_" + n); got != n {
			t.Fatalf("zd_%s 应还原，得 %q", n, got)
		}
	}
	for _, n := range []string{"mytool", "subagent", "todo_write", "web_fetch", "skill", ""} {
		if got := aliasFor(n); got != n {
			t.Fatalf("非保留名 %q 不应改名，得 %q", n, got)
		}
		if got := unaliasToolName(n); got != n {
			t.Fatalf("非别名 %q 不应改名，得 %q", n, got)
		}
	}
}

// 请求改写：tools 定义 + 历史调用 + tool_choice 三形态。
func TestAliasRequestMapping(t *testing.T) {
	mkResp := func(name string) map[string]any {
		return map[string]any{"type": "function", "name": name, "description": "d",
			"parameters": map[string]any{"type": "object"}}
	}
	in := map[string]any{
		"tools": []any{mkResp("bash"), mkResp("mytool")},
		"input": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "edit", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		},
		"tool_choice": map[string]any{"type": "function", "name": "read"},
	}
	if n := aliasRequestTools(in, FmtResponses); n != 3 {
		t.Fatalf("应改名 3 处（bash/edit/read），得 %d", n)
	}
	items, _ := in["tools"].([]any)
	got := map[string]string{}
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		got[asStr(m["name"])] = asStr(m["description"])
	}
	if _, ok := got["zd_bash"]; !ok {
		t.Fatalf("bash 应改 zd_bash：%v", got)
	}
	if _, ok := got["bash"]; ok {
		t.Fatalf("bash 原名不应残留：%v", got)
	}
	if _, ok := got["mytool"]; !ok {
		t.Fatalf("mytool 应保留：%v", got)
	}
	arr, _ := in["input"].([]any)
	fc := asMap(arr[1])
	if asStr(fc["name"]) != "zd_edit" {
		t.Fatalf("历史 function_call 应改名，得 %q", asStr(fc["name"]))
	}
	if asStr(asMap(arr[2])["call_id"]) != "c1" {
		t.Fatal("function_call_output 不应动")
	}
	tc := asMap(in["tool_choice"])
	if asStr(asMap(tc["function"])["name"]) != "" {
		t.Fatalf("tool_choice 非 function 形不应动：%v", tc)
	}
	if asStr(tc["name"]) != "" && asStr(tc["name"]) != "zd_read" {
		t.Fatalf("tool_choice 应改名，得 %v", tc)
	}
}

// chat 形：function.name 改写。
func TestAliasRequestMappingChat(t *testing.T) {
	in := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{"id": "t1", "type": "function",
					"function": map[string]any{"name": "write", "arguments": "{}"}}}},
		},
		"tools": []any{map[string]any{"type": "function",
			"function": map[string]any{"name": "glob", "description": "d"}}},
		"tool_choice": "glob",
	}
	if n := aliasRequestTools(in, FmtChat); n != 3 {
		t.Fatalf("应改名 3 处，得 %d", n)
	}
	tcHist := asArr(asMap(asArr(in["messages"])[1])["tool_calls"])
	fn := asMap(asMap(tcHist[0])["function"])
	if asStr(fn["name"]) != "zd_write" {
		t.Fatalf("历史 tool_calls 应改名，得 %q", asStr(fn["name"]))
	}
	if asStr(in["tool_choice"]) != "zd_glob" {
		t.Fatalf("字符串 tool_choice 应改名，得 %q", in["tool_choice"])
	}
}

// 端到端（mock 上游）：harness 碰撞名进 → 别名上游；上游别名调用回 → 原名下游。
func TestAliasEndToEnd(t *testing.T) {
	resetPoolForTest()
	withMockUpstream(t, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"resp_fc1\",\"call_id\":\"call_9\",\"name\":\"zd_edit\",\"arguments\":\"{}\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r9\",\"status\":\"completed\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	body := `{"model":"m","stream":true,"input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"edit","description":"d","parameters":{"type":"object"}}]}`
	code, out := postResponses(t, "http://x", body)
	if code != 200 {
		t.Fatalf("http=%d body=%.200s", code, out)
	}
	_, upBody, _ := getLastUp()
	var up map[string]any
	if err := json.Unmarshal([]byte(upBody), &up); err != nil {
		t.Fatalf("上游体非法: %v", err)
	}
	names := map[string]bool{}
	for _, raw := range asArr(up["tools"]) {
		names[asStr(asMap(raw)["name"])] = true
	}
	if !names["zd_edit"] || names["edit"] {
		t.Fatalf("上游应见别名：%v", names)
	}
	if !strings.Contains(out, `"name":"edit"`) || strings.Contains(out, "zd_edit") {
		t.Fatalf("下游应还原原名：%.300s", out)
	}
}

// 字节恢复：转义态碰不到，非别名不动。
func TestRestoreAliasNamesBytes(t *testing.T) {
	in := []byte(`data: {"type":"x","name":"zd_read"} {"name": "zd_bash"} {"name":"mytool"}`)
	out := string(restoreAliasNames(in))
	if strings.Contains(out, "zd_read") || strings.Contains(out, "zd_bash") {
		t.Fatalf("别名应还原：%s", out)
	}
	if !strings.Contains(out, `"name":"read"`) || !strings.Contains(out, `"name": "bash"`) {
		t.Fatalf("还原形状不对：%s", out)
	}
	esc := []byte(`data: {"arguments":"{\\"name\\":\\"zd_read\\"}"}`)
	if string(restoreAliasNames(esc)) != string(esc) {
		t.Fatal("转义态不得改动")
	}
	if string(restoreAliasNames([]byte("plain line"))) != "plain line" {
		t.Fatal("无 zd_ 行应原样（零拷贝语义可顺带）")
	}
}

// 工具名提取：双形态通吃，去重保序（4xx 日志定位新增保留名用）。
func TestToolNamesForLog(t *testing.T) {
	in := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "bash", "description": "d"},
		map[string]any{"type": "function",
			"function": map[string]any{"name": "glob", "description": "d"}},
		map[string]any{"type": "function", "name": "bash", "description": "d"},
		"garbage",
	}}
	got := toolNamesForLog(in)
	if len(got) != 2 || got[0] != "bash" || got[1] != "glob" {
		t.Fatalf("应去重保序 [bash glob]，得 %v", got)
	}
	if len(toolNamesForLog(map[string]any{})) != 0 {
		t.Fatal("空输入应空")
	}
}

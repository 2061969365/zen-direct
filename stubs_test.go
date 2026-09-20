package main

import (
	"encoding/json"
	"testing"
)

// 客户端自带工具排前面；缺的桩按表补齐；重名不重复。
func TestMergeStubTools(t *testing.T) {
	mk := func(name string) map[string]any {
		return map[string]any{
			"type": "function", "name": name,
			"description": "client tool",
			"parameters":  map[string]any{"type": "object"},
		}
	}
	in := map[string]any{"tools": []any{mk("run_shell"), mk("bash")}}
	mergeStubTools(in)
	items, _ := in["tools"].([]any)
	if len(items) != len(stubToolNames)+1 {
		t.Fatalf("应为桩数+1=%d，得 %d", len(stubToolNames)+1, len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["name"] != "run_shell" {
		t.Fatalf("客户端工具必须排第一：%v", first["name"])
	}
	seen := map[string]int{}
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		seen[m["name"].(string)]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("重复：%s x%d", n, c)
		}
	}
	// 客户端的 bash 定义必须原样保留（不能被桩覆盖），桩名必须齐。
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		if m["name"] == "bash" && m["description"] != "client tool" {
			t.Fatalf("客户端 bash 被桩覆盖：%v", m["description"])
		}
	}
	for _, n := range stubToolNames {
		if seen[n] != 1 {
			t.Fatalf("桩名缺失或重复：%s x%d", n, seen[n])
		}
	}
	// 无 tools 键时全量补桩。
	empty := map[string]any{}
	mergeStubTools(empty)
	if items, _ := empty["tools"].([]any); len(items) != len(stubToolNames) {
		t.Fatalf("空输入应全量补桩，得 %d", len(items))
	}
}

// v0.2.9 slim: 13 non-cad names (probe4 2026-09-20 C6=200); must contain read.
func TestStubSetSlim13(t *testing.T) {
	if len(stubToolNames) != 13 {
		t.Fatalf("桩应精简为13，得 %d", len(stubToolNames))
	}
	hasRead := false
	for _, n := range stubToolNames {
		if n == "read" {
			hasRead = true
		}
		if len(n) >= 8 && n[:8] == "cad-mcp_" {
			t.Fatalf("cad 桩应已删除：%s", n)
		}
	}
	if !hasRead {
		t.Fatal("桩必须含 read（门禁必要条件）")
	}
}

// provider/model-id 去前缀，裸 id 不动。
func TestNormalizeModel(t *testing.T) {
	in := map[string]any{"model": "test/muse-spark-1.3-contributor-free"}
	normalizeModel(in)
	if in["model"] != "muse-spark-1.3-contributor-free" {
		t.Fatalf("前缀未去掉：%v", in["model"])
	}
	bare := map[string]any{"model": "muse-spark-1.3-contributor-free"}
	normalizeModel(bare)
	if bare["model"] != "muse-spark-1.3-contributor-free" {
		t.Fatalf("裸 id 不应动：%v", bare["model"])
	}
}

func TestStubDedupOnWire(t *testing.T) {
	withMockUpstream(t, "data: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n")
	body := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"function","name":"bash","description":"client tool","parameters":{"type":"object"}},{"type":"function","name":"my_tool","description":"x","parameters":{"type":"object"}}]}`
	code, _ := postResponses(t, "http://x", body)
	if code != 200 {
		t.Fatalf("mock上游应200: %d", code)
	}
	_, upBody, _ := getLastUp()
	var up map[string]any
	if err := json.Unmarshal([]byte(upBody), &up); err != nil {
		t.Fatalf("上游体应为合法JSON: %v", err)
	}
	tools, _ := up["tools"].([]any)
	seen := map[string]int{}
	desc := map[string]string{}
	for _, raw := range tools {
		m, _ := raw.(map[string]any)
		n, _ := m["name"].(string)
		seen[n]++
		if d, ok := m["description"].(string); ok {
			desc[n] = d
		}
	}
	for _, n := range stubToolNames {
		if seen[n] != 1 {
			t.Fatalf("线名缺失或重复：%s x%d（共%d个tools）", n, seen[n], len(tools))
		}
	}
	if seen["my_tool"] != 1 || desc["bash"] != "client tool" {
		t.Fatalf("客户端工具未原样上游：bash=%q my_tool=%d", desc["bash"], seen["my_tool"])
	}
}

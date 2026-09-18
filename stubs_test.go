package main

import (
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
	// 无 tools 键时全量补桩。
	empty := map[string]any{}
	mergeStubTools(empty)
	if items, _ := empty["tools"].([]any); len(items) != len(stubToolNames) {
		t.Fatalf("空输入应全量补桩，得 %d", len(items))
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

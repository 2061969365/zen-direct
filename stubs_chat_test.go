package main

import (
	"testing"
)

// chat 桩：shell/read，chat functions 形。
func TestMergeChatStubTools(t *testing.T) {
	mk := func(name string) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name, "description": "client",
				"parameters": map[string]any{"type": "object"},
			},
		}
	}
	in := map[string]any{"tools": []any{mk("mytool")}}
	mergeChatStubTools(in)
	items, _ := in["tools"].([]any)
	if len(items) != len(stubToolNames)+1 {
		t.Fatalf("应为桩数+1=%d，得 %d", len(stubToolNames)+1, len(items))
	}
	first, _ := items[0].(map[string]any)
	fn, _ := first["function"].(map[string]any)
	if fn["name"] != "mytool" {
		t.Fatalf("客户端工具必须排第一：%v", fn["name"])
	}
	seen := map[string]int{}
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		n, _ := fn["name"].(string)
		seen[n]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("重复：%s x%d", n, c)
		}
	}
}

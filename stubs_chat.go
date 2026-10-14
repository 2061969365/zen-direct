package main

// chatStubToolItem chat functions 形桩（官方描述+真 schema，见 stubs.go）。
func chatStubToolItem(name string) map[string]any {
	desc, params, req := stubSpec(name)
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": params,
			"required": []any{req}, "additionalProperties": false},
	}}
}

// mergeChatStubTools chat 形注桩（客户端工具排前，按 function.name 去重）。
func mergeChatStubTools(in map[string]any) {
	toolName := func(raw any) string {
		m, ok := raw.(map[string]any)
		if !ok {
			return ""
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				return n
			}
		}
		if n, ok := m["name"].(string); ok {
			return n
		}
		return ""
	}
	have := map[string]bool{}
	items, _ := in["tools"].([]any)
	for _, raw := range items {
		if n := toolName(raw); n != "" {
			have[n] = true
		}
	}
	for _, n := range stubToolNames {
		if !have[n] {
			items = append(items, chatStubToolItem(n))
		}
	}
	in["tools"] = items
}

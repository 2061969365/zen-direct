package main

// chatStubToolItem chat functions 形桩（mimo 已验：名对即可，body 忽略）。
func chatStubToolItem(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": "opencode built-in",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
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

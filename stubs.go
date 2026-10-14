// stubs.go: V2 门禁桩 shell/read（官方描述前缀 + 真参数 schema，缺一即 403）。
// 对照 oc2api v2.6.0（抓 2.0.29 官方 tool/plugin/{shell,read}.ts）。
package main

// V2 门禁桩：仅 shell/read，且描述前缀与参数 schema 须与官方一致，否则 403。
// 空 schema/假描述不再放行。
var stubToolNames = []string{"shell", "read"}

// stubNeverCall 禁调后缀：桩只为过门禁存在，instruction-following 强模型遵守。
const stubNeverCall = " Reserved for the host runtime; do not call or select this function. Use tools explicitly supplied by the user instead."

// stubToolItem builds an official-shaped function tool entry (responses 扁平形态)。
func stubToolItem(name string) map[string]any {
	desc, params, req := stubSpec(name)
	return map[string]any{"type": "function", "name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": params,
			"required": []any{req}, "additionalProperties": false}}
}

// stubSpec 单桩官方定义。required 返名单（调用方包 []any）。
func stubSpec(name string) (string, map[string]any, string) {
	if name == "shell" {
		return "Execute a shell command and return its output." + stubNeverCall,
			map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
				"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in"},
				"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds"},
				"background": map[string]any{"type": "boolean", "description": "Run the command in the background"},
			}, "command"
	}
	return "Read the contents of a file or directory." + stubNeverCall,
		map[string]any{
			"path":   map[string]any{"type": "string", "description": "File or directory to read"},
			"offset": map[string]any{"type": "integer", "minimum": 0, "description": "Starting line or directory entry (1-based)"},
			"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "Maximum lines or directory entries to read"},
		}, "path"
}

// mergeStubTools appends missing stub names to req tools (client tools first, dedupe by name).
// 注意：显式 tools:null/缺省也会被注桩——门禁要求名在，无桩必 403，
// “不要工具”语义让位于“能通话”，by design。
func mergeStubTools(in map[string]any) {
	have := map[string]bool{}
	items, _ := in["tools"].([]any)
	for _, raw := range items {
		if m, ok := raw.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				have[n] = true
			}
		}
	}
	for _, n := range stubToolNames {
		if !have[n] {
			items = append(items, stubToolItem(n))
		}
	}
	in["tools"] = items
}

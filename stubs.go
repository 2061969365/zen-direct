// stubs.go: opencode tool-name allowlist (descriptions intentionally stubbed).
// zen FreeTier gate requires 'read' + companions on stream+tools requests
// (probe4 2026-09-20: 13 non-cad names = 200; cad 44 unrelated to gate).
// 57 -> 13 slim (v0.2.9): drop 44 cad-mcp_* (upstream echoes tools in every
// response.completed; 8KB saved per round-trip both directions).
// Generated from a captured opencode request; regenerate if the gate changes.
package main

var stubToolNames = []string{
	"bash",
	"edit",
	"glob",
	"grep",
	"list_mcp_resource_templates",
	"list_mcp_resources",
	"read",
	"read_mcp_resource",
	"skill",
	"task",
	"todowrite",
	"webfetch",
	"write",
}

// stubToolItem builds a minimal function tool entry (bodies ignored by gate).
// description 显式禁调：桩只为过 FreeTier 门禁存在，无工具请求被注桩后，
// 模型若调桩名（如 bash）harness 无法执行即卡死；此句 instruction-following
// 强的模型大概率遵守，残余风险见 README/注释（toolless 场景留意 function_call）。
func stubToolItem(name string) map[string]any {
	return map[string]any{"type": "function", "name": name, "description": "internal capability marker, never call", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
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

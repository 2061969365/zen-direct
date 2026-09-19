package main

import (
	"os"
	"strings"
)

// stripForeignBlobs 剥外来 blob 留 summary（C 案兜底）。
// 首见 fp 的对话不可能有签发历史，其回放的 blob 必为外来（砖会话的毒串
// 正是这么来的）；池内 lineage 原样不动。
// STRIP_FOREIGN_BLOB=0 可关，默认开。
func stripForeignBlobs(in map[string]any, isNewFP bool) {
	if v := os.Getenv("STRIP_FOREIGN_BLOB"); v == "0" || strings.EqualFold(v, "false") {
		return
	}
	if !isNewFP {
		return
	}
	items, _ := in["input"].([]any)
	if len(items) == 0 {
		return
	}
	kept := make([]any, 0, len(items))
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		// 内嵌形态：message.content[] 里的 reasoning part 同样剥离
		// （某客户端把 thinking 放在 content 数组里，顶层扫描会漏）。
		if m["type"] != "reasoning" {
			if content, ok := m["content"].([]any); ok {
				stripped := stripReasoningParts(content)
				if len(stripped) != len(content) {
					m["content"] = stripped
				}
			}
			kept = append(kept, raw)
			continue
		}
		if keep, cleaned := cleanReasoningItem(m); keep {
			kept = append(kept, cleaned)
		}
	}
	in["input"] = kept
}

// stripReasoningParts 清洗 content 数组里的内嵌 reasoning part，返回保留项。
// 外来 blob/id 去绑定留文本，无文本整条丢（与顶层同规则）。
func stripReasoningParts(parts []any) []any {
	kept := make([]any, 0, len(parts))
	for _, raw := range parts {
		m, ok := raw.(map[string]any)
		if !ok || (m["type"] != "reasoning" && m["type"] != "thinking") {
			kept = append(kept, raw)
			continue
		}
		if keep, cleaned := cleanReasoningItem(m); keep {
			kept = append(kept, cleaned)
		}
	}
	return kept
}

// summaryHasText 任一形态有可读文本即 true。
func summaryHasText(sum any) bool {
	switch s := sum.(type) {
	case []any:
		for _, e := range s {
			if sm, ok := e.(map[string]any); ok {
				if t, ok := sm["text"].(string); ok && strings.TrimSpace(t) != "" {
					return true
				}
			} else if t, ok := e.(string); ok && strings.TrimSpace(t) != "" {
				return true
			}
		}
	case string:
		return strings.TrimSpace(s) != ""
	case map[string]any:
		if t, ok := s["text"].(string); ok && strings.TrimSpace(t) != "" {
			return true
		}
	}
	return false
}

// cleanReasoningItem 单条 reasoning 去外来绑定：
// 外来纯 id（无 blob）去 id；无 blob 无 id 原样留；有 blob 看文本。
// 有文本去 blob+id 留文本，无文本整条丢。返回 (保留?, 条目)。
func cleanReasoningItem(m map[string]any) (bool, map[string]any) {
	blob, _ := m["encrypted_content"].(string)
	id, _ := m["id"].(string)
	// 外来纯 id（无 blob）同样不可信：上游必报 not found/expired。
	// 有可读文本则去 id 留文本，无文本整条丢。
	if blob == "" && id != "" {
		delete(m, "id")
	}
	if blob == "" && id == "" {
		// 无绑定也无文本的 thinking 块：下游无用，丢弃省 tokens。
		if summaryHasText(m["summary"]) {
			return true, m
		}
		return false, nil
	}
	// summary 形态：数组元素 .text；字符串/对象直接取文本。
	if !summaryHasText(m["summary"]) {
		return false, nil // 纯 blob 无文本：整条丢弃
	}
	delete(m, "encrypted_content")
	delete(m, "id")
	return true, m
}

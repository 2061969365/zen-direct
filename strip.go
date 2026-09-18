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
		if !ok || m["type"] != "reasoning" {
			kept = append(kept, raw)
			continue
		}
		blob, _ := m["encrypted_content"].(string)
		if blob == "" {
			kept = append(kept, raw)
			continue
		}
		sum, _ := m["summary"].([]any)
		texts := 0
		for _, s := range sum {
			if sm, ok := s.(map[string]any); ok {
				if t, ok := sm["text"].(string); ok && strings.TrimSpace(t) != "" {
					texts++
				}
			}
		}
		if texts == 0 {
			continue // 纯 blob 无文本：整条丢弃
		}
		delete(m, "encrypted_content")
		delete(m, "id")
		kept = append(kept, m)
	}
	in["input"] = kept
}

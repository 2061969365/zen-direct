package main

import (
	"strings"
	"sync"
)

// badids.go: caller-mismatch 实锤毒 id 的进程内负缓存。
// 池命中时 strip 被跳过（保 lineage），同一毒串每轮都要走“400→自愈”交一次税；
// 自愈成功即把当轮 id 记入本表，下轮命中路径先行剥离（只动记名 id，其余原样）。
// key 轮换后旧 id 自然失效——表只为省“每轮一次 400”，不为正确性（自愈仍在兜底）。
var badMu sync.Mutex
var badIDs = map[string]struct{}{}
var badQ []string

const maxBadIDs = 1000

func normBadID(id string) string { return strings.TrimPrefix(id, "nested:") }

func learnBadIDs(ids []string) {
	badMu.Lock()
	defer badMu.Unlock()
	for _, raw := range ids {
		id := normBadID(strings.TrimSpace(raw))
		if id == "" || id == "(no-id)" || id == "nested:(no-id)" {
			continue
		}
		if _, ok := badIDs[id]; ok {
			continue
		}
		badIDs[id] = struct{}{}
		badQ = append(badQ, id)
		if len(badQ) > maxBadIDs {
			old := badQ[0]
			badQ = badQ[1:]
			delete(badIDs, old)
		}
	}
}

func isBadID(id string) bool {
	id = normBadID(id)
	badMu.Lock()
	defer badMu.Unlock()
	_, ok := badIDs[id]
	return ok
}

// stripKnownBadIDs 剥离记名毒 id（顶层 reasoning + message.content[] 内嵌），返回剥离数。
// 语义“留文本去串”：删 encrypted_content/id，summary 文本保留；
// 无文本也无串的判不出归属，不动（等 400 自愈兜底）。
func stripKnownBadIDs(in map[string]any) int {
	n := 0
	items, _ := in["input"].([]any)
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "reasoning" {
			if id, _ := m["id"].(string); id != "" && isBadID(id) {
				delete(m, "encrypted_content")
				delete(m, "id")
				n++
			}
			continue
		}
		if content, ok := m["content"].([]any); ok {
			for _, p := range content {
				pm, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if pm["type"] == "reasoning" || pm["type"] == "thinking" {
					if id, _ := pm["id"].(string); id != "" && isBadID(id) {
						delete(pm, "encrypted_content")
						delete(pm, "id")
						n++
					}
				}
			}
		}
	}
	return n
}

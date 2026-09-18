package main

import (
	"testing"
)

// 首见 fp + 外来 blob → 剥 blob 留 summary（不断链）。
func TestStripForeignBlob(t *testing.T) {
	in := map[string]any{"input": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"type": "reasoning", "id": "rs_foreign",
			"encrypted_content": "blobblob",
			"summary":           []any{map[string]any{"type": "summary_text", "text": "thought"}}},
	}}
	stripForeignBlobs(in, true)
	items, _ := in["input"].([]any)
	if len(items) != 2 {
		t.Fatalf("项数错误：%d", len(items))
	}
	rs, _ := items[1].(map[string]any)
	if _, ok := rs["encrypted_content"]; ok {
		t.Fatal("外来 blob 未剥离")
	}
	if _, ok := rs["id"]; ok {
		t.Fatal("外来 id 未剥离")
	}
	sum, _ := rs["summary"].([]any)
	if len(sum) != 1 {
		t.Fatal("summary 文本应保留")
	}
}

// 非首见（池内 lineage）→ 原样不动。
func TestKeepOwnBlob(t *testing.T) {
	in := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "id": "rs_own",
			"encrypted_content": "blobblob",
			"summary":           []any{map[string]any{"type": "summary_text", "text": "t"}}},
	}}
	stripForeignBlobs(in, false)
	rs, _ := in["input"].([]any)[0].(map[string]any)
	if rs["encrypted_content"] != "blobblob" || rs["id"] != "rs_own" {
		t.Fatal("自家 blob 不应动")
	}
}

// 无 summary 的纯 blob 项 → 整条丢弃（防上游空 reasoning 400）。
func TestDropEmptyBlobItem(t *testing.T) {
	in := map[string]any{"input": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"type": "reasoning", "id": "rs_x", "encrypted_content": "b"},
	}}
	stripForeignBlobs(in, true)
	items, _ := in["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("纯 blob 项应整条丢弃，得 %d 项", len(items))
	}
}

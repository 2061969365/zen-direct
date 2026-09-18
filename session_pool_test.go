package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 同一下游对话命中复用上游 session。
func TestPoolHitReuse(t *testing.T) {
	resetPoolForTest()
	fp := fpKey("", "hello", "sys")
	ses := poolLookup(fp)
	if ses != "" {
		t.Fatalf("空池应失配，得 %q", ses)
	}
	got := poolBorrow(fp)
	if got == "" {
		t.Fatal("失配应新签")
	}
	if again := poolLookup(fp); again != got {
		t.Fatalf("应复用 %q，得 %q", got, again)
	}
}

// X-Session-Id 头优先于回退 hash。
func TestPoolHeaderKey(t *testing.T) {
	a := fpKey("hdr-A", "hello", "sys")
	b := fpKey("", "hello", "sys")
	if a == b {
		t.Fatal("有头无头应异键")
	}
	if a != fpKey("hdr-A", "world", "other") {
		t.Fatal("同头应同键")
	}
}

// LRU 上限逐出最久未用。
func TestPoolLRUEvict(t *testing.T) {
	resetPoolForTest()
	oldCap := poolCap
	poolCap = 3
	defer func() { poolCap = oldCap }()
	poolBorrow("fp-1")
	poolBorrow("fp-2")
	poolBorrow("fp-3")
	poolLookup("fp-1") // touch fp-1
	poolBorrow("fp-4") // 逐出 fp-2
	if poolLookup("fp-2") != "" {
		t.Fatal("fp-2 应被逐出")
	}
	if poolLookup("fp-1") == "" || poolLookup("fp-3") == "" || poolLookup("fp-4") == "" {
		t.Fatal("热键不应被逐出")
	}
}

// TTL 过期失效。
func TestPoolTTLExpire(t *testing.T) {
	resetPoolForTest()
	oldTTL := poolTTL
	poolTTL = -1 // 立刻过期
	defer func() { poolTTL = oldTTL }()
	ses := poolBorrow("fp-ttl")
	if poolLookup("fp-ttl") != "" {
		t.Fatalf("过期应失效，得 %q", ses)
	}
}

// 落盘 reload：重启不断链。
func TestPoolPersistReload(t *testing.T) {
	dir := t.TempDir()
	oldFile := poolFile
	poolFile = filepath.Join(dir, "ses.json")
	defer func() { poolFile = oldFile }()
	resetPoolForTest()
	ses := poolBorrow("fp-disk")
	poolSave()
	resetPoolForTest()
	if _, err := os.Stat(poolFile); err != nil {
		t.Fatalf("落盘文件缺失：%v", err)
	}
	poolLoad()
	if got := poolLookup("fp-disk"); got != ses {
		t.Fatalf("reload 应恢复 %q，得 %q", ses, got)
	}
}

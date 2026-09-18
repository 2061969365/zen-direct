package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// 会话池：同一下游对话复用稳定上游 session，使 blob 回放的签发 caller
// 与回放 caller 一致。无状态直连的最大短板由它补上。
// 键：X-Session-Id 头优先；无头回退首条 user 文本 + instructions 全文 hash。
// 落盘：SHIM_SESSION_FILE（缺省 ./shim_sessions.json），重启不断链。

var (
	poolMu    sync.Mutex
	poolMap   = map[string]*poolEntry{}
	poolQ     []string
	poolCap   = 256
	poolTTL   = 24 * time.Hour
	poolFile  = ""
	poolDirty = false
)

type poolEntry struct {
	Session  string    `json:"ses"`
	LastSeen time.Time `json:"seen"`
}

func poolFilePath() string {
	if poolFile != "" {
		return poolFile
	}
	if v := os.Getenv("SHIM_SESSION_FILE"); v != "" {
		return v
	}
	return "./shim_sessions.json"
}

// fpKey 会话键。sid 非空即 hdr: 前缀（同头同键，与 body 无关）。
func fpKey(sid, first, instructions string) string {
	if sid != "" {
		return "hdr:" + sid
	}
	sum := sha256.Sum256([]byte(first + "\x00" + instructions))
	return "fp:" + hex.EncodeToString(sum[:])
}

// poolLookup 命中返回上游 session（并 touch），失配/过期返回 ""。
func poolLookup(fp string) string {
	if fp == "" {
		return ""
	}
	poolMu.Lock()
	defer poolMu.Unlock()
	e, ok := poolMap[fp]
	if !ok {
		return ""
	}
	if time.Since(e.LastSeen) > poolTTL {
		delete(poolMap, fp)
		for i, k := range poolQ {
			if k == fp {
				poolQ = append(poolQ[:i], poolQ[i+1:]...)
				break
			}
		}
		poolDirty = true
		return ""
	}
	e.LastSeen = time.Now()
	for i, k := range poolQ {
		if k == fp {
			poolQ = append(poolQ[:i], poolQ[i+1:]...)
			break
		}
	}
	poolQ = append(poolQ, fp)
	return e.Session
}

// poolBorrow 查池；失配则 mint 新签并入库返回。
func poolBorrow(fp string) string {
	if s := poolLookup(fp); s != "" {
		return s
	}
	ses := mintID("ses")
	poolMu.Lock()
	if e, ok := poolMap[fp]; ok {
		e.Session = ses
		e.LastSeen = time.Now()
	} else {
		poolMap[fp] = &poolEntry{Session: ses, LastSeen: time.Now()}
		poolQ = append(poolQ, fp)
		if len(poolQ) > poolCap {
			delete(poolMap, poolQ[0])
			poolQ = poolQ[1:]
		}
	}
	poolDirty = true
	poolMu.Unlock()
	poolSaveAsync()
	return ses
}

// poolEvict 驱逐映射（上游 401/403 死 session 后调用）。
func poolEvict(fp string) {
	if fp == "" {
		return
	}
	poolMu.Lock()
	delete(poolMap, fp)
	for i, k := range poolQ {
		if k == fp {
			poolQ = append(poolQ[:i], poolQ[i+1:]...)
			break
		}
	}
	poolDirty = true
	poolMu.Unlock()
	poolSaveAsync()
}

var poolSaveMu sync.Mutex
var poolSaveTimer *time.Timer

// poolSaveAsync 置脏 + debounce 1s 刷盘，避免每次 Borrow 同步写。
func poolSaveAsync() {
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()
	if poolSaveTimer != nil {
		poolSaveTimer.Stop()
	}
	poolSaveTimer = time.AfterFunc(time.Second, poolSave)
}

// poolSave 快照写临时文件 + rename（崩溃不丢旧文件）。
func poolSave() {
	poolMu.Lock()
	if !poolDirty {
		poolMu.Unlock()
		return
	}
	snap := make(map[string]*poolEntry, len(poolMap))
	for k, e := range poolMap {
		cp := *e
		snap[k] = &cp
	}
	poolDirty = false
	poolMu.Unlock()

	path := poolFilePath()
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// poolLoad 启动加载（过滤过期、截断保 MRU）。
func poolLoad() {
	path := poolFilePath()
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snap map[string]*poolEntry
	if err := json.Unmarshal(b, &snap); err != nil {
		return
	}
	poolMu.Lock()
	defer poolMu.Unlock()
	type kv struct {
		k string
		e *poolEntry
	}
	var all []kv
	for k, e := range snap {
		if e == nil || e.Session == "" {
			continue
		}
		if poolTTL < 0 || time.Since(e.LastSeen) > poolTTL {
			continue
		}
		all = append(all, kv{k, e})
	}
	// 按 LastSeen 升序，老的先挤掉。
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].e.LastSeen.Before(all[i].e.LastSeen) {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	if len(all) > poolCap {
		all = all[len(all)-poolCap:]
	}
	poolMap = map[string]*poolEntry{}
	poolQ = nil
	for _, kv := range all {
		poolMap[kv.k] = kv.e
		poolQ = append(poolQ, kv.k)
	}
}

func resetPoolForTest() {
	poolMu.Lock()
	defer poolMu.Unlock()
	poolMap = map[string]*poolEntry{}
	poolQ = nil
	poolDirty = false
}

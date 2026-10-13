package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	// genuineSessions 白名单真 session 备份（本地 opencode.db 收割）。
	// 注：现签已修正为 descending（低 12 位大计数），本身即过门禁；
	// 名单仅作无 service/无 ACP 环境的兜底。
	genuineSessions []string
	genuineSet       = map[string]bool{}
	// badSes 401/403 下烧掉的 session（内存态，重启清空；上限防 403 风暴无界涨）。
	badSes = map[string]bool{}
)

// badSesCap 烧号表上限：超了整表清零（误烧的 transient 迟早恢复，白名单重学）。
const badSesCap = 2048

// markBadSesLocked 记烧号。调用方须持 poolMu。
func markBadSesLocked(s string) {
	if s == "" {
		return
	}
	if len(badSes) >= badSesCap {
		badSes = map[string]bool{}
		log.Printf("[pool] bad-session table full, reset")
	}
	badSes[s] = true
}

type poolEntry struct {
	Session  string    `json:"ses"`
	LastSeen time.Time `json:"seen"`
	// Gen 真 session（service/ACP/harvest 名单）vs 现签假 session。
	// 重启过滤只认 Gen 或名单成员；老快照缺该键按 false（名单兜底）。
	Gen bool `json:"gen,omitempty"`
}

func poolFilePath() string {
	if poolFile != "" {
		return poolFile
	}
	if v := os.Getenv("SHIM_SESSION_FILE"); v != "" {
		return v
	}
	// 缺省放 Temp（不随 CWD 漂移，容器只读盘失败也有日志可查）。
	return filepath.Join(os.TempDir(), "shim_sessions.json")
}

// genuineFilePath 真 session 名单路径：GENUINE_SESSION_FILE 优先，
// 缺省与会话池同目录 genuine_sessions.jsonl（harvest_genuine.py 生成）。
func genuineFilePath() string {
	if v := os.Getenv("GENUINE_SESSION_FILE"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(poolFilePath()), "genuine_sessions.txt")
}

// genuineLoad 读真 session 名单（无锁：仅启动 poolLoad 内单线程调，单测单线程调）。
func genuineLoad() {
	genuineSessions = nil
	genuineSet = map[string]bool{}
	path := genuineFilePath()
	if b, err := os.ReadFile(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[pool] genuine load %s: %v", path, err)
		}
	} else {
		for _, ln := range strings.Split(string(b), "\n") {
			s := strings.TrimSpace(ln)
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			if !strings.HasPrefix(s, "ses_") || len(s) < 10 {
				continue
			}
			if !genuineSet[s] {
				genuineSet[s] = true
				genuineSessions = append(genuineSessions, s)
			}
			if len(genuineSessions) >= 1024 {
				break
			}
		}
		if len(genuineSessions) > 0 {
			log.Printf("[pool] loaded %d genuine sessions from %s", len(genuineSessions), path)
		}
	}
	// GENUINE_SESSIONS_INLINE：换行分隔的真 session（容器/远端无 DB 时用，
	// 如 Railway 变量粘贴 harvest 结果）。文件优先，变量追加。
	if extra := os.Getenv("GENUINE_SESSIONS_INLINE"); extra != "" {
		n0 := len(genuineSessions)
		for _, ln := range strings.Split(extra, "\n") {
			s := strings.TrimSpace(ln)
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			if !strings.HasPrefix(s, "ses_") || len(s) < 10 {
				continue
			}
			if !genuineSet[s] {
				genuineSet[s] = true
				genuineSessions = append(genuineSessions, s)
			}
			if len(genuineSessions) >= 1024 {
				break
			}
		}
		if len(genuineSessions) > n0 {
			log.Printf("[pool] loaded %d genuine sessions from env", len(genuineSessions)-n0)
		}
	}
}

// genuineHash fp 定点散列（同 fp 跨重启落到同一真 session，保 lineage 稳定）。
func genuineHash(fp string) uint64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(fp))
	return uint64(h.Sum32())
}

// pickGenuineLocked 按 fp 定点选真 session，跳过烧号；全烧/无货返回 ""。
// 调用方须持 poolMu。
func pickGenuineLocked(fp string, salt uint64) string {
	n := len(genuineSessions)
	if n == 0 {
		return ""
	}
	start := (genuineHash(fp) + salt) % uint64(n)
	for i := 0; i < n; i++ {
		s := genuineSessions[(int(start)+i)%n]
		if !badSes[s] {
			return s
		}
	}
	return ""
}

// pickGenuine 加锁版（轮换外/借用外调用）。
func pickGenuine(fp string, salt uint64) string {
	poolMu.Lock()
	defer poolMu.Unlock()
	return pickGenuineLocked(fp, salt)
}
// fpKey 会话键。sid 非空即 hdr: 前缀（同头同键，与 body 无关）。
func fpKey(sid, first, instructions string) string {
	if sid != "" {
		return "hdr:" + sid
	}
	sum := sha256.Sum256([]byte(first + "\x00" + instructions))
	return "fp:" + hex.EncodeToString(sum[:])
}

// poolLookup 命中返回上游 session（并 touch），失配/过期/烧号返回 ""。
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
	if badSes[e.Session] {
		// 别家 fp 轮换时烧掉的共享 session：本 fp 视同失配，下次借用重取。
		// 不 touch 不复用，免每家各交一次 403 学费。
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
	ses, _ := poolBorrowEx(fp)
	return ses
}

// poolBorrowEx 同 poolBorrow，多返回 isNew（首见=true，给剥 blob 用）。
// 失配先 service 现铸（一 fp 一真 session，毫秒级），再退 harvest 名单，
// 再退 ACP 现铸（无 service 的机器），最后本地 descending 现签（已验证过门禁）。
func poolBorrowEx(fp string) (string, bool) {
	if s := poolLookup(fp); s != "" {
		return s, false
	}
	ses, gen := "", false
	if s := svcMintFunc(); s != "" {
		ses, gen = s, true
	} else if s := pickGenuine(fp, 0); s != "" {
		ses, gen = s, true
	} else if s := acpMintFunc(); s != "" {
		ses, gen = s, true
	} else {
		ses = mintID("ses")
	}
	poolMu.Lock()
	if e, ok := poolMap[fp]; ok {
		e.Session = ses
		e.Gen = gen
		e.LastSeen = time.Now()
	} else {
		poolMap[fp] = &poolEntry{Session: ses, LastSeen: time.Now(), Gen: gen}
		poolQ = append(poolQ, fp)
		if len(poolQ) > poolCap {
			delete(poolMap, poolQ[0])
			poolQ = poolQ[1:]
		}
	}
	poolDirty = true
	poolMu.Unlock()
	poolSaveAsync()
	return ses, true
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

// poolRotate 轮换：旧 session 记烧号，新 session 入库。
// 注意非严格原子：慢铸造（service/ACP 上下游）时 unlock 出临界区，并发
// borrow/rotate 可交错；delete+insert 仍在同一临界区内完成，last-writer-wins
// 自洽（旧注释的“原子”指此段，不含铸造等待）。
// 顺序 service → harvest → ACP → 现签；烧号表有上限（见 markBadSes）。
func poolRotate(fp string) (string, bool) {
	if fp == "" {
		if s := svcMintFunc(); s != "" {
			return s, true
		}
		if s := pickGenuine("", 0); s != "" {
			return s, true
		}
		if s := acpMintFunc(); s != "" {
			return s, true
		}
		return mintID("ses"), true
	}
	poolMu.Lock()
	if e, ok := poolMap[fp]; ok && e.Session != "" {
		markBadSesLocked(e.Session)
	}
	poolMu.Unlock()
	ses, gen := "", false
	if s := svcMintFunc(); s != "" {
		ses, gen = s, true
	} else if s := pickGenuine(fp, 0); s != "" {
		ses, gen = s, true
	} else if s := acpMintFunc(); s != "" {
		ses, gen = s, true
	} else {
		ses = mintID("ses")
	}
	poolMu.Lock()
	delete(poolMap, fp)
	poolMap[fp] = &poolEntry{Session: ses, LastSeen: time.Now(), Gen: gen}
	for i, k := range poolQ {
		if k == fp {
			poolQ = append(poolQ[:i], poolQ[i+1:]...)
			break
		}
	}
	poolQ = append(poolQ, fp)
	if len(poolQ) > poolCap {
		delete(poolMap, poolQ[0])
		poolQ = poolQ[1:]
	}
	poolDirty = true
	poolMu.Unlock()
	poolSaveAsync()
	return ses, true
}

var poolSaveMu sync.Mutex
var poolSaveTimer *time.Timer

// poolSaveSeq 单调序号：快照携带 seq，落盘只许新覆旧（AfterFunc 回调并发时
// 旧快照 IO 慢于新快照也不得覆写回去）。
var poolSaveSeq uint64
var poolSavedSeq uint64

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
// 全程持 poolSaveMu：并发回调串行化，旧快照不得覆写新文件。
func poolSave() {
	poolSaveMu.Lock()
	defer poolSaveMu.Unlock()
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
	poolSaveSeq++
	seq := poolSaveSeq
	poolDirty = false
	poolMu.Unlock()

	if seq <= poolSavedSeq {
		return
	}
	path := poolFilePath()
	b, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[pool] save marshal: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		log.Printf("[pool] save write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[pool] save rename: %v", err)
		return
	}
	poolSavedSeq = seq
}

// poolLoad 启动加载（过滤过期、截断保 MRU）。
// 先读真 session 名单；有真货时顺手丢弃快照里的现签假 session（必 403），
// 对应 fp 下次请求重借真 session。
func poolLoad() {
	genuineLoad()
	path := poolFilePath()
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[pool] load %s: %v", path, err)
		}
		return
	}
	var snap map[string]*poolEntry
	if err := json.Unmarshal(b, &snap); err != nil {
		log.Printf("[pool] load parse %s: %v", path, err)
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
	if len(genuineSessions) > 0 {
		dropped := 0
		for k, e := range poolMap {
			// 名单成员或 Gen 标记（运行期 service/ACP 现铸）保留；其余现签假
			// session 必 403，丢弃后对应 fp 下次重借。老快照无 Gen 键按 false，
			// 名单兜底。poolDirty 由下次借用落盘（崩溃窗口内下次启动再滤）。
			if e == nil || (!e.Gen && !genuineSet[e.Session]) {
				delete(poolMap, k)
				dropped++
			}
		}
		if dropped > 0 {
			kept := poolQ[:0]
			for _, k := range poolQ {
				if _, ok := poolMap[k]; ok {
					kept = append(kept, k)
				}
			}
			poolQ = kept
			poolDirty = true
			log.Printf("[pool] dropped %d non-genuine sessions, genuine=%d", dropped, len(genuineSessions))
		}
	}
	if len(all) > 0 {
		log.Printf("[pool] loaded %d sessions from %s", len(all), path)
	}
}

func resetPoolForTest() {
	poolMu.Lock()
	defer poolMu.Unlock()
	poolMap = map[string]*poolEntry{}
	poolQ = nil
	poolDirty = false
	poolSaveSeq = 0
	poolSavedSeq = 0
	badSes = map[string]bool{}
	genuineSessions = nil
	genuineSet = map[string]bool{}
	// 单测永不拉真子进程/真服务：无 acp、无 service 也能跑，CI 也不慢。
	acpMintFunc = func() string { return "" }
	svcMintFunc = func() string { return "" }
	svcInvalidate()
}

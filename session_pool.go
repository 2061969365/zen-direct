package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// badSes 401/403/免费额度烧掉的 session（落盘名单，重启由 badSesLoad 恢复；
	// 上限防 403 风暴无界涨）。
	badSes = map[string]bool{}
	// badSesPrefix 前缀命中表：名单里可能抄进被日志截断的短 id（如 ses_ + 16 位），
	// 精确查表永远放不中，等于把已打满的 session 又原样发回去——2026-10-23 事故：
	// 6 条截断条目全失效，干净出口 IP 一接就被连坐。故短条目按前缀匹配，宁严勿漏。
	badSesPrefix []string
)

// sesIDLen 现网上游 session id 定长（ses_ 4 位 + 26 位）。短于此的名单条目
// 只可能是日志截断的残条（网关 429 日志曾用 shortStr(ses,20)）。
const sesIDLen = 30

// badSesCap 烧号表上限：超了整表清零（误烧的 transient 迟早恢复，白名单重学）。
const badSesCap = 2048

// badSesFilePath 烧号名单路径：DIRECT_POISON_FILE 优先，缺省与会话池同目录。
// 免费额度按上游 session 计且用完即永久 429（到窗口重置），重启后若从
// genuine 名单里再捡回同一 id，等于把毒 session 重新请回来，故必须落盘。
// badSesFile 单测覆盖用（resetPoolForTest 指到临时文件，不碰生产名单）。
var badSesFile = ""

// poisonResetAt 这批毒号失效的时刻（下一次窗口重置）。到点整表清空、文件删除：
// 打满的 session 是“今天用完”，不是永久报废。
var poisonResetAt time.Time

// poisonExpireLocked 过点即清空毒名单。调用方须持 poolMu。
func poisonExpireLocked(now time.Time) {
	if poisonResetAt.IsZero() {
		poisonResetAt = nextFreeReset(now)
		return
	}
	if now.Before(poisonResetAt) {
		return
	}
	n := len(badSes)
	if n > 0 {
		badSesArchiveLocked(poisonResetAt)
	}
	badSes = map[string]bool{}
	badSesPrefix = nil
	poisonResetAt = nextFreeReset(now)
	if n > 0 {
		log.Printf("[pool] free window reset, dropped %d poisoned sessions (archived)", n)
	}
	if err := os.Remove(badSesFilePath()); err != nil && !os.IsNotExist(err) {
		log.Printf("[pool] poison reset %s: %v", badSesFilePath(), err)
	}
}

// badSesArchiveLocked 归档本窗口的毒号：窗口重置时追加进 poison_archive.txt，
// 带窗口结束时间。日志只留 7 天，归档文件常驻——「每天记了哪些毒号」可查，
// 也给 turns 观测做对照。调用方须持 poolMu。
func badSesArchiveLocked(ended time.Time) {
	ids := make([]string, 0, len(badSes))
	for id := range badSes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	path := filepath.Join(filepath.Dir(badSesFilePath()), "poison_archive.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[pool] poison archive mkdir: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("[pool] poison archive %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "# window ended %s (%d ids)\n",
		ended.In(freeResetZone).Format("2006-01-02 15:04"), len(ids)); err != nil {
		log.Printf("[pool] poison archive write %s: %v", path, err)
		return
	}
	for _, id := range ids {
		if _, err := fmt.Fprintln(f, id); err != nil {
			log.Printf("[pool] poison archive write %s: %v", path, err)
			return
		}
	}
}

func badSesFilePath() string {
	if badSesFile != "" {
		return badSesFile
	}
	if v := os.Getenv("DIRECT_POISON_FILE"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(poolFilePath()), "poisoned_sessions.txt")
}

// badSesLoad 启动时读烧号名单（poolLoad 内先于快照加载调用）。
// 名单只覆盖「当前窗口」（上次 07:59 之后）：文件太旧整份丢弃，随后再把当天
// 日志里 free-usage 429 的 id 收回来——重启不丢今天的证据。
func badSesLoad() {
	path := badSesFilePath()
	now := time.Now()
	stale := false
	if st, err := os.Stat(path); err == nil && st.ModTime().Before(lastFreeReset(now)) {
		log.Printf("[pool] poison list %s predates the last reset, dropping", path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[pool] poison drop %s: %v", path, err)
		}
		stale = true
	}
	poolMu.Lock()
	defer poolMu.Unlock()
	poisonResetAt = nextFreeReset(now)
	n := 0
	if !stale {
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[pool] poison load %s: %v", path, err)
			}
		} else {
			for _, ln := range strings.Split(string(b), "\n") {
				s := strings.TrimSpace(ln)
				if s == "" || strings.HasPrefix(s, "#") {
					continue
				}
				if len(s) < 10 {
					log.Printf("[pool] poison entry %q too short, ignored", s)
					continue
				}
				if badSesAddLocked(s) {
					n++
				}
				if len(s) < sesIDLen {
					// 残条：精确匹配废掉，只能靠前缀兜。出现这条日志就说明有抄错的名单，
					// 该换成完整 id（网关 429 日志已改打全长 id）。
					log.Printf("[pool] poison entry %s truncated (%d<%d), prefix-match enabled", s, len(s), sesIDLen)
				}
			}
		}
	}
	if n > 0 {
		log.Printf("[pool] loaded %d poisoned sessions from %s", n, path)
	}
	if m := badSesHarvestTodayLogLocked(); m > 0 {
		log.Printf("[pool] harvested %d poisoned sessions from today's log", m)
	}
}

// logDateRe 日志文件名里的日期段（launcher 命名 zen-direct-20261023.err.log）。
var logDateRe = regexp.MustCompile(`\d{8}`)

// todayLogPath 当天日志路径（基名；轮转片见 harvest）。未设 LOG_FILE 返回 ""。
func todayLogPath() string {
	lp := env("LOG_FILE", "")
	if lp == "" {
		return ""
	}
	base := filepath.Base(lp)
	if logDateRe.MatchString(base) {
		base = logDateRe.ReplaceAllString(base, time.Now().In(freeResetZone).Format("20060102"))
	}
	return filepath.Join(filepath.Dir(lp), base)
}

// badSesHarvestTodayLogLocked 从今天的网关日志回收 free-usage 429 的 session id。
// 进程被杀、名单被手工改过、内存表被清空后，重启也能把「今天已经打满」的证据
// 捡回来；只读今天这一个文件（含 .1-.3 轮转片），昨天的不碰。调用方须持 poolMu。
func badSesHarvestTodayLogLocked() int {
	path := todayLogPath()
	if path == "" {
		return 0
	}
	n := 0
	for _, p := range []string{path, path + ".1", path + ".2", path + ".3"} {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			id := freeLimitSesFromLine(sc.Text())
			if id == "" {
				continue
			}
			if badSesAddLocked(id) {
				n++
			}
		}
		f.Close()
	}
	return n
}

// freeLimitSesFromLine 从 "… free-usage 429 ses=<id>, poisoned+rotate …" 取 id。
// 老日志里这一段的 id 可能只有 20 位，交给前缀匹配兜底。
func freeLimitSesFromLine(line string) string {
	const marker = "free-usage 429 ses="
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	if j := strings.IndexAny(rest, ", \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "ses_") || len(rest) < 10 {
		return ""
	}
	return rest
}


// badSesAddLocked 记一条毒名单（精确表 + 前缀表）。已在表内返回 false。
// 残条（日志里被截断的短 id）若已被某条全长 id 覆盖，就不再重复记。
// 调用方须持 poolMu。
func badSesAddLocked(s string) bool {
	if s == "" || badSes[s] {
		return false
	}
	if len(s) < sesIDLen {
		for full := range badSes {
			if len(full) >= sesIDLen && strings.HasPrefix(full, s) {
				return false
			}
		}
	}
	badSes[s] = true
	badSesPrefix = append(badSesPrefix, s)
	return true
}

// isBadSesLocked 精确命中，或以名单里的残条为前缀，即判毒。调用方须持 poolMu。
// 前缀比对只对 len(p) < len(s) 生效：全长 id 互不为前缀，故不误伤。
// 顺带做窗口重置检查：到点整表作废，毒号第二天自动解禁。
func isBadSesLocked(s string) bool {
	poisonExpireLocked(time.Now())
	if s == "" {
		return false
	}
	if badSes[s] {
		return true
	}
	for _, p := range badSesPrefix {
		if len(p) < len(s) && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// badSesAppendLocked 追加一行到烧号名单。调用方须持 poolMu。
// 上限触顶时整表清零并重写文件：误烧的 transient 迟早恢复，名单不能无限涨。
func badSesAppendLocked(s string) {
	path := badSesFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[pool] poison mkdir: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("[pool] poison append %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(s + "\n"); err != nil {
		log.Printf("[pool] poison write %s: %v", path, err)
	}
}

// markBadSesLocked 记烧号（内存 + 落盘）。调用方须持 poolMu。
func markBadSesLocked(s string) {
	if s == "" {
		return
	}
	poisonExpireLocked(time.Now())
	if badSes[s] {
		return
	}
	if len(badSes) >= badSesCap {
		badSes = map[string]bool{}
		badSesPrefix = nil
		log.Printf("[pool] bad-session table full, reset")
		if err := os.Remove(badSesFilePath()); err != nil && !os.IsNotExist(err) {
			log.Printf("[pool] poison reset %s: %v", badSesFilePath(), err)
		}
	}
	badSesAddLocked(s)
	badSesAppendLocked(s)
}

type poolEntry struct {
	Session  string    `json:"ses"`
	LastSeen time.Time `json:"seen"`
	// Gen 真 session（service/ACP/harvest 名单）vs 现签假 session。
	// 重启过滤只认 Gen 或名单成员；老快照缺该键按 false（名单兜底）。
	Gen bool `json:"gen,omitempty"`
	// Turns 这条上游 session 已经成功服务了几轮。纯观测：429 时打进日志，
	// 攒一天就能量出"一条号能吃多少轮"，再决定要不要提前换号。
	Turns int `json:"turns,omitempty"`
}

// poolNoteTurn 记一次成功轮次（给 429 日志里的 turns=N 用）。
func poolNoteTurn(fp string) {
	if fp == "" {
		return
	}
	poolMu.Lock()
	if e, ok := poolMap[fp]; ok {
		e.Turns++
		poolDirty = true
	}
	poolMu.Unlock()
}

// poolTurnsLocked 当前 fp 名下那条号已经服务的轮数。调用方须持 poolMu。
func poolTurnsLocked(fp string) int {
	if fp == "" {
		return 0
	}
	if e, ok := poolMap[fp]; ok {
		return e.Turns
	}
	return 0
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
		if !isBadSesLocked(s) {
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
	if isBadSesLocked(e.Session) {
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

// freshPerTurn 每轮换号，默认开：一个对话的每一轮都用一条本轮没用过的上游
// session。DIRECT_FRESH_PER_TURN=0 退回“一对话一号”。
//
// 依据（实测，见 zen-remote-probe/FINDINGS.md）：额度按上游 session 计，一条
// session 打满就一直 429 到窗口重置；而碰一次打满的 session 会让出口 IP 连坐
// ≥45 分钟。于是“对话钉住一条号”最致命的场景是长对话聊到第 N 轮撞板——不但
// 这一轮失败，后面 45 分钟整个网关都 429。改成每轮换新号后，发出去的号最多
// 只承担该对话的一轮，撞板概率≈0；老对话也不会因为存着一条死号而一开口就炸。
//
// 代价：每轮都是新 lineage，responses 表单里的 reasoning 串一律剥离（本来
// 首见也要剥），session 级 prompt cache 可能少一些。
func freshPerTurn() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DIRECT_FRESH_PER_TURN"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// rotateSeq 全局换号游标：每轮 +1，让连续请求落在池里不同的号上；同一对话
// 的相邻两轮也不会撞同一个号（除非池子正好转了一圈）。
var rotateSeq atomic.Uint64

// poolNextSes 自愿换新号：与 poolRotate 的区别是**不把旧号记毒**——旧号是好的，
// 只是这一轮不再用它。顺序同 borrow：service 现铸 → 收割名单 → ACP → 本地现签。
func poolNextSes(fp string) (string, bool) {
	if fp == "" {
		return poolBorrowEx(fp)
	}
	ses, gen := "", false
	if s := svcMintFunc(); s != "" {
		ses, gen = s, true
	} else if s := pickGenuine(fp, rotateSeq.Add(1)); s != "" {
		ses, gen = s, true
	} else if s := acpMintFunc(); s != "" {
		ses, gen = s, true
	} else {
		ses = mintID("ses")
	}
	poolMu.Lock()
	if old, ok := poolMap[fp]; ok {
		old.Session = ses
		old.Gen = gen
		old.LastSeen = time.Now()
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
	badSesLoad()
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
	skipped := 0
	for _, kv := range all {
		if isBadSesLocked(kv.e.Session) {
			// 额度已打尽的 session 不能随快照复活：再用一次就把出口 IP 拖下水。
			// 残条名前缀命中也走这里，重启后旧快照里的毒号同样拦得住。
			skipped++
			continue
		}
		poolMap[kv.k] = kv.e
		poolQ = append(poolQ, kv.k)
	}
	if skipped > 0 {
		log.Printf("[pool] skipped %d poisoned sessions in %s", skipped, path)
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
		log.Printf("[pool] loaded %d sessions from %s", len(poolMap), path)
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
	badSesPrefix = nil
	poisonResetAt = nextFreeReset(time.Now())
	freeLimitUntil.Store(0)
	freeLimitDur.Store(0)
	freeLimitIP.Store("")
	freeLimitIPCheck.Store(0)
	// 单测的烧号名单写到独立临时文件，绝不碰生产的 poisoned_sessions.txt。
	badSesFile = filepath.Join(os.TempDir(), "zen-direct-test-poisoned.txt")
	_ = os.Remove(badSesFile)
	genuineSessions = nil
	genuineSet = map[string]bool{}
	// 单测永不拉真子进程/真服务：无 acp、无 service 也能跑，CI 也不慢。
	acpMintFunc = func() string { return "" }
	svcMintFunc = func() string { return "" }
	svcInvalidate()
}

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

// 真 session 优先：有货时借用不再现签，同 fp 稳定命中同一真 session。
func TestGenuinePickStable(t *testing.T) {
	resetPoolForTest()
	genuineSessions = []string{"ses_genuine_aaa", "ses_genuine_bbb", "ses_genuine_ccc"}
	genuineSet = map[string]bool{"ses_genuine_aaa": true, "ses_genuine_bbb": true, "ses_genuine_ccc": true}
	defer resetPoolForTest()
	got, isNew := poolBorrowEx("fp-g")
	if !isNew || !genuineSet[got] {
		t.Fatalf("有货应借真 session，得 %q", got)
	}
	again, isNew := poolBorrowEx("fp-g")
	if isNew || again != got {
		t.Fatalf("同 fp 应稳定复用 %q，得 %q", got, again)
	}
}

// 轮换跳过烧号：403 的旧 session 不再被选中；烧光才回退现签。
func TestGenuineRotateSkipsBad(t *testing.T) {
	resetPoolForTest()
	genuineSessions = []string{"ses_genuine_aaa", "ses_genuine_bbb"}
	genuineSet = map[string]bool{"ses_genuine_aaa": true, "ses_genuine_bbb": true}
	defer resetPoolForTest()
	first := poolBorrow("fp-r")
	second, _ := poolRotate("fp-r")
	if second == first || !genuineSet[second] {
		t.Fatalf("轮换应切到另一真 session，得 %q（旧 %q）", second, first)
	}
	third, _ := poolRotate("fp-r")
	if genuineSet[third] {
		t.Fatalf("烧光后应回退现签，得 %q", third)
	}
	if poolLookup("fp-r") != third {
		t.Fatalf("轮换结果应入库，得 %q", poolLookup("fp-r"))
	}
}

// 名单文件加载：非法行跳过，去重，上限内全收。
func TestGenuineLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "genuine_sessions.txt")
	body := "ses_genuine_aaa\n\n# comment\nbadline\nses_genuine_aaa\nses_genuine_bbb\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GENUINE_SESSION_FILE", p)
	resetPoolForTest()
	genuineLoad()
	if len(genuineSessions) != 2 || !genuineSet["ses_genuine_aaa"] || !genuineSet["ses_genuine_bbb"] {
		t.Fatalf("应加载 2 个去重真 session，得 %v", genuineSessions)
	}
	resetPoolForTest()
}
// 借用顺序：service → harvest → ACP → 现签；每层为空才往下一层掉。
func TestPoolMintChainOrder(t *testing.T) {
	resetPoolForTest()
	genuineSessions = []string{"ses_genuine_aaa"}
	genuineSet = map[string]bool{"ses_genuine_aaa": true}
	svcMintFunc = func() string { return "ses_svc_first" }
	acpMintFunc = func() string { t.Error("service 有货时不应调 ACP"); return "" }
	defer resetPoolForTest()
	if got := poolBorrow("fp-chain"); got != "ses_svc_first" {
		t.Fatalf("service 应优先，得 %q", got)
	}
	svcMintFunc = func() string { return "" }
	acpMintFunc = func() string { return "ses_acp_last" }
	if got, _ := poolRotate("fp-chain"); got != "ses_genuine_aaa" {
		t.Fatalf("service 无货应退 harvest，得 %q", got)
	}
	genuineSessions = nil
	genuineSet = map[string]bool{}
	if got, _ := poolRotate("fp-chain"); got != "ses_acp_last" {
		t.Fatalf("harvest 烧光应退 ACP，得 %q", got)
	}
}

// 烧号表有上限：403 风暴不无界涨内存，超了整表清零（误烧的迟早恢复）。
func TestBadSesCapReset(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	for i := 0; i < badSesCap+10; i++ {
		markBadSesLocked("ses_burn_" + itoa(i))
	}
	if len(badSes) > 20 {
		t.Fatalf("超上限应整表清零，得 %d 条", len(badSes))
	}
}

// 命中烧号 session 视同失配：不复用不 touch，摘除后下次重借。
func TestPoolLookupSkipsBad(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	ses := poolBorrow("fp-bad")
	markBadSesLocked(ses)
	if got := poolLookup("fp-bad"); got != "" {
		t.Fatalf("烧号命中应视同失配，得 %q", got)
	}
	if _, ok := poolMap["fp-bad"]; ok {
		t.Fatal("烧号条目应摘除")
	}
}

// Gen 落盘跨重启：运行期现铸真 session 不在 harvest 名单里也必须留下。
func TestGenSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	oldFile := poolFile
	poolFile = filepath.Join(dir, "ses.json")
	defer func() { poolFile = oldFile }()
	resetPoolForTest()
	svcMintFunc = func() string { return "ses_svc_keep001" }
	ses := poolBorrow("fp-keep")
	if ses != "ses_svc_keep001" {
		t.Fatalf("应借到 svc session，得 %q", ses)
	}
	poolSave()
	resetPoolForTest()
	if err := os.WriteFile(filepath.Join(dir, "genuine_sessions.txt"), []byte("ses_other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	poolLoad()
	if got := poolLookup("fp-keep"); got != ses {
		t.Fatalf("Gen 应跨重启保留，得 %q", got)
	}
	resetPoolForTest()
}

// 名单环境变量加载：无文件时也能收（容器/Railway 变量粘贴）。
func TestGenuineLoadFromEnv(t *testing.T) {
	resetPoolForTest()
	t.Setenv("GENUINE_SESSION_FILE", filepath.Join(t.TempDir(), "none.txt"))
	t.Setenv("GENUINE_SESSIONS_INLINE", "ses_env_aaa\nses_env_bbb\nbadline\n")
	defer resetPoolForTest()
	genuineLoad()
	if len(genuineSessions) != 2 || !genuineSet["ses_env_aaa"] || !genuineSet["ses_env_bbb"] {
		t.Fatalf("应从 env 加载 2 个，得 %v", genuineSessions)
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

// 每轮换号：同一对话连续两轮必须落到不同上游 session，且换下来的旧号
// 不得记毒（旧号是好的，只是这一轮不再用）。
func TestFreshPerTurnRotates(t *testing.T) {
	resetPoolForTest()
	defer resetPoolForTest()
	t.Setenv("DIRECT_FRESH_PER_TURN", "1")
	genuineSessions = []string{
		"ses_aaaa0001ffeaaaaaaaaaaaaaa1",
		"ses_aaaa0001ffeaaaaaaaaaaaaaa2",
		"ses_aaaa0001ffeaaaaaaaaaaaaaa3",
		"ses_aaaa0001ffeaaaaaaaaaaaaaa4",
	}
	genuineSet = map[string]bool{}
	for _, s := range genuineSessions {
		genuineSet[s] = true
	}
	first, isNew := poolNextSes("fp:rotate")
	if !isNew || first == "" {
		t.Fatalf("换号应报首见且有值，得 %q new=%v", first, isNew)
	}
	second, _ := poolNextSes("fp:rotate")
	if second == "" || second == first {
		t.Fatalf("相邻两轮应用不同 session：%q -> %q", first, second)
	}
	if got := poolLookup("fp:rotate"); got != second {
		t.Fatalf("映射应更新到 %q，得 %q", second, got)
	}
	poolMu.Lock()
	n, poisoned := len(badSes), badSes[first] || badSes[second]
	poolMu.Unlock()
	if poisoned || n != 0 {
		t.Fatalf("自愿换号不得记毒：%d 条，first/second 命中=%v", n, poisoned)
	}
}

// DIRECT_FRESH_PER_TURN 解析：默认开，显式 0/false/off 才关。
func TestFreshPerTurnSwitch(t *testing.T) {
	for _, v := range []string{"0", "false", "off", "NO"} {
		t.Setenv("DIRECT_FRESH_PER_TURN", v)
		if freshPerTurn() {
			t.Fatalf("%q 应关闭每轮换号", v)
		}
	}
	for _, v := range []string{"1", "true", "", "yes"} {
		t.Setenv("DIRECT_FRESH_PER_TURN", v)
		if !freshPerTurn() {
			t.Fatalf("%q 应开启每轮换号", v)
		}
	}
}

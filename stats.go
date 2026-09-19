package main

import (
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

// DIRECT_DIAG 总开关（缺省开；设为 0 则诊断静默）：
// 关 = 不打 [STATS] 行、不打 WARN 大包体行、不打 debug 转储行；
// 计数器与 ttfb/waitMs 字段保留（原子加法零成本，格式稳定，/health 照常）。
// 与 v0.2.3 DIRECT_DEBUG 并存：debug 转储需两者同时开。
var diagEnabled = os.Getenv("DIRECT_DIAG") != "0"

func diagOn() bool { return diagEnabled }

// stats.go: 被动观测——网关自身就是探针，不打 synthetic 流量。
// 每请求几次原子加法 + 首字节釆样（环形 cap 256）；每 5 分钟一行 [STATS]，
// /health 同口径暴露累计值。测试直接调 record/snapshot，无需等 ticker。
type statsKeeper struct {
	mu             sync.Mutex
	reqs           uint64
	transportRetry uint64
	flakyRetry     uint64 // 502/503 退避重试
	up5xx          uint64
	up4xx          uint64
	circuitOpens   uint64
	ttfb           []int64
}

var stats = &statsKeeper{}

func statsAddReq() {
	stats.mu.Lock()
	stats.reqs++
	stats.mu.Unlock()
}

func statsAddTransportRetry() {
	stats.mu.Lock()
	stats.transportRetry++
	stats.mu.Unlock()
}

func statsAddFlakyRetry() {
	stats.mu.Lock()
	stats.flakyRetry++
	stats.mu.Unlock()
}

func statsAddUpstream(status int) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if status >= 500 {
		stats.up5xx++
	} else if status >= 400 {
		stats.up4xx++
	}
}

func statsAddCircuitOpen() {
	stats.mu.Lock()
	stats.circuitOpens++
	stats.mu.Unlock()
}

func statsAddTTFB(ms int64) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.ttfb = append(stats.ttfb, ms)
	if len(stats.ttfb) > 256 {
		stats.ttfb = stats.ttfb[len(stats.ttfb)-256:]
	}
}

type statsSnap struct {
	Reqs, TransportRetry, FlakyRetry uint64
	Up5xx, Up4xx, CircuitOpens        uint64
	TTFBP50, TTFBP95                  int64
}

func statsSnapshot() statsSnap {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	s := statsSnap{
		Reqs: stats.reqs, TransportRetry: stats.transportRetry,
		FlakyRetry: stats.flakyRetry, Up5xx: stats.up5xx,
		Up4xx: stats.up4xx, CircuitOpens: stats.circuitOpens,
	}
	if len(stats.ttfb) > 0 {
		cp := append([]int64{}, stats.ttfb...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		s.TTFBP50 = cp[len(cp)/2]
		s.TTFBP95 = cp[len(cp)*95/100]
	}
	return s
}

func statsReset() {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// 逐字段清零：整体赋值会覆盖 mutex 本体导致 unlock-of-unlocked。
	stats.reqs, stats.transportRetry, stats.flakyRetry = 0, 0, 0
	stats.up5xx, stats.up4xx, stats.circuitOpens = 0, 0, 0
	stats.ttfb = nil
}

// statsReporter 每 5 分钟一行 [STATS]（main() 内起，单测不碰）。
func statsReporter(d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for range t.C {
		s := statsSnapshot()
		log.Printf("[STATS] window=5m reqs=%d transport_retry=%d flaky_retry=%d up5xx=%d up4xx=%d circuit_open=%d ttfb_p50=%dms p95=%dms",
			s.Reqs, s.TransportRetry, s.FlakyRetry, s.Up5xx, s.Up4xx, s.CircuitOpens, s.TTFBP50, s.TTFBP95)
	}
}

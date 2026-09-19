package main

import (
	"sync"
	"time"
)

// circuit.go: 上游 5xx 熔断器（cc-switch circuit_breaker 思想：
// Closed/Open/HalfOpen，阈值开、窗口后半开探测）。
//
// 只计上游 5xx：传输错是本地/代理问题、4xx 是请求问题（caller-mismatch
// 另有自愈），都不触发熔断；任意 200 清零。
var (
	cbMu        sync.Mutex
	cbFails     int
	cbOpenUntil time.Time
	cbProbing   bool
	// 包级变量：单测可覆盖短窗口验证，生产用缺省。
	cbThreshold  = 5
	cbOpenWindow = 60 * time.Second
)

// cbReset 测试/启动复位。
func cbReset() {
	cbMu.Lock()
	defer cbMu.Unlock()
	cbFails, cbProbing = 0, false
	cbOpenUntil = time.Time{}
}

// cbAllow 熔断开则 false。窗口过期放且只放一个半开探测。
func cbAllow() bool {
	cbMu.Lock()
	defer cbMu.Unlock()
	now := time.Now()
	if now.Before(cbOpenUntil) {
		return false
	}
	if cbProbing {
		return false
	}
	if !cbOpenUntil.IsZero() {
		// 熔断过 + 窗口已过 = 半开：只放一个。
		cbProbing = true
	}
	return true
}

// cbRecord 记录一次上游终态：5xx 累计（探测失败重开），200 关闭+清零。
func cbRecord(status int) {
	cbMu.Lock()
	defer cbMu.Unlock()
	if status >= 500 {
		cbFails++
		if cbFails >= cbThreshold {
			cbOpenUntil = time.Now().Add(cbOpenWindow)
			cbProbing = false
		}
		return
	}
	if status == 200 {
		cbFails, cbProbing = 0, false
		cbOpenUntil = time.Time{}
	}
}

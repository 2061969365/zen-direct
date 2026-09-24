package main

import "testing"

// 未注入时版本为 dev；build.ps1/CI 通过 ldflags 覆盖。
func TestAppVersionDefault(t *testing.T) {
	if appVersion == "" {
		t.Fatal("appVersion must not be empty")
	}
}

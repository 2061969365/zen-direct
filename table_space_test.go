
package main

import "testing"

func TestSpaceBunnyRoutesChat(t *testing.T) {
	seedBuiltinTable()
	f, ok := lookupFormat("space-bunny-free")
	if !ok || f != FmtChat {
		t.Fatalf("space-bunny-free 应 chat，得 %q,%v", f, ok)
	}
	// 前缀兜底也应 chat（防表被清后仍对）。
	if f := guessFormatByPrefix("space-bunny-free"); f != FmtChat {
		t.Fatalf("space- 前缀应 chat，得 %q", f)
	}
	if f, ok := lookupFormat("mimo-v2.6-flash-free"); !ok || f != FmtChat {
		t.Fatalf("mimo-v2.6 应 chat，得 %q,%v", f, ok)
	}
}

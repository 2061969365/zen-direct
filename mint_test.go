package main

import (
	"strconv"
	"strings"
	"testing"
)

// 现签 descending：低 12 位须为 0xFFF-ctr（上游 bot 过滤只认大计数）。
// 固定 ts 下首签 ctr=1 → 低 12 位恒为 ffe。
func TestMintDescendingLowBits(t *testing.T) {
	const ts = int64(1791872614000)
	a := mintIDAt("ses", ts)
	b := mintIDAt("ses", ts)
	for _, id := range []string{a, b} {
		if len(id) != 30 || !strings.HasPrefix(id, "ses_") {
			t.Fatalf("形状应 ses_+26，得 %q", id)
		}
		for _, c := range id[4:] {
			if !strings.ContainsRune(mintHex+mintChars, c) {
				t.Fatalf("非法字符 %q in %q", c, id)
			}
		}
	}
	lo := func(id string) uint64 {
		v, err := strconv.ParseUint(id[4:16], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v & 0xfff
	}
	if got := lo(a); got != 0xffe {
		t.Fatalf("首签低 12 位应 ffe，得 %x (%s)", got, a)
	}
	if got := lo(b); got != 0xffd {
		t.Fatalf("次签低 12 位应 ffd，得 %x (%s)", got, b)
	}
	if a == b {
		t.Fatal("随机尾应不同")
	}
}

package main

import (
	"os"
	"strings"
	"testing"
)

// TestAcpMintLive spawns the real `opencode acp` and mints one session.
// Env-gated (ACP_LIVE=1): CI skips, local dev proves the child path works.
// The minted session is genuine but unused (no zen traffic in this test).
func TestAcpMintLive(t *testing.T) {
	if os.Getenv("ACP_LIVE") == "" {
		t.Skip("set ACP_LIVE=1 to spawn real opencode acp")
	}
	sid := acpMintSession()
	if sid == "" {
		t.Fatal("live mint failed (see [acp] log line above)")
	}
	if !strings.HasPrefix(sid, "ses_") {
		t.Fatalf("bad session id %q", sid)
	}
	t.Logf("minted %s", sid)
}

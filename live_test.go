package releasebot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in check of the installed adapter and real credentials. It uses a
// disposable directory and a read-only prompt, never a release job.
func TestLiveACP(t *testing.T) {
	if os.Getenv("RELEASE_BOT_LIVE_SMOKE") != "1" {
		t.Skip("set RELEASE_BOT_LIVE_SMOKE=1 to exercise the real ACP agent")
	}
	cfg := DefaultConfig()
	cfg.Directory, cfg.StateDirectory = t.TempDir(), t.TempDir()
	cfg.Agent.Model = os.Getenv("RELEASE_BOT_LIVE_MODEL")
	cfg.Agent.Effort = os.Getenv("RELEASE_BOT_LIVE_EFFORT")
	if err := ResolveAgent(&cfg, true); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cfg.Directory, "README.md"), "ACP integration fixture: release-bot-smoke-ok\n")
	a := agentProcess{config: cfg, log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, err := a.Execute(ctx, `This is a read-only ACP integration test, not a release job. Read README.md with a tool. Do not edit files, create commits, access GitHub, publish anything, or perform any other work. Return one line: RELEASE_RESULT {"status":"ready","detail":"THE_MARKER_FROM_README"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ready" || !strings.Contains(r.Detail, "release-bot-smoke-ok") {
		t.Fatalf("agent did not complete the read-only tool round trip: %+v", r)
	}
}

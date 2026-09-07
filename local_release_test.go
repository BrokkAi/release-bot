package releasebot

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalUnreleasedCommitsReachPreparationAndReleaseAfterSquashMerge(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	base := f.head
	s := &State{Format: 1, Remote: f.engine.config.Remote, Branch: "master", Directory: f.engine.config.Directory, Released: base, ReleasedAt: f.now}
	if err := writeState(f.engine.config, s); err != nil {
		t.Fatal(err)
	}
	dir := f.engine.config.Directory
	writeTestFile(t, filepath.Join(dir, "fix"), "unreleased repair")
	localGit(t, dir, "add", "fix")
	localGit(t, dir, "commit", "-m", "repair left after release")
	localHead := localGit(t, dir, "rev-parse", "HEAD")
	calls := 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		calls++
		if strings.HasPrefix(prompt, "# Publishability") {
			if fixtureState(t, f).Job.Target != base {
				t.Fatal("local hash used as ancestry requirement would prevent squash merging")
			}
			if localGit(t, dir, "rev-parse", "HEAD") != localHead {
				t.Fatal("local work was lost before preparation")
			}
			// The agent owns the PR. Simulate its topic-branch push and the
			// server's squash merge, which replaces the local commit hash.
			localGit(t, dir, "switch", "-c", "release-preparation")
			localGit(t, dir, "push", "origin", "release-preparation")
			localGit(t, f.source, "fetch", "origin", "release-preparation")
			localGit(t, f.source, "merge", "--squash", "FETCH_HEAD")
			localGit(t, f.source, "commit", "-m", "merge preparation PR")
			localGit(t, f.source, "push", "origin", "master")
			localGit(t, dir, "fetch", "origin")
			localGit(t, dir, "switch", "-C", "master", "origin/master")
			f.head = localGit(t, dir, "rev-parse", "HEAD")
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		return f.published(t, true), nil
	})
	if err := f.engine.cycle(ctx, false); err != nil || calls != 0 {
		t.Fatalf("local commits bypassed cadence: calls=%d err=%v", calls, err)
	}
	if fixtureState(t, f).Observed != localHead {
		t.Fatal("local unreleased commits were invisible")
	}
	f.now = f.now.Add(25 * time.Hour)
	if err := f.engine.cycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = fixtureState(t, f)
	if calls != 2 || s.Job != nil || s.Released != f.head || s.Released == localHead || s.Released == base {
		t.Fatalf("merged work was not released: calls=%d state=%+v", calls, s)
	}
	if got := localGit(t, f.source, "show", "master:fix"); got != "unreleased repair" {
		t.Fatalf("released branch lost local repair: %q", got)
	}
}

func TestUnmergedPreparationCannotStartPublication(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		calls++
		if calls > 1 {
			t.Fatal("publisher started before preparation reached remote branch")
		}
		dir := f.engine.config.Directory
		localGit(t, dir, "switch", "-c", "open-pr")
		localGit(t, dir, "commit", "--allow-empty", "-m", "unmerged preparation")
		localGit(t, dir, "push", "origin", "open-pr")
		f.head = localGit(t, dir, "rev-parse", "HEAD")
		return Result{Status: "ready", Plan: f.plan()}, nil
	})
	err := f.engine.cycle(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "finish merging") {
		t.Fatalf("expected unmerged preparation to block: %v", err)
	}
	if calls != 1 || fixtureState(t, f).Released == f.head {
		t.Fatal("unmerged work was treated as released")
	}
}

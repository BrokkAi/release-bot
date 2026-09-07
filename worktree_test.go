package releasebot

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateWorktreeLeavesOtherBotsAlone(t *testing.T) {
	source, _ := discoveryRepo(t)
	other := filepath.Join(t.TempDir(), "other-bot")
	localGit(t, source, "worktree", "add", "-b", "another-bot", other, "main")
	writeTestFile(t, filepath.Join(source, "README.md"), "staged user change")
	localGit(t, source, "add", "README.md")
	writeTestFile(t, filepath.Join(source, "README.md"), "unstaged user change")
	writeTestFile(t, filepath.Join(other, "README.md"), "another bot's unfinished change")
	worktrees := localGit(t, source, "worktree", "list", "--porcelain")
	refs := localGit(t, source, "show-ref")
	index := localGit(t, source, "diff", "--cached")
	status := localGit(t, source, "status", "--porcelain")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg, err := Discover(context.Background(), other, "")
	if err != nil {
		t.Fatal(err)
	}
	g := checkout{cfg}
	ctx := context.Background()
	if err := g.open(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(cfg.Directory, ".git"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("expected linked worktree gitfile: %v %v", info, err)
	}
	if common := localGit(t, cfg.Directory, "rev-parse", "--path-format=absolute", "--git-common-dir"); common != g.repositoryDirectory() {
		t.Fatalf("worktree does not own its Git storage: %s", common)
	}
	first, err := g.startBranch(ctx)
	if err != nil || !strings.HasPrefix(first, "brb/release-") {
		t.Fatalf("missing unique preparation branch: %s %v", first, err)
	}
	writeTestFile(t, filepath.Join(cfg.Directory, "README.md"), "bot preparation")
	localGit(t, cfg.Directory, "add", "README.md")
	localGit(t, cfg.Directory, "commit", "-m", "prepare release")
	localGit(t, cfg.Directory, "tag", "local-bot-tag")
	if err := g.open(ctx); err != nil {
		t.Fatal(err)
	}
	if localGit(t, cfg.Directory, "branch", "--show-current") != first {
		t.Fatal("reopening changed the preparation branch")
	}
	if localGit(t, source, "worktree", "list", "--porcelain") != worktrees || localGit(t, source, "show-ref") != refs {
		t.Fatal("bot changed another repository's worktrees, branches or tags")
	}
	if localGit(t, source, "diff", "--cached") != index || localGit(t, source, "status", "--porcelain") != status {
		t.Fatal("bot changed another checkout's index or files")
	}
	data, _ := os.ReadFile(filepath.Join(other, "README.md"))
	if string(data) != "another bot's unfinished change" {
		t.Fatal("bot changed another worktree's unfinished edits")
	}
	second := cfg
	second.Directory, second.StateDirectory = filepath.Join(t.TempDir(), "checkout"), t.TempDir()
	g2 := checkout{second}
	if err := g2.open(ctx); err != nil {
		t.Fatal(err)
	}
	branch, err := g2.startBranch(ctx)
	if err != nil || branch == first {
		t.Fatalf("independent bot workspaces reused a branch: %s %v", branch, err)
	}
}

func TestExistingCloneAndExternalWorktreeOwnership(t *testing.T) {
	f := newFixture(t)
	g := f.engine.git
	localGit(t, t.TempDir(), "clone", f.engine.config.Remote, g.config.Directory)
	localGit(t, g.config.Directory, "switch", "-c", "existing-preparation")
	writeTestFile(t, filepath.Join(g.config.Directory, "manifest"), "unfinished legacy work")
	if err := g.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if localGit(t, g.config.Directory, "branch", "--show-current") != "existing-preparation" || localGit(t, g.config.Directory, "status", "--porcelain") == "" {
		t.Fatal("opening an old clone changed its work")
	}
	external := filepath.Join(t.TempDir(), "shared")
	localGit(t, f.source, "worktree", "add", "-b", "shared-bot", external)
	g.config.Directory = external
	if err := g.open(context.Background()); err == nil || !strings.Contains(err.Error(), "shares Git metadata") {
		t.Fatalf("accepted a worktree belonging to another repository: %v", err)
	}
}

func TestDivergentPreparationMergesConcurrentWorkAndStartsNextJobSeparately(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	dir, base := f.engine.config.Directory, f.head
	s := &State{Format: 1, Remote: f.engine.config.Remote, Branch: "master", Directory: dir, Released: base, ReleasedAt: f.now.Add(-25 * time.Hour)}
	if err := writeState(f.engine.config, s); err != nil {
		t.Fatal(err)
	}
	localGit(t, dir, "switch", "-c", "unfinished-local-fix")
	writeTestFile(t, filepath.Join(dir, "manifest"), "release bot's change")
	localGit(t, dir, "commit", "-am", "local preparation")
	local := localGit(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(f.source, "manifest"), "another bot's change")
	localGit(t, f.source, "commit", "-am", "concurrent remote change")
	localGit(t, f.source, "push", "origin", "master")
	remote := localGit(t, f.source, "rev-parse", "HEAD")
	if err := f.engine.git.fetch(ctx); err != nil {
		t.Fatal(err)
	}
	total, _, err := f.engine.git.changes(ctx, base, local, time.Time{})
	if err != nil || total != 2 {
		t.Fatalf("divergent local and remote commits were not both counted: %d %v", total, err)
	}
	var branch string
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			job := fixtureState(t, f).Job
			branch = job.WorkBranch
			if !strings.HasPrefix(branch, "brb/release-") || job.Target != remote || localGit(t, dir, "branch", "--show-current") != branch {
				t.Fatalf("job did not start on its own branch with the remote baseline: %+v", job)
			}
			if !strings.Contains(prompt, branch) || !strings.Contains(prompt, `"workspace":`) {
				t.Fatal("preparation did not receive its workspace and branch")
			}
			if localGit(t, dir, "rev-parse", "HEAD") != local {
				t.Fatal("divergence handling discarded local work")
			}
			// The agent handles a real merge conflict in its own worktree.
			merge := exec.Command("git", "merge", "--no-commit", "origin/master")
			merge.Dir = dir
			merge.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
			if output, err := merge.CombinedOutput(); err == nil || !strings.Contains(string(output), "CONFLICT") {
				t.Fatalf("expected real merge conflict: %s %v", output, err)
			}
			writeTestFile(t, filepath.Join(dir, "manifest"), "both bots' changes reconciled")
			localGit(t, dir, "add", "manifest")
			localGit(t, dir, "commit", "-m", "resolve preparation conflict")
			localGit(t, dir, "push", "origin", branch)
			// Simulate the server merging this PR, then detach at that exact commit.
			localGit(t, f.source, "fetch", "origin", branch)
			localGit(t, f.source, "merge", "--ff-only", "FETCH_HEAD")
			localGit(t, f.source, "push", "origin", "master")
			localGit(t, dir, "fetch", "origin")
			localGit(t, dir, "switch", "--detach", "origin/master")
			f.head = localGit(t, dir, "rev-parse", "HEAD")
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		localGit(t, f.source, "commit", "--allow-empty", "-m", "another bot moves master during publication")
		localGit(t, f.source, "push", "origin", "master")
		return f.published(t, true), nil
	})
	if err := f.engine.cycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	if fixtureState(t, f).Released != f.head || localGit(t, dir, "rev-parse", "unfinished-local-fix") != local {
		t.Fatal("release lost the exact commit or rewrote previous local history")
	}
	f.now = f.now.Add(25 * time.Hour)
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		job := fixtureState(t, f).Job
		if job.WorkBranch == branch || !strings.HasPrefix(job.WorkBranch, "brb/release-") {
			t.Fatal("next release reused the prior PR's branch")
		}
		if localGit(t, dir, "rev-parse", "HEAD") != localGit(t, f.source, "rev-parse", "HEAD") {
			t.Fatal("next release did not pull in concurrent remote changes")
		}
		return Result{}, errors.New("fixture stops before next publication")
	})
	if err := f.engine.cycle(ctx, false); err == nil || !strings.Contains(err.Error(), "fixture stops") {
		t.Fatalf("next release did not reach preparation: %v", err)
	}
}

func TestRestartPreservesPendingBranchAndDirtyWork(t *testing.T) {
	f := newFixture(t)
	var branch string
	calls := 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		calls++
		job := fixtureState(t, f).Job
		if calls == 1 {
			branch = job.WorkBranch
			writeTestFile(t, filepath.Join(f.engine.config.Directory, "manifest"), "unfinished PR fix")
		} else {
			if job.WorkBranch != branch || localGit(t, f.engine.config.Directory, "branch", "--show-current") != branch {
				t.Fatal("restart changed the pending PR branch")
			}
			data, _ := os.ReadFile(filepath.Join(f.engine.config.Directory, "manifest"))
			if string(data) != "unfinished PR fix" {
				t.Fatal("restart discarded pending edits")
			}
		}
		return Result{}, errors.New("fixture pending PR")
	})
	if err := f.engine.cycle(context.Background(), false); err == nil {
		t.Fatal("fixture should leave a pending job")
	}
	localGit(t, f.source, "commit", "--allow-empty", "-m", "remote moves while bot stopped")
	localGit(t, f.source, "push", "origin", "master")
	f.engine.starting = true
	if err := f.engine.cycle(context.Background(), false); err == nil || calls != 2 {
		t.Fatalf("pending work did not resume: calls=%d err=%v", calls, err)
	}
}

func TestRemoteChangesResetQuietPeriodOnDivergentTopicBranch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	s := &State{Format: 1, Remote: f.engine.config.Remote, Branch: "master", Directory: f.engine.config.Directory, Released: f.head, ReleasedAt: f.now}
	if err := writeState(f.engine.config, s); err != nil {
		t.Fatal(err)
	}
	localGit(t, f.engine.config.Directory, "commit", "--allow-empty", "-m", "local work")
	local := localGit(t, f.engine.config.Directory, "rev-parse", "HEAD")
	f.engine.agent = scriptedAgent(func(context.Context, string) (Result, error) {
		t.Fatal("cadence should not start a release yet")
		return Result{}, nil
	})
	if err := f.engine.cycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Minute)
	localGit(t, f.source, "commit", "--allow-empty", "-m", "remote work")
	localGit(t, f.source, "push", "origin", "master")
	if err := f.engine.cycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = fixtureState(t, f)
	if s.Observed != local || s.ObservedRemote != localGit(t, f.source, "rev-parse", "HEAD") || !s.ChangedAt.Equal(f.now) {
		t.Fatalf("concurrent remote update was invisible: %+v", s)
	}
}

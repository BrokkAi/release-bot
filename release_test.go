package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

type scriptedAgent func(context.Context, string) (Result, error)

func (s scriptedAgent) Execute(ctx context.Context, p string) (Result, error) { return s(ctx, p) }

type fixture struct {
	engine                             *engine
	source, registry, permission, head string
	now                                time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	source := filepath.Join(dir, "developer")
	localGit(t, dir, "init", "--bare", remote)
	localGit(t, dir, "init", "-b", "master", source)
	writeTestFile(t, filepath.Join(source, "manifest"), "fixture artifact")
	localGit(t, source, "add", "manifest")
	localGit(t, source, "commit", "-m", "baseline")
	base := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "commit", "--allow-empty", "-m", "feature")
	head := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "remote", "add", "origin", remote)
	localGit(t, source, "push", "origin", "master")
	cfg := DefaultConfig()
	cfg.Remote = remote
	cfg.Directory = filepath.Join(dir, "bot")
	cfg.StateDirectory = filepath.Join(dir, "state")
	cfg.InitialRef = base
	cfg.Verify = []string{"git", "cat-file", "-e", head}
	if err := os.MkdirAll(cfg.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{source: source, registry: filepath.Join(dir, "published"), permission: filepath.Join(dir, "permit"), head: head, now: time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)}
	writeTestFile(t, f.permission, "publishing identity authorized")
	f.engine = &engine{config: cfg, git: checkout{cfg}, github: github{config: cfg}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.now }}
	return f
}
func (f *fixture) plan() *PublicationPlan {
	return &PublicationPlan{Commit: f.head, Tag: "v1.0.0", Destinations: []Destination{{Name: "fixture-registry/package", Kind: "test registry", Version: "1.0.0", Environment: "fixture publisher", Checks: []PublishabilityCheck{
		{Kind: "build", Command: []string{"test", "-f", "manifest"}, Evidence: "artifact input exists"},
		{Kind: "authorization", Command: []string{"test", "-f", f.permission}, Evidence: "fixture authorization endpoint permits publication"},
		{Kind: "version", Command: []string{"git", "check-ref-format", "refs/tags/v1.0.0"}, Evidence: "fixture version validation"},
	}, Verify: []string{"test", "-f", f.registry}}}}
}
func (f *fixture) published(t *testing.T, complete bool) Result {
	t.Helper()
	cfg := f.engine.config
	if localGit(t, cfg.Directory, "tag", "--list", "v1.0.0") == "" {
		localGit(t, cfg.Directory, "tag", "v1.0.0", f.head)
		localGit(t, cfg.Directory, "push", "origin", "v1.0.0")
	}
	if complete {
		writeTestFile(t, f.registry, "all fixture artifacts uploaded")
	}
	return Result{Status: "released", Commit: f.head, Tag: "v1.0.0"}
}
func fixtureState(t *testing.T, f *fixture) *State {
	t.Helper()
	state, err := ReadState(f.engine.config)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPublishabilityBlocksPublicationAndCanBeRepaired(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(f.permission); err != nil {
		t.Fatal(err)
	}
	preparations, publications := 0, 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, p string) (Result, error) {
		if strings.HasPrefix(p, "# Publishability") {
			preparations++
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		publications++
		return f.published(t, true), nil
	})
	if err := f.engine.cycle(context.Background(), false); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("expected denied preflight: %v", err)
	}
	if preparations != 1 || publications != 0 {
		t.Fatal("publication started before authorization passed")
	}
	if localGit(t, f.source, "ls-remote", "--tags", "origin") != "" {
		t.Fatal("preflight failure left a public tag")
	}
	state := fixtureState(t, f)
	if state.Released == f.head || state.Job.Phase != "validating" {
		t.Fatal("failed gate advanced state")
	}
	writeTestFile(t, f.permission, "permissions repaired")
	f.now = f.now.Add(time.Hour)
	if err := f.engine.cycle(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if publications != 1 || fixtureState(t, f).Job != nil {
		t.Fatal("release failed to resume after repaired permission")
	}
}
func TestPartialPublicationRemainsPendingAndReconciles(t *testing.T) {
	f := newFixture(t)
	publicationCalls := 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, p string) (Result, error) {
		if strings.HasPrefix(p, "# Publishability") {
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		publicationCalls++
		return f.published(t, false), nil
	})
	if err := f.engine.cycle(context.Background(), false); err == nil {
		t.Fatal("missing registry output counted as success")
	}
	state := fixtureState(t, f)
	if state.Job.Candidate == nil || state.Released == f.head {
		t.Fatal("partial publication was not retained")
	}
	// An async upload succeeds while the daemon is stopped.
	writeTestFile(t, f.registry, "completed")
	f.now = f.now.Add(time.Hour)
	restarted := *f.engine
	if err := restarted.cycle(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if publicationCalls != 1 || fixtureState(t, f).Job != nil {
		t.Fatal("reconciliation republished")
	}
}
func TestConcurrentCommitsAreNotSwallowed(t *testing.T) {
	f := newFixture(t)
	f.engine.agent = scriptedAgent(func(ctx context.Context, p string) (Result, error) {
		if strings.HasPrefix(p, "# Publishability") {
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		localGit(t, f.source, "commit", "--allow-empty", "-m", "arrived during publication")
		localGit(t, f.source, "push", "origin", "master")
		return f.published(t, true), nil
	})
	if err := f.engine.cycle(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s := fixtureState(t, f)
	if s.Released != f.head {
		t.Fatal("recorded newer branch head instead of released commit")
	}
	head, err := f.engine.git.resolve(context.Background(), f.engine.git.branchRef())
	if err != nil {
		t.Fatal(err)
	}
	count, _, err := f.engine.git.changes(context.Background(), s.Released, head, f.now.Add(-time.Hour))
	if err != nil || count != 1 {
		t.Fatalf("lost concurrent commit: %d %v", count, err)
	}
}
func TestAttemptBudgetAndCorruptState(t *testing.T) {
	f := newFixture(t)
	f.engine.config.Attempts = 1
	calls := 0
	f.engine.agent = scriptedAgent(func(context.Context, string) (Result, error) {
		calls++
		return Result{}, errors.New("publisher credentials unavailable")
	})
	if f.engine.cycle(context.Background(), false) == nil {
		t.Fatal("expected failure")
	}
	if err := f.engine.cycle(context.Background(), false); err != nil {
		t.Fatal("backoff should skip", err)
	}
	f.now = f.now.Add(time.Hour)
	if f.engine.cycle(context.Background(), false) == nil || calls != 1 {
		t.Fatal("retry budget ignored")
	}
	if err := Retry(f.engine.config); err != nil {
		t.Fatal(err)
	}
	if f.engine.cycle(context.Background(), false) == nil || calls != 2 {
		t.Fatal("retry did not reset budget")
	}
	writeTestFile(t, filepath.Join(f.engine.config.StateDirectory, "state.json"), "broken")
	if f.engine.cycle(context.Background(), false) == nil {
		t.Fatal("corrupted publication state accepted")
	}
}
func TestStartupResumesPendingReleaseWithoutWaiting(t *testing.T) {
	f := newFixture(t)
	calls := 0
	f.engine.agent = scriptedAgent(func(context.Context, string) (Result, error) {
		calls++
		return Result{}, errors.New("fixture failure")
	})
	if err := f.engine.cycle(context.Background(), false); err == nil {
		t.Fatal("expected initial failure")
	}
	if err := f.engine.cycle(context.Background(), false); err != nil || calls != 1 {
		t.Fatal("ordinary polling must still honor failure backoff")
	}
	restarted := *f.engine
	restarted.starting = true
	if err := restarted.cycle(context.Background(), false); err == nil || calls != 2 {
		t.Fatalf("startup did not retry immediately: calls=%d err=%v", calls, err)
	}
	restarted.config.Attempts = 2
	if err := restarted.cycle(context.Background(), false); err == nil || calls != 2 {
		t.Fatal("restart bypassed the real failure budget")
	}
}

func TestRunStartsAgentDespiteSavedRetryTimer(t *testing.T) {
	f := newFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.engine.config
	cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_WIRE_PEER": "normal"}}
	s := &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Job: &Job{Target: f.head, Phase: "preflight", Tries: 1, RetryAt: time.Now().Add(time.Hour), Failure: "preflight agent: context canceled"}}
	if err := writeState(cfg, s); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The real subprocess peer reads/writes a fixture file, then deliberately
	// returns a publication receipt instead of a preparation plan.
	err = Run(ctx, cfg, f.engine.log, true, false)
	if err == nil || !strings.Contains(err.Error(), "preflight requires status ready") {
		t.Fatalf("Run did not reach the agent on startup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Directory, "wire.txt")); err != nil {
		t.Fatal("agent tool round trip did not execute:", err)
	}
}

func TestInterruptedReleaseResumesWithoutFailureBackoff(t *testing.T) {
	for _, publishing := range []bool{false, true} {
		t.Run(fmt.Sprint("publishing=", publishing), func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				if publishing && strings.HasPrefix(prompt, "# Publishability") {
					return Result{Status: "ready", Plan: f.plan()}, nil
				}
				if publishing {
					f.published(t, true)
				}
				cancel(errors.New("fixture SIGTERM"))
				return Result{}, ctx.Err()
			})
			if err := f.engine.cycle(ctx, false); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost interruption: %v", err)
			}
			s := fixtureState(t, f)
			if s.Job == nil || s.Job.Tries != 0 || !s.Job.RetryAt.IsZero() || s.Job.Failure != "" || s.Job.Interruption != "fixture SIGTERM" {
				t.Fatalf("interruption counted as a failure: %+v", s.Job)
			}
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				if strings.HasPrefix(prompt, "# Publishability") {
					return Result{Status: "ready", Plan: f.plan()}, nil
				}
				return f.published(t, true), nil
			})
			if err := f.engine.cycle(context.Background(), false); err != nil {
				t.Fatal("could not resume immediately:", err)
			}
			if fixtureState(t, f).Job != nil {
				t.Fatal("resumed job did not finish")
			}
		})
	}
}

func TestRemoteTagAndPreparedCommitGuards(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	localGit(t, f.engine.config.Directory, "tag", "v1.0.0")
	if f.engine.git.verify(ctx, f.head, Result{Commit: f.head, Tag: "v1.0.0"}) == nil {
		t.Fatal("unpublished local tag accepted")
	}
	writeTestFile(t, filepath.Join(f.engine.config.Directory, "manifest"), "uncommitted change")
	if f.engine.validatePublication(ctx, f.head, f.plan()) == nil {
		t.Fatal("dirty prepared tree accepted")
	}
	if f.engine.git.advance(ctx) == nil {
		t.Fatal("dirty work overwritten")
	}
}
func TestScheduleAndLocking(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()
	for _, tc := range []struct {
		age, quiet    time.Duration
		total, recent int
		want          bool
	}{
		{48 * time.Hour, time.Hour, 0, 0, false}, {24 * time.Hour, 0, 1, 1, true}, {time.Hour, time.Hour, 30, 30, false}, {3 * time.Hour, 20 * time.Minute, 5, 5, true}, {3 * time.Hour, 20 * time.Minute, 4, 4, false}, {3 * time.Hour, 0, 5, 5, false}, {3 * time.Hour, time.Hour, 40, 1, false},
	} {
		s := &State{ReleasedAt: now.Add(-tc.age), ChangedAt: now.Add(-tc.quiet)}
		if (due(cfg, s, tc.total, tc.recent, now) != "") != tc.want {
			t.Fatalf("wrong cadence: %+v", tc)
		}
	}
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockFile(path); err == nil {
		second()
		t.Fatal("duplicate lock acquired")
	}
	unlock()
	unlock, err = lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestStartupReleaseDeadline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		noRelease   bool
		age         time.Duration
		unchanged   bool
		restart     bool
		wantAttempt bool
	}{
		{name: "first release", noRelease: true, wantAttempt: true},
		{name: "overdue", age: 48 * time.Hour, wantAttempt: true},
		{name: "exact deadline", age: 24 * time.Hour, wantAttempt: true},
		{name: "before deadline", age: 24*time.Hour - time.Second},
		{name: "recent release", age: time.Hour},
		{name: "unchanged overdue repository", age: 48 * time.Hour, unchanged: true},
		{name: "restart at deadline", age: 23 * time.Hour, restart: true, wantAttempt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cfg := &f.engine.config
			base := cfg.InitialRef
			cfg.InitialRef = ""
			cfg.GitHub.Repo = "owner/repo"
			cfg.Burst = 0
			// Neither a long poll interval nor a quiet period may defer startup.
			cfg.Poll = Duration(7 * 24 * time.Hour)
			cfg.Quiet = Duration(7 * 24 * time.Hour)
			if tc.unchanged {
				base = f.head
			}
			var releases []publishedRelease
			published := f.now.Add(-tc.age)
			if !tc.noRelease {
				localGit(t, f.source, "tag", "v0.1.0", base)
				localGit(t, f.source, "tag", "v0.2.0", base)
				localGit(t, f.source, "push", "origin", "--tags")
				// Deliberately oldest first: cadence must use the latest publication.
				releases = []publishedRelease{{Tag: "v0.1.0", Published: published.Add(-48 * time.Hour)}, {Tag: "v0.2.0", Published: published}}
			}
			queries := 0
			f.engine.github = github{config: *cfg, request: func(ctx context.Context, path, projection string) (string, error) {
				if path != "repos/owner/repo/releases?per_page=100&page=1" {
					return "", fmt.Errorf("unexpected endpoint %s", path)
				}
				queries++
				data, err := json.Marshal(map[string]any{"count": len(releases), "releases": releases})
				return string(data), err
			}}
			attempts := 0
			stop := errors.New("fixture stops before publication")
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				attempts++
				if !strings.HasPrefix(prompt, "# Publishability") {
					t.Fatal("immediate release bypassed preparation")
				}
				return Result{}, stop
			})
			ctx := context.Background()
			if tc.restart {
				if err := f.engine.cycle(ctx, false); err != nil || attempts != 0 {
					t.Fatalf("released before deadline: attempts=%d err=%v", attempts, err)
				}
				f.now = f.now.Add(time.Hour)
				restarted := *f.engine
				f.engine = &restarted
			}
			err := f.engine.cycle(ctx, false)
			if tc.wantAttempt {
				if attempts != 1 || !errors.Is(err, stop) {
					t.Fatalf("first check did not immediately prepare release: attempts=%d err=%v", attempts, err)
				}
			} else if attempts != 0 || err != nil {
				t.Fatalf("unexpected release attempt: attempts=%d err=%v", attempts, err)
			}
			s := fixtureState(t, f)
			if tc.wantAttempt && (s.Job == nil || s.Job.Target != f.head || !s.Job.Started.Equal(f.now) || s.Job.Phase != "preflight") {
				t.Fatalf("release was not durably started in preflight: %+v", s.Job)
			}
			if !tc.noRelease && (s.Released != base || !s.ReleasedAt.Equal(published)) {
				t.Fatalf("startup reset the repository's release baseline: %+v", s)
			}
			if tc.noRelease && (s.Released != "" || !s.ReleasedAt.IsZero()) {
				t.Fatalf("startup invented a previous release: %+v", s)
			}
			if queries != 1 {
				t.Fatalf("expected one baseline lookup, got %d", queries)
			}
		})
	}
}

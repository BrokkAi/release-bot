package releasebot

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTriage(t *testing.T) {
	decision, err := parseTriage("thinking...\nTRIAGE_RESULT {\"decision\":\"release\",\"reason\":\" fixes a crash on startup \"}\n")
	if err != nil || decision.Decision != "release" || decision.Reason != "fixes a crash on startup" {
		t.Fatalf("unexpected decision %+v, %v", decision, err)
	}
	for _, text := range []string{"no decision", "TRIAGE_RESULT {\"decision\":\"maybe\"}", "TRIAGE_RESULT not json"} {
		if _, err := parseTriage(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestAgentTriageRemoteAdvanceWithLocalCommits(t *testing.T) {
	for _, quiet := range []time.Duration{0, 15 * time.Minute} {
		t.Run(quiet.String(), func(t *testing.T) {
			f := newFixture(t)
			cfg := &f.engine.config
			cfg.Triage, cfg.Burst, cfg.Quiet = true, 0, Duration(quiet)
			base := cfg.InitialRef
			cfg.InitialRef = ""
			ctx := context.Background()
			if err := f.engine.git.open(ctx); err != nil {
				t.Fatal(err)
			}
			localGit(t, cfg.Directory, "commit", "--allow-empty", "-m", "local documentation")
			localHead := localGit(t, cfg.Directory, "rev-parse", "HEAD")
			if err := writeState(*cfg, &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Released: base, ReleasedAt: f.now.Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			var prompts []string
			f.engine.agent = triageAgent{
				scriptedAgent: func(context.Context, string) (Result, error) {
					t.Fatal("wait decision started a release")
					return Result{}, nil
				},
				triage: func(_ context.Context, prompt string) (TriageDecision, error) {
					prompts = append(prompts, prompt)
					return TriageDecision{Decision: "wait", Reason: "routine changes"}, nil
				},
			}
			cycle := func(want int) {
				t.Helper()
				if err := f.engine.cycle(ctx, false); err != nil {
					t.Fatal(err)
				}
				if len(prompts) != want {
					t.Fatalf("got %d triage calls, want %d", len(prompts), want)
				}
			}
			if quiet > 0 {
				cycle(0)
				f.now = f.now.Add(quiet)
			}
			cycle(1)
			f.now = f.now.Add(time.Hour)
			cycle(1) // Persisted decisions are reused for unchanged commits.
			localGit(t, f.source, "commit", "--allow-empty", "-m", "hotfix: crash on startup")
			localGit(t, f.source, "push", "origin", "master")
			remoteHead := localGit(t, f.source, "rev-parse", "HEAD")
			if quiet > 0 {
				cycle(1)
				f.now = f.now.Add(quiet)
			}
			cycle(2)
			for _, want := range []string{`"head": "` + localHead + `"`, `"remote_head": "` + remoteHead + `"`, `"unreleased_commits": 3`} {
				if !strings.Contains(prompts[1], want) {
					t.Fatalf("prompt lacks %q:\n%s", want, prompts[1])
				}
			}
			if got := localGit(t, cfg.Directory, "rev-parse", "HEAD"); got != localHead {
				t.Fatalf("local work moved: got %s, want %s", got, localHead)
			}
			f.now = f.now.Add(time.Hour)
			cycle(2)
			// Older state has no remote head and must be assessed once again.
			s := fixtureState(t, f)
			if s.Triage.RemoteHead != remoteHead || s.Triage.Head != localHead || s.Job != nil {
				t.Fatalf("unexpected saved triage state: %+v", s)
			}
			s.Triage.RemoteHead = ""
			if err := writeState(*cfg, s); err != nil {
				t.Fatal(err)
			}
			cycle(3)
			cycle(3)
		})
	}
}

// A recently released repository with only one unreleased commit is not due
// by cadence; the agent decides whether that commit warrants an early release.
func TestAgentTriageReleasesImportantFixesEarly(t *testing.T) {
	f := newFixture(t)
	cfg := &f.engine.config
	cfg.Burst = 0
	cfg.Quiet = 0
	base := cfg.InitialRef
	cfg.InitialRef = ""
	if err := writeState(*cfg, &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Released: base, ReleasedAt: f.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var prompts []string
	decision := TriageDecision{Decision: "wait", Reason: "documentation only"}
	var dirty bool
	f.engine.agent = triageAgent{
		scriptedAgent: func(context.Context, string) (Result, error) {
			return Result{}, errors.New("release attempt started")
		},
		triage: func(ctx context.Context, prompt string) (TriageDecision, error) {
			prompts = append(prompts, prompt)
			if dirty {
				writeTestFile(t, filepath.Join(cfg.Directory, "scratch"), "left behind")
			}
			return decision, nil
		},
	}
	cycle := func() error { return f.engine.cycle(context.Background(), false) }

	cfg.Triage = false
	if err := cycle(); err != nil || len(prompts) != 0 {
		t.Fatalf("disabled triage ran: %d prompts, %v", len(prompts), err)
	}
	cfg.Triage = true
	if err := cycle(); err != nil || len(prompts) != 1 {
		t.Fatalf("triage did not run once: %d prompts, %v", len(prompts), err)
	}
	prompt := prompts[0]
	for _, want := range []string{"TRIAGE_RESULT", "\"previous_release_commit\": \"" + base + "\"", "\"head\": \"" + f.head + "\"", "\"unreleased_commits\": 1"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	s := fixtureState(t, f)
	if s.Job != nil || s.Triage == nil || s.Triage.Decision != "wait" || s.Triage.Head != f.head {
		t.Fatalf("wait decision not recorded: %+v", s.Triage)
	}
	// The same commits are not assessed again on every poll.
	f.now = f.now.Add(time.Hour)
	if err := cycle(); err != nil || len(prompts) != 1 {
		t.Fatalf("triage repeated for unchanged commits: %d prompts, %v", len(prompts), err)
	}

	// A new commit is assessed; a session that edits the checkout is ignored.
	localGit(t, f.source, "commit", "--allow-empty", "-m", "fix: crash on startup")
	localGit(t, f.source, "push", "origin", "master")
	fix := localGit(t, f.source, "rev-parse", "HEAD")
	decision = TriageDecision{Decision: "release", Reason: "crash fix"}
	dirty = true
	if err := cycle(); err != nil || len(prompts) != 2 {
		t.Fatalf("new commit not triaged: %d prompts, %v", len(prompts), err)
	}
	s = fixtureState(t, f)
	if s.Job != nil || s.Triage == nil || s.Triage.Decision != "error" || s.Triage.Head != fix {
		t.Fatalf("dirty triage session was trusted: %+v job=%v", s.Triage, s.Job)
	}
	localGit(t, cfg.Directory, "clean", "-fd")
	dirty = false
	// Errors are retried after the retry delay, not on every poll.
	if err := cycle(); err != nil || len(prompts) != 2 {
		t.Fatalf("triage error retried immediately: %d prompts, %v", len(prompts), err)
	}
	f.now = f.now.Add(time.Duration(cfg.RetryDelay))
	err := cycle()
	if err == nil || !strings.Contains(err.Error(), "release attempt started") || len(prompts) != 3 {
		t.Fatalf("release decision did not start a release: %d prompts, %v", len(prompts), err)
	}
	s = fixtureState(t, f)
	if s.Job == nil || s.Job.Target != fix || s.Triage.Decision != "release" || s.Triage.Reason != "crash fix" {
		t.Fatalf("release decision not recorded: job=%+v triage=%+v", s.Job, s.Triage)
	}
}

func TestAgentTriageRespectsQuietPeriodAndDeadline(t *testing.T) {
	f := newFixture(t)
	cfg := &f.engine.config
	cfg.Burst = 0
	cfg.Quiet = Duration(15 * time.Minute)
	base := cfg.InitialRef
	cfg.InitialRef = ""
	if err := writeState(*cfg, &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Released: base, ReleasedAt: f.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.engine.agent = triageAgent{
		scriptedAgent: func(context.Context, string) (Result, error) { return Result{}, errors.New("release attempt started") },
		triage: func(context.Context, string) (TriageDecision, error) {
			calls++
			return TriageDecision{}, errors.New("agent unavailable")
		},
	}
	// Freshly observed commits wait for the quiet period before triage.
	if err := f.engine.cycle(context.Background(), false); err != nil || calls != 0 {
		t.Fatalf("triage ran during the quiet period: %d, %v", calls, err)
	}
	f.now = f.now.Add(15 * time.Minute)
	if err := f.engine.cycle(context.Background(), false); err != nil || calls != 1 {
		t.Fatalf("triage did not run after the quiet period: %d, %v", calls, err)
	}
	if s := fixtureState(t, f); s.Job != nil || s.Triage == nil || s.Triage.Decision != "error" {
		t.Fatalf("agent failure not recorded: %+v", s.Triage)
	}
	// The daily deadline releases regardless of triage failures.
	f.now = f.now.Add(24 * time.Hour)
	if err := f.engine.cycle(context.Background(), false); err == nil || !strings.Contains(err.Error(), "release attempt started") || calls != 1 {
		t.Fatalf("deadline release blocked by triage: %d, %v", calls, err)
	}
}

package releasebot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func countedPlan(f *fixture, counts string) *PublicationPlan {
	p := f.plan()
	for i := range p.Destinations[0].Checks {
		c := &p.Destinations[0].Checks[i]
		command := append([]string{}, c.Command...)
		// Count actual subprocess executions outside the prepared tree.
		c.Command = append([]string{"sh", "-c", `printf '%s\n' "$1" >> "$2"; shift 2; exec "$@"`, "check", c.Kind, counts}, command...)
	}
	return p
}

func TestRestartReusesPreparationAndBuildButRefreshesMutableGates(t *testing.T) {
	for _, revoked := range []string{"", "authorization", "operator"} {
		t.Run("revoked="+revoked, func(t *testing.T) {
			f := newFixture(t)
			counts := filepath.Join(t.TempDir(), "checks")
			operator := filepath.Join(t.TempDir(), "operator")
			writeTestFile(t, operator, "allowed")
			f.engine.config.Preflight = []string{"sh", "-c", `echo operator >> "$1"; test -f "$2"`, "gate", counts, operator}
			preparations, publications := 0, 0
			lost := errors.New("fixture connection lost")
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				if strings.HasPrefix(prompt, "# Publishability") {
					preparations++
					return Result{Status: "ready", Plan: countedPlan(f, counts)}, nil
				}
				publications++
				s := fixtureState(t, f)
				if s.Job.ValidatedAt.IsZero() {
					t.Fatal("publication has no durable gate timestamp")
				}
				if publications == 1 {
					return Result{}, lost
				}
				return f.published(t, true), nil
			})
			if err := f.engine.cycle(context.Background(), false); !errors.Is(err, lost) {
				t.Fatal(err)
			}
			s := fixtureState(t, f)
			if len(s.Job.BuildChecks) != 1 || s.Job.NeedsPreparation {
				t.Fatalf("checkpoint lost: %+v", s.Job)
			}
			switch revoked {
			case "authorization":
				if err := os.Remove(f.permission); err != nil {
					t.Fatal(err)
				}
			case "operator":
				if err := os.Remove(operator); err != nil {
					t.Fatal(err)
				}
			}
			// Reload persisted state and resume immediately, as a new daemon does.
			restarted := *f.engine
			restarted.starting = true
			err := restarted.cycle(context.Background(), false)
			if revoked == "" {
				if err != nil || publications != 2 || fixtureState(t, f).Job != nil {
					t.Fatalf("resume: publications=%d err=%v", publications, err)
				}
			} else if err == nil || publications != 1 {
				t.Fatalf("revoked gate bypassed: publications=%d err=%v", publications, err)
			}
			if preparations != 1 {
				t.Fatalf("repeated preparation %d times", preparations)
			}
			data, err := os.ReadFile(counts)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if strings.Count(text, "build\n") != 1 || strings.Count(text, "authorization\n") != 2 {
				t.Fatalf("wrong check reuse: %s", text)
			}
			if revoked != "authorization" && (strings.Count(text, "version\n") != 2 || strings.Count(text, "operator\n") != 2) {
				t.Fatalf("mutable checks cached: %s", text)
			}
		})
	}
}

func TestChangedInputsInvalidateCheckpoints(t *testing.T) {
	for _, change := range []string{"commit", "dirty tree", "command", "destination", "target", "legacy state"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			counts := filepath.Join(t.TempDir(), "checks")
			preparations := 0
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				if strings.HasPrefix(prompt, "# Publishability") {
					preparations++
					if change == "dirty tree" && preparations == 2 {
						// Repairing an edited tree back to the old commit must not resurrect its cache.
						localGit(t, f.engine.config.Directory, "restore", "manifest")
					}
					return Result{Status: "ready", Plan: countedPlan(f, counts)}, nil
				}
				return Result{}, errors.New("fixture publisher disconnected")
			})
			if f.engine.cycle(context.Background(), false) == nil {
				t.Fatal("fixture should fail")
			}
			s := fixtureState(t, f)
			switch change {
			case "commit":
				localGit(t, f.engine.config.Directory, "commit", "--allow-empty", "-m", "changed inputs")
				localGit(t, f.engine.config.Directory, "push", "origin", "master")
				f.head = localGit(t, f.engine.config.Directory, "rev-parse", "HEAD")
			case "dirty tree":
				writeTestFile(t, filepath.Join(f.engine.config.Directory, "manifest"), "modified")
			case "command":
				s.Job.Plan.Destinations[0].Checks[0].Command[2] = "true; " + s.Job.Plan.Destinations[0].Checks[0].Command[2]
			case "destination":
				s.Job.Plan.Destinations[0].Environment = "different publishing environment"
			case "target":
				s.Job.Target = f.engine.config.InitialRef
			case "legacy state":
				s.Job.BuildChecks = nil
				s.Job.ValidatedAt = time.Time{}
			}
			if err := writeState(f.engine.config, s); err != nil {
				t.Fatal(err)
			}
			f.engine.starting = true
			if f.engine.cycle(context.Background(), false) == nil {
				t.Fatal("fixture should fail")
			}
			data, err := os.ReadFile(counts)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "build\n") != 2 {
				t.Fatalf("stale build reused: %s", data)
			}
			want := 1
			if change == "commit" || change == "dirty tree" {
				want = 2
			}
			if preparations != want {
				t.Fatalf("preparations=%d want=%d", preparations, want)
			}
		})
	}
}

func TestInterruptedBuildGateKeepsOnlyCompletedChecks(t *testing.T) {
	f := newFixture(t)
	counts := filepath.Join(t.TempDir(), "checks")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	preparations := 0
	plan := countedPlan(f, counts)
	// The build finishes but the later authorization gate is interrupted.
	marker := filepath.Join(t.TempDir(), "gate-started")
	original := plan.Destinations[0].Checks[1].Command
	plan.Destinations[0].Checks[1].Command = append([]string{"sh", "-c", `if [ ! -f "$1" ]; then touch "$1"; sleep 30; fi; shift; exec "$@"`, "gate", marker}, original...)
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			preparations++
			return Result{Status: "ready", Plan: plan}, nil
		}
		return f.published(t, true), nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTicker(10 * time.Millisecond)
		defer timer.Stop()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-timer.C:
				if _, err := os.Stat(marker); err == nil {
					cancel()
					return
				}
			case <-deadline.C:
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	if err := f.engine.cycle(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-done
	s := fixtureState(t, f)
	if len(s.Job.BuildChecks) != 1 || s.Job.NeedsPreparation {
		t.Fatalf("interruption lost checkpoint: %+v", s.Job)
	}
	if err := f.engine.cycle(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(counts)
	if err != nil {
		t.Fatal(err)
	}
	if preparations != 1 || strings.Count(string(data), "build\n") != 1 {
		t.Fatalf("repeated completed work: preparations=%d checks=%s", preparations, data)
	}
}

func TestLostPublicationReceiptReconcilesWithoutAgentEvenAtAttemptLimit(t *testing.T) {
	for _, phase := range []string{"publish", "validating", "preflight"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			calls := 0
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				calls++
				if strings.HasPrefix(prompt, "# Publishability") {
					return Result{Status: "ready", Plan: f.plan()}, nil
				}
				f.published(t, true)
				return Result{}, errors.New("lost receipt")
			})
			if f.engine.cycle(context.Background(), false) == nil {
				t.Fatal("fixture should fail")
			}
			s := fixtureState(t, f)
			s.Job.Tries = f.engine.config.Attempts
			s.Job.Phase = phase
			s.Job.NeedsPreparation = phase != "publish"
			if err := writeState(f.engine.config, s); err != nil {
				t.Fatal(err)
			}
			f.engine.starting = true
			if err := f.engine.cycle(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || fixtureState(t, f).Job != nil {
				t.Fatal("lost receipt restarted an agent")
			}
		})
	}
}

func TestBlockedPublisherRequestsFocusedPreparation(t *testing.T) {
	f := newFixture(t)
	preparations := 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			preparations++
			if preparations == 2 && !strings.Contains(prompt, "workflow needs repair") {
				t.Fatal("lost repair context")
			}
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		if preparations == 1 {
			return parseResult(`RELEASE_RESULT {"status":"blocked","detail":"workflow needs repair"}`)
		}
		return f.published(t, true), nil
	})
	if f.engine.cycle(context.Background(), false) == nil {
		t.Fatal("fixture should fail")
	}
	if !fixtureState(t, f).Job.NeedsPreparation {
		t.Fatal("blocked plan marked reusable")
	}
	f.engine.starting = true
	if err := f.engine.cycle(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if preparations != 2 {
		t.Fatal("blocked plan was not repaired")
	}
}

func TestFailedBuildIsNeverCheckpointed(t *testing.T) {
	f := newFixture(t)
	counts := filepath.Join(t.TempDir(), "checks")
	permit := filepath.Join(t.TempDir(), "build-permit")
	preparations := 0
	plan := countedPlan(f, counts)
	build := &plan.Destinations[0].Checks[0]
	build.Command = append(build.Command[:len(build.Command)-3], "test", "-f", permit)
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			preparations++
			return Result{Status: "ready", Plan: plan}, nil
		}
		return f.published(t, true), nil
	})
	if f.engine.cycle(context.Background(), false) == nil {
		t.Fatal("failed build passed")
	}
	s := fixtureState(t, f)
	if len(s.Job.BuildChecks) != 0 || !s.Job.NeedsPreparation {
		t.Fatalf("failed check cached: %+v", s.Job)
	}
	writeTestFile(t, permit, "build repaired")
	f.engine.starting = true
	if err := f.engine.cycle(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(counts)
	if err != nil {
		t.Fatal(err)
	}
	if preparations != 2 || strings.Count(string(data), "build\n") != 2 {
		t.Fatalf("failed build reused: %s", data)
	}
}

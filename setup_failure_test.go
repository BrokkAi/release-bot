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

func TestDaemonExitsOnBadModelWithoutSpendingAttemptsAndUsesCorrectedModel(t *testing.T) {
	f := newFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.engine.config
	cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_WIRE_PEER": "model"}, Model: "fixture-typo"}
	cfg.Poll = Duration(time.Hour)
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := Run(ctx, cfg, f.engine.log, false, false)
		cancel()
		var setup *agentSetupError
		if !errors.As(err, &setup) || !strings.Contains(err.Error(), "fixture-typo") {
			t.Fatalf("did not exit with startup error: %v", err)
		}
		s := fixtureState(t, f)
		if s.Job.Tries != 0 || !s.Job.RetryAt.IsZero() || s.Job.SetupFailure == "" || s.Job.Failure != "" {
			t.Fatalf("setup error poisoned job: %+v", s.Job)
		}
	}
	sessions, err := filepath.Glob(filepath.Join(cfg.StateDirectory, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("daemon retried invalid model: %d sessions", len(sessions))
	}
	cfg.Agent.Model = "fixture-large"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The protocol fixture returns a publication receipt, not a preparation plan;
	// reaching that response proves the corrected selection was used.
	err = Run(ctx, cfg, f.engine.log, true, false)
	if err == nil || !strings.Contains(err.Error(), "preflight requires status ready") {
		t.Fatalf("corrected model never reached prompt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Directory, "wire.txt")); err != nil {
		t.Fatal("corrected model never performed work", err)
	}
	s := fixtureState(t, f)
	if s.Job.SetupFailure != "" || s.Job.Tries != 1 {
		t.Fatalf("stale setup failure retained: %+v", s.Job)
	}
}

func TestLegacyModelTypoAtBudgetLimitUsesCurrentCommand(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := f.engine.bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Job = &Job{Target: f.head, Phase: "preflight", Tries: f.engine.config.Attempts, RetryAt: f.now.Add(time.Hour), Failure: `preflight agent: unknown Model "gpt-5-6-luna"; available values: gpt-5.6-luna, gpt-5.6-terra`}
	if err := writeState(f.engine.config, s); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.engine.config
	cfg.Agent = AgentConfig{Command: []string{executable, "-test.run=^TestWirePeer$"}, Environment: map[string]string{"RELEASE_BOT_WIRE_PEER": "model"}, Model: "fixture-large"}
	err = Run(ctx, cfg, f.engine.log, true, false)
	if err == nil || !strings.Contains(err.Error(), "preflight requires status ready") {
		t.Fatalf("old typo blocked corrected command: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Directory, "wire.txt")); err != nil {
		t.Fatal("current agent settings were not used", err)
	}
	s = fixtureState(t, f)
	// Refund exactly the known invalid attempt, not the earlier real failures.
	if s.Job.Tries != cfg.Attempts || s.Job.SetupFailure != "" || strings.Contains(s.Job.Failure, "gpt-5-6-luna") {
		t.Fatalf("incorrect migration: %+v", s.Job)
	}
}

func TestSetupFailureDuringPublicationPreservesCheckpointsAndEarlierFailures(t *testing.T) {
	f := newFixture(t)
	preparations := 0
	setupErr := &agentSetupError{errors.New("unsupported effort")}
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			preparations++
			return Result{Status: "ready", Plan: f.plan()}, nil
		}
		return Result{}, setupErr
	})
	ctx := context.Background()
	if err := f.engine.cycle(ctx, false); !errors.Is(err, setupErr) {
		t.Fatal(err)
	}
	s := fixtureState(t, f)
	if s.Job.Tries != 0 || s.Job.Phase != "publish" || len(s.Job.BuildChecks) != 1 {
		t.Fatalf("checkpoint lost: %+v", s.Job)
	}
	s.Job.Tries = 1
	s.Job.Failure = "earlier real release failure"
	if err := writeState(f.engine.config, s); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.cycle(ctx, false); !errors.Is(err, setupErr) {
		t.Fatal(err)
	}
	s = fixtureState(t, f)
	if s.Job.Tries != 1 || s.Job.Failure != "earlier real release failure" || !s.Job.RetryAt.IsZero() {
		t.Fatalf("earlier failure lost: %+v", s.Job)
	}
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			t.Fatal("repeated preparation")
		}
		return f.published(t, true), nil
	})
	if err := f.engine.cycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	if preparations != 1 || fixtureState(t, f).Job != nil {
		t.Fatal("corrected startup did not resume")
	}
}

func TestRealExhaustedBudgetExitsWithoutTryingNewSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := f.engine.bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Job = &Job{Target: f.head, Phase: "preflight", Tries: f.engine.config.Attempts, Failure: "publishability gate: unknown Model is in a tool log"}
	if err := writeState(f.engine.config, s); err != nil {
		t.Fatal(err)
	}
	cfg := f.engine.config
	cfg.Agent.Model = "different-model"
	cfg.Agent.Command = []string{"must-not-launch-this-agent"}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := Run(bounded, cfg, f.engine.log, false, false); !errors.Is(err, errAttemptsExhausted) {
		t.Fatalf("real budget looped or reset: %v", err)
	}
	if fixtureState(t, f).Job.Tries != cfg.Attempts {
		t.Fatal("real failures were refunded")
	}
}

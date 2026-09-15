package releasebot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryFinishPersistsReceiptsAndBaseline(t *testing.T) {
	f := newFixture(t)
	cfg := f.engine.config
	s := &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory}
	for i := 1; i <= 2; i++ {
		plan := f.plan()
		plan.Tag = fmt.Sprintf("v%d", i)
		plan.Commit = fmt.Sprintf("%040d", i)
		plan.Destinations[0].Version = fmt.Sprint(i)
		plan.Destinations[0].Checks[0].Evidence = fmt.Sprintf("evidence %d", i)
		r := Result{Status: "released", Tag: plan.Tag, Commit: plan.Commit, URL: "https://example.invalid/" + plan.Tag, Plan: plan}
		s.Job = &Job{Target: r.Commit}
		if err := f.engine.finish(s, r); err != nil {
			t.Fatal(err)
		}
		plan.Destinations[0].Version = "mutated"
		if s.History[i-1].Result.Plan.Destinations[0].Version != fmt.Sprint(i) {
			t.Fatal("history shares mutable plan")
		}
		s = fixtureState(t, f) // restart from disk
		if s.Released != r.Commit || s.Job != nil || len(s.History) != i || s.LastResult.Tag != r.Tag {
			t.Fatalf("inconsistent snapshot: %+v", s)
		}
		f.now = f.now.Add(time.Hour)
	}
	for i, receipt := range s.VerifiedHistory() {
		if receipt.Result.Commit != fmt.Sprintf("%040d", i+1) || receipt.Result.Plan.Destinations[0].Checks[0].Evidence != fmt.Sprintf("evidence %d", i+1) || receipt.VerifiedAt == nil {
			t.Fatalf("lost receipt: %+v", receipt)
		}
	}
	before := s.VerifiedHistory()
	if err := f.engine.finish(s, *s.LastResult); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, fixtureState(t, f).VerifiedHistory()) {
		t.Fatal("duplicate finish changed history")
	}
	// A partial temporary write left by interruption must not replace the snapshot.
	writeTestFile(t, filepath.Join(cfg.StateDirectory, ".state-interrupted"), `{"history":`)
	if !reflect.DeepEqual(before, fixtureState(t, f).VerifiedHistory()) {
		t.Fatal("interrupted temporary write affected state")
	}
	info, err := os.Stat(filepath.Join(cfg.StateDirectory, "state.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private state permissions: %v %v", info, err)
	}
}

func TestHistoryRetentionOrderingAndLegacy(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	legacy := Result{Status: "released", Tag: "legacy", Commit: "legacy"}
	s := &State{LastResult: &legacy}
	if got := s.VerifiedHistory(); len(got) != 1 || got[0].VerifiedAt != nil || s.History != nil {
		t.Fatal("legacy read invented time or mutated state")
	}
	s.ReleasedAt = at
	if !s.VerifiedHistory()[0].VerifiedAt.Equal(at) {
		t.Fatal("legacy timestamp lost")
	}
	for i := 1; i <= 101; i++ {
		s.recordVerified(Result{Status: "released", Tag: fmt.Sprintf("v%03d", i), Commit: fmt.Sprint(i)}, at.Add(time.Duration(i)*time.Hour))
	}
	got := s.VerifiedHistory()
	if len(got) != HistoryLimit || got[0].Result.Tag != "v002" || got[99].Result.Tag != "v101" {
		t.Fatalf("wrong retention: %+v", got)
	}
	s = &State{}
	s.recordVerified(Result{Tag: "same", Commit: "b"}, at)
	s.recordVerified(Result{Tag: "same", Commit: "a"}, at)
	s.recordVerified(Result{Tag: "same", Commit: "a"}, at.Add(time.Hour))
	got = s.VerifiedHistory()
	if len(got) != 2 || got[0].Result.Commit != "a" || got[1].Result.Commit != "b" {
		t.Fatal("unstable ordering or incorrect deduplication")
	}
}

func TestHistoryTwoVerifiedReleases(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 2; i++ {
		if i == 2 {
			localGit(t, f.source, "commit", "--allow-empty", "-m", "next feature")
			localGit(t, f.source, "push", "origin", "master")
			f.head = localGit(t, f.source, "rev-parse", "HEAD")
			f.now = f.now.Add(25 * time.Hour)
		}
		plan := f.plan()
		plan.Tag = fmt.Sprintf("v%d.0.0", i)
		plan.Destinations[0].Version = fmt.Sprintf("%d.0.0", i)
		plan.Destinations[0].Checks[0].Evidence = fmt.Sprintf("build evidence %d", i)
		f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
			if strings.HasPrefix(prompt, "# Publishability") {
				return Result{Status: "ready", Plan: plan}, nil
			}
			localGit(t, f.engine.config.Directory, "tag", plan.Tag, plan.Commit)
			localGit(t, f.engine.config.Directory, "push", "origin", plan.Tag)
			writeTestFile(t, f.registry, "all artifacts uploaded")
			return Result{Status: "released", Tag: plan.Tag, Commit: plan.Commit}, nil
		})
		if err := f.engine.cycle(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		receipts := fixtureState(t, f).VerifiedHistory()
		if len(receipts) != i || !reflect.DeepEqual(receipts[i-1].Result.Plan, plan) {
			t.Fatalf("release %d receipt lost: %+v", i, receipts)
		}
	}
	receipts := fixtureState(t, f).VerifiedHistory()
	if receipts[0].Result.Commit == receipts[1].Result.Commit || receipts[0].Result.Plan.Destinations[0].Checks[0].Evidence != "build evidence 1" {
		t.Fatal("new release replaced earlier evidence")
	}
}

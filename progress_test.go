package releasebot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestProgressTracksVerifiedReleaseAndRecovery(t *testing.T) {
	for _, complete := range []bool{true, false} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			f := newFixture(t)
			var snapshots []Progress
			f.engine.observe = func(p Progress) {
				snapshots = append(snapshots, p)
				if p.Phase == "publishing" {
					s := fixtureState(t, f)
					if s.Job.Plan == nil || s.Job.ValidatedAt.IsZero() || s.Job.Phase != "publish" {
						t.Fatal("publication shown before durable validation")
					}
				}
			}
			calls := 0
			f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
				calls++
				if strings.HasPrefix(prompt, "# Publishability") {
					return Result{Status: "ready", Plan: f.plan()}, nil
				}
				return f.published(t, complete), nil
			})
			err := f.engine.cycle(context.Background(), false)
			if complete && err != nil || !complete && err == nil {
				t.Fatalf("cycle: %v", err)
			}
			phases := map[string]bool{}
			for _, p := range snapshots {
				phases[p.Phase] = true
				if p.Phase == "publishing" && (p.LastTag != "" || p.Releases[0].Status != "publish") {
					t.Fatal("unverified publication shown as complete or old snapshot mutated")
				}
			}
			for _, phase := range []string{"fetching", "preparing", "attempt", "validating", "publishing", "verifying"} {
				if !phases[phase] {
					t.Errorf("missing %s", phase)
				}
			}
			last := snapshots[len(snapshots)-1]
			if !complete {
				if last.LastTag != "" || last.Releases[0].Status == "released" {
					t.Fatal("partial publication counted as verified")
				}
				writeTestFile(t, f.registry, "all artifacts uploaded")
				f.now = f.now.Add(time.Hour)
				if err := f.engine.cycle(context.Background(), false); err != nil {
					t.Fatal(err)
				}
				last = snapshots[len(snapshots)-1]
			}
			if last.Phase != "complete" || last.LastTag != "v1.0.0" || len(last.Releases) != 1 || last.Releases[0].Status != "released" || calls != 2 {
				t.Fatalf("wrong verified result or repeated agent work: %+v", last)
			}
		})
	}
}

func TestProgressWaitAndOwnedPlan(t *testing.T) {
	now := time.Now()
	var got Progress
	e := engine{config: DefaultConfig(), now: func() time.Time { return now }, observe: func(p Progress) { got = p }}
	s := &State{Job: &Job{Target: "abc", Plan: &PublicationPlan{Tag: "v2", Destinations: []Destination{{Name: "original"}}}}}
	e.report(s, "waiting", "Retry")
	s.Job.Plan.Destinations[0].Name = "mutated"
	if !strings.Contains(got.Releases[0].Body, "original") || strings.Contains(got.Releases[0].Body, "mutated") {
		t.Fatal("snapshot shares mutable plan data")
	}
	if !got.WakeAt.Equal(now.Add(time.Duration(e.config.Poll))) {
		t.Fatal("countdown does not match polling")
	}
}

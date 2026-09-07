package releasebot

import (
	"context"
	"strings"
	"testing"
)

func TestIncompletePublishabilityPlanNeverStartsPublisher(t *testing.T) {
	for _, missing := range []string{"build", "authorization", "version", "post-publication"} {
		t.Run(missing, func(t *testing.T) {
			f := newFixture(t)
			plan := f.plan()
			if missing == "post-publication" {
				plan.Destinations[0].Verify = nil
			} else {
				var keep []PublishabilityCheck
				for _, check := range plan.Destinations[0].Checks {
					if check.Kind != missing {
						keep = append(keep, check)
					}
				}
				plan.Destinations[0].Checks = keep
			}
			calls := 0
			f.engine.agent = scriptedAgent(func(context.Context, string) (Result, error) {
				calls++
				return Result{Status: "ready", Plan: plan}, nil
			})
			if f.engine.cycle(context.Background(), false) == nil {
				t.Fatal("incomplete plan passed")
			}
			if calls != 1 {
				t.Fatal("publisher started with incomplete checks")
			}
		})
	}
}
func TestOperatorPreflightAndChangedPublicationAreRejected(t *testing.T) {
	t.Run("operator gate", func(t *testing.T) {
		f := newFixture(t)
		f.engine.config.Preflight = []string{"test", "-f", "missing-required-approval"}
		calls := 0
		f.engine.agent = scriptedAgent(func(context.Context, string) (Result, error) {
			calls++
			return Result{Status: "ready", Plan: f.plan()}, nil
		})
		if f.engine.cycle(context.Background(), false) == nil || calls != 1 {
			t.Fatal("operator preflight bypassed")
		}
	})
	t.Run("different tag", func(t *testing.T) {
		f := newFixture(t)
		f.engine.agent = scriptedAgent(func(ctx context.Context, p string) (Result, error) {
			if strings.HasPrefix(p, "# Publishability") {
				return Result{Status: "ready", Plan: f.plan()}, nil
			}
			return Result{Status: "released", Tag: "unapproved-version", Commit: f.head}, nil
		})
		if err := f.engine.cycle(context.Background(), false); err == nil || !strings.Contains(err.Error(), "validated commit/tag") {
			t.Fatalf("changed publication accepted: %v", err)
		}
	})
}

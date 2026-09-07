package releasebot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func TestFirstReleaseCanPrepareInfrastructureBeforePublication(t *testing.T) {
	f := newFixture(t)
	f.engine.config.InitialRef = ""
	f.engine.config.GitHub.Repo = "owner/repo"
	original := f.head
	if err := os.Remove(f.permission); err != nil {
		t.Fatal(err)
	}
	f.engine.github = github{config: f.engine.config, request: func(ctx context.Context, path, projection string) (string, error) {
		switch {
		case strings.Contains(path, "/releases?"):
			return `{"count":0,"releases":[]}`, nil
		case strings.Contains(path, "/actions/runs?"):
			if !strings.Contains(path, "head_sha="+f.head) {
				t.Fatal("verification used the original commit instead of the setup commit")
			}
			run := goodRun(1, "Release", "v1.0.0", "workflow_dispatch")
			run.SHA, run.Path = f.head, ".github/workflows/release.yml"
			data, err := json.Marshal(map[string]any{"total_count": 1, "workflow_runs": []workflowRun{run}})
			return string(data), err
		case strings.Contains(path, "/releases/tags/"):
			return `{"id":1,"tag_name":"v1.0.0","draft":false,"published_at":"2026-09-07T12:00:00Z"}`, nil
		case strings.Contains(path, "/assets?"):
			return `[]`, nil
		default:
			return "", fmt.Errorf("unexpected endpoint %s", path)
		}
	}}
	publications := 0
	f.engine.agent = scriptedAgent(func(ctx context.Context, prompt string) (Result, error) {
		if strings.HasPrefix(prompt, "# Publishability") {
			if f.head == original {
				// Simulate the agent adding a project's first release infrastructure.
				dir := f.engine.config.Directory
				if err := os.MkdirAll(filepath.Join(dir, ".github/workflows"), 0755); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(dir, ".github/workflows/release.yml"), "name: Release\non: workflow_dispatch\njobs:\n  package:\n    runs-on: ubuntu-latest\n    steps:\n      - run: test -n \"$GITHUB_SHA\"\n")
				writeTestFile(t, filepath.Join(dir, "RELEASING.md"), "Fixture release: validate manifest and publisher authorization, then upload to fixture registry.")
				localGit(t, dir, "add", ".github/workflows/release.yml", "RELEASING.md")
				localGit(t, dir, "commit", "-m", "set up fixture releases")
				localGit(t, dir, "push", "origin", "HEAD:master")
				f.head = localGit(t, dir, "rev-parse", "HEAD")
			}
			plan := f.plan()
			plan.GitHubWorkflows = []string{"release.yml"}
			return Result{Status: "ready", Plan: plan}, nil
		}
		publications++
		return f.published(t, true), nil
	})
	ctx := context.Background()
	if err := f.engine.cycle(ctx, false); err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("first release did not validate publisher rights: %v", err)
	}
	if publications != 0 || localGit(t, f.source, "ls-remote", "--tags", "origin") != "" {
		t.Fatal("setup published before all destinations were authorized")
	}
	s := fixtureState(t, f)
	if s.Job == nil || s.Job.Target != original || s.Job.Plan.Commit != f.head || s.Released != "" {
		t.Fatalf("setup lost original target or advanced release state: %+v", s)
	}
	writeTestFile(t, f.permission, "fixture publishing access repaired")
	if err := Retry(f.engine.config); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.cycle(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = fixtureState(t, f)
	if publications != 1 || s.Job != nil || s.Released != f.head {
		t.Fatal("first release did not finish from the validated setup commit")
	}
}

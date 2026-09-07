package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func goodRun(id int64, name, ref, event string) workflowRun {
	return workflowRun{ID: id, Workflow: 1, Attempt: 1, SHA: "release-sha", Name: name, Path: ".github/workflows/ci.yml", Ref: ref, Event: event, Status: "completed", Conclusion: "success"}
}
func TestActionsVerification(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runs     []workflowRun
		required []string
		expect   string
	}{
		{"empty", nil, nil, "waiting"},
		{"success", []workflowRun{goodRun(1, "CI", "master", "push")}, []string{"ci.yml"}, "success"},
		{"missing release", []workflowRun{goodRun(1, "CI", "master", "push")}, []string{"Release"}, "waiting"},
		{"wrong commit", []workflowRun{{ID: 1, Workflow: 1, SHA: "other", Status: "completed", Conclusion: "success"}}, nil, "waiting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := evaluateRuns(tc.runs, "release-sha", tc.required)
			var wait *waiting
			if tc.expect == "success" && err != nil {
				t.Fatal(err)
			}
			if tc.expect == "waiting" && !errors.As(err, &wait) {
				t.Fatalf("expected pending: %v", err)
			}
		})
	}
	failed := goodRun(1, "CI", "master", "push")
	failed.Conclusion = "failure"
	fixed := failed
	fixed.Attempt = 2
	fixed.Conclusion = "success"
	if err := evaluateRuns([]workflowRun{failed, fixed}, "release-sha", nil); err != nil {
		t.Fatal("latest rerun should supersede failure:", err)
	}
	tagFailure := failed
	tagFailure.ID = 2
	tagFailure.Ref = "v1"
	tagFailure.Event = "release"
	if err := evaluateRuns([]workflowRun{fixed, tagFailure}, "release-sha", nil); err == nil {
		t.Fatal("branch success hid release failure")
	}
	for _, conclusion := range []string{"cancelled", "skipped", "neutral", "timed_out", ""} {
		run := fixed
		run.Conclusion = conclusion
		if evaluateRuns([]workflowRun{run}, "release-sha", nil) == nil {
			t.Fatalf("accepted %q", conclusion)
		}
	}
}
func TestGitHubPublicationAndPagination(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.Workflows = []string{"Release"}
	cfg.GitHub.Assets = []string{"*-linux.tar.gz"}
	for _, tc := range []struct {
		name                     string
		draft, missing, badAsset bool
	}{
		{"valid", false, false, false}, {"draft", true, false, false}, {"missing asset", false, true, false}, {"incomplete asset", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := 0
			g := github{config: cfg, request: func(ctx context.Context, path, query string) (string, error) {
				switch {
				case strings.Contains(path, "/actions/runs"):
					pages++
					var runs []workflowRun
					if strings.HasSuffix(path, "&page=1") {
						for i := 0; i < 100; i++ {
							runs = append(runs, goodRun(int64(i+1), "CI", "master", "push"))
						}
					} else {
						r := goodRun(101, "Release", "v1", "release")
						r.Workflow = 2
						runs = []workflowRun{r}
					}
					b, _ := json.Marshal(map[string]any{"total_count": 101, "workflow_runs": runs})
					return string(b), nil
				case strings.Contains(path, "/releases/tags/"):
					return fmt.Sprintf(`{"id":1,"tag_name":"v1","draft":%v,"published_at":"2026-09-07T00:00:00Z"}`, tc.draft), nil
				case strings.Contains(path, "/assets?"):
					if tc.missing {
						return `[]`, nil
					}
					if tc.badAsset {
						return `[{"name":"app-linux.tar.gz","state":"new","size":0}]`, nil
					}
					return `[{"name":"app-linux.tar.gz","state":"uploaded","size":42}]`, nil
				}
				return "", fmt.Errorf("unexpected endpoint %s", path)
			}}
			err := g.check(context.Background(), Result{Commit: "release-sha", Tag: "v1"})
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("unexpected verification: %v", err)
			}
			if pages != 2 {
				t.Fatal("did not paginate Actions")
			}
		})
	}
}

func TestDiscoveredWorkflowsAreEnforcedWithoutConfiguration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.engine.git.open(ctx); err != nil {
		t.Fatal(err)
	}
	result := f.published(t, true)
	result.Plan = f.plan()
	result.Plan.GitHubWorkflows = []string{"Publish packages"}
	f.engine.config.GitHub.Repo = "owner/repo"
	f.engine.config.VerificationTimeout = Duration(5 * time.Second)
	missingCtx, cancelMissing := context.WithCancel(ctx)
	defer cancelMissing()
	includePublication := false
	f.engine.github = github{config: f.engine.config, request: func(ctx context.Context, path, query string) (string, error) {
		switch {
		case strings.Contains(path, "/actions/runs"):
			ci := goodRun(1, "CI", "master", "push")
			ci.SHA = f.head
			runs := []workflowRun{ci}
			if includePublication {
				publish := goodRun(2, "Publish packages", "v1.0.0", "release")
				publish.SHA = f.head
				publish.Workflow = 2
				runs = append(runs, publish)
			} else {
				// End the pending-workflow poll only after Git verification and
				// Actions discovery, without a timing race against subprocesses.
				cancelMissing()
			}
			data, _ := json.Marshal(map[string]any{"total_count": len(runs), "workflow_runs": runs})
			return string(data), nil
		case strings.Contains(path, "/releases/tags/"):
			return `{"id":1,"tag_name":"v1.0.0","draft":false,"published_at":"2026-09-07T00:00:00Z"}`, nil
		case strings.Contains(path, "/assets?"):
			return `[]`, nil
		default:
			return "", fmt.Errorf("unexpected endpoint %s", path)
		}
	}}
	if err := f.engine.verify(missingCtx, f.head, result); err == nil || !strings.Contains(err.Error(), "missing required workflow Publish packages") {
		t.Fatalf("discovered requirement was ignored: %v", err)
	}
	includePublication = true
	if err := f.engine.verify(ctx, f.head, result); err != nil {
		t.Fatal(err)
	}
}

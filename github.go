package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BrokkAi/release-bot/internal/osrun"
)

type github struct {
	config  Config
	request func(context.Context, string, string) (string, error)
}

func (g github) api(ctx context.Context, endpoint, projection string) (string, error) {
	if g.request != nil {
		return g.request(ctx, endpoint, projection)
	}
	args := []string{"gh", "api", "--hostname", g.config.GitHub.Host, "-H", "Accept: application/vnd.github+json", endpoint}
	if projection != "" {
		args = append(args, "--jq", projection)
	}
	return osrun.Run(ctx, g.config.Directory, map[string]string{"GH_PROMPT_DISABLED": "1", "GH_PAGER": "cat"}, args...)
}
func (g github) path() string { return "repos/" + g.config.GitHubRepo() }

type workflowRun struct {
	ID         int64  `json:"id"`
	Workflow   int64  `json:"workflow_id"`
	Attempt    int    `json:"run_attempt"`
	SHA        string `json:"head_sha"`
	Ref        string `json:"head_branch"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"html_url"`
}
type waiting struct{ reason string }

func (w *waiting) Error() string { return w.reason }

func evaluateRuns(runs []workflowRun, sha string, required []string) error {
	selected := make(map[string]workflowRun)
	for _, run := range runs {
		if run.SHA != sha {
			continue
		}
		key := fmt.Sprintf("%d:%s:%s", run.Workflow, run.Event, run.Ref)
		previous := selected[key]
		if run.ID > previous.ID || (run.ID == previous.ID && run.Attempt > previous.Attempt) {
			selected[key] = run
		}
	}
	if len(selected) == 0 {
		return &waiting{"no Actions runs exist for " + sha}
	}
	found := make(map[string]bool)
	var pending, failed []string
	for _, run := range selected {
		found[run.Name] = true
		found[run.Path] = true
		found[filepath.Base(run.Path)] = true
		label := fmt.Sprintf("%s run %d (%s, %s) %s", run.Name, run.ID, run.Event, run.Ref, run.URL)
		if run.Status != "completed" {
			pending = append(pending, label+": "+run.Status)
		} else if run.Conclusion != "success" {
			failed = append(failed, label+": "+run.Conclusion)
		}
	}
	for _, name := range required {
		if !found[name] {
			pending = append(pending, "missing required workflow "+name)
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("GitHub Actions failed:\n%s\nUse gh run view RUN_ID --log-failed to diagnose", strings.Join(failed, "\n"))
	}
	if len(pending) > 0 {
		sort.Strings(pending)
		return &waiting{strings.Join(pending, "\n")}
	}
	return nil
}
func (g github) check(ctx context.Context, r Result) error {
	var runs []workflowRun
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/actions/runs?head_sha=%s&per_page=100&page=%d", g.path(), url.QueryEscape(r.Commit), page)
		text, err := g.api(ctx, endpoint, `{total_count,workflow_runs:[.workflow_runs[]|{id,workflow_id,run_attempt,head_sha,head_branch,name,path,event,status,conclusion,html_url}]}`)
		if err != nil {
			return err
		}
		var batch struct {
			Count int           `json:"total_count"`
			Runs  []workflowRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal([]byte(text), &batch); err != nil {
			return err
		}
		runs = append(runs, batch.Runs...)
		if len(batch.Runs) < 100 {
			break
		}
		if page >= 10 {
			return errors.New("Actions query reached GitHub's 1000-result limit; cannot verify complete history")
		}
	}
	if err := evaluateRuns(runs, r.Commit, g.config.GitHub.Workflows); err != nil {
		return err
	}
	text, err := g.api(ctx, g.path()+"/releases/tags/"+url.PathEscape(r.Tag), "{id,tag_name,draft,published_at}")
	if err != nil {
		return err
	}
	var release struct {
		ID        int64  `json:"id"`
		Tag       string `json:"tag_name"`
		Draft     bool   `json:"draft"`
		Published string `json:"published_at"`
	}
	if err := json.Unmarshal([]byte(text), &release); err != nil {
		return err
	}
	if release.ID == 0 || release.Tag != r.Tag || release.Draft || release.Published == "" {
		return errors.New("GitHub release must be published, non-draft, and match the tag")
	}
	var names []string
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/releases/%d/assets?per_page=100&page=%d", g.path(), release.ID, page)
		text, err := g.api(ctx, endpoint, `[.[]|{name,state,size}]`)
		if err != nil {
			return err
		}
		var assets []struct {
			Name, State string
			Size        int64
		}
		if err := json.Unmarshal([]byte(text), &assets); err != nil {
			return err
		}
		for _, asset := range assets {
			if asset.State != "uploaded" || asset.Size <= 0 {
				return fmt.Errorf("incomplete release asset %q", asset.Name)
			}
			names = append(names, asset.Name)
		}
		if len(assets) < 100 {
			break
		}
		if page >= 100 {
			return errors.New("too many assets to verify")
		}
	}
	for _, pattern := range g.config.GitHub.Assets {
		present := false
		for _, name := range names {
			match, _ := filepath.Match(pattern, name)
			present = present || match
		}
		if !present {
			return fmt.Errorf("missing release asset matching %q", pattern)
		}
	}
	return nil
}
func (g github) verify(ctx context.Context, r Result) error {
	for {
		err := g.check(ctx, r)
		var pending *waiting
		if !errors.As(err, &pending) {
			return err
		}
		timer := time.NewTimer(min(30*time.Second, time.Duration(g.config.Poll)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s: %w", err, ctx.Err())
		case <-timer.C:
		}
	}
}

type publishedRelease struct {
	Tag       string    `json:"tag_name"`
	Published time.Time `json:"published_at"`
}

func (g github) releases(ctx context.Context) ([]publishedRelease, error) {
	var releases []publishedRelease
	for page := 1; ; page++ {
		text, err := g.api(ctx, fmt.Sprintf("%s/releases?per_page=100&page=%d", g.path(), page), `{count:length,releases:[.[]|select(.draft==false)|{tag_name,published_at}]}`)
		if err != nil {
			return nil, err
		}
		var batch struct {
			Count    int                `json:"count"`
			Releases []publishedRelease `json:"releases"`
		}
		if err := json.Unmarshal([]byte(text), &batch); err != nil {
			return nil, err
		}
		releases = append(releases, batch.Releases...)
		if batch.Count < 100 {
			break
		}
		if page >= 100 {
			return nil, errors.New("too many releases; set initial_ref explicitly")
		}
	}
	sort.Slice(releases, func(i, j int) bool { return releases[i].Published.After(releases[j].Published) })
	return releases, nil
}

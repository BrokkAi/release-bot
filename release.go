package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/BrokkAi/release-bot/internal/osrun"
)

type engine struct {
	config Config
	git    checkout
	github github
	agent  Agent
	log    *slog.Logger
	now    func() time.Time
}

// Run watches one repository. Once executes one poll/recovery attempt; force
// ignores cadence for a new release but never publishes without new commits.
func Run(ctx context.Context, cfg Config, log *slog.Logger, once, force bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	e := engine{config: cfg, git: checkout{cfg}, github: github{config: cfg}, agent: agentProcess{cfg, log}, log: log, now: time.Now}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cycleCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Timeout)+2*time.Duration(cfg.VerificationTimeout)+5*time.Minute)
		err := e.cycle(cycleCtx, force)
		cancel()
		if once {
			return err
		}
		if err != nil {
			log.Error("release cycle", "error", err)
		}
		timer := time.NewTimer(time.Duration(cfg.Poll))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (e *engine) bootstrap(ctx context.Context) (*State, error) {
	s := &State{Format: 1, Remote: e.config.Remote, Branch: e.config.Branch, Directory: e.config.Directory}
	if e.config.InitialRef != "" {
		base, err := e.git.resolve(ctx, e.config.InitialRef)
		if err != nil {
			return nil, err
		}
		s.Released = base
		return s, nil
	}
	if e.config.GitHubRepo() != "" {
		releases, err := e.github.releases(ctx)
		if err != nil {
			return nil, err
		}
		for _, release := range releases {
			commit, err := e.git.resolve(ctx, "refs/tags/"+release.Tag)
			if err != nil {
				continue
			}
			if e.git.contains(ctx, e.git.branchRef(), commit) == nil {
				s.Released = commit
				s.ReleasedAt = release.Published
				break
			}
		}
	}
	return s, nil
}
func due(cfg Config, s *State, total, recent int, now time.Time) string {
	if total == 0 {
		return ""
	}
	age := now.Sub(s.ReleasedAt)
	if !s.ReleasedAt.IsZero() && age < time.Duration(cfg.MinimumGap) {
		return ""
	}
	if s.ReleasedAt.IsZero() || age >= time.Duration(cfg.Daily) {
		return "daily deadline"
	}
	if cfg.Burst > 0 && recent >= cfg.Burst && now.Sub(s.ChangedAt) >= time.Duration(cfg.Quiet) {
		return "commit burst"
	}
	return ""
}
func (e *engine) cycle(ctx context.Context, force bool) error {
	s, err := ReadState(e.config)
	if err != nil {
		return err
	}
	e.log.Info("Checking repository for changes")
	if err := e.git.open(ctx); err != nil {
		return err
	}
	if s == nil {
		s, err = e.bootstrap(ctx)
		if err != nil {
			return err
		}
	}
	if s.Job != nil {
		return e.resume(ctx, s)
	}
	now := e.now().UTC()
	head, err := e.git.resolve(ctx, e.git.branchRef())
	if err != nil {
		return err
	}
	total, recent, err := e.git.changes(ctx, s.Released, head, now.Add(-time.Duration(e.config.BurstWindow)))
	if err != nil {
		return err
	}
	if s.Observed != head {
		s.Observed = head
		s.ChangedAt = now
	}
	if err := writeState(e.config, s); err != nil {
		return err
	}
	reason := due(e.config, s, total, recent, now)
	if force && total > 0 {
		reason = "forced cadence"
	}
	if reason == "" {
		e.log.Info("monitoring", "unreleased_commits", total, "recent_commits", recent)
		return nil
	}
	if err := e.git.advance(ctx); err != nil {
		return err
	}
	s.Job = &Job{Target: head, Started: now}
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.log.Info("release due", "reason", reason, "target", head)
	return e.resume(ctx, s)
}
func (e *engine) resume(ctx context.Context, s *State) error {
	j := s.Job
	if e.now().Before(j.RetryAt) {
		e.log.Info("retry scheduled", "at", j.RetryAt)
		return nil
	}
	if j.Candidate != nil {
		if err := e.verify(ctx, j.Target, *j.Candidate); err == nil {
			return e.finish(s, *j.Candidate)
		} else {
			j.Failure = err.Error()
		}
	}
	if j.Tries >= e.config.Attempts {
		j.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
		if err := writeState(e.config, s); err != nil {
			return err
		}
		return errors.New("agent retry budget exhausted; inspect state/sessions and use retry to resume this release")
	}
	j.Tries++
	j.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
	// The target and consumed attempt are durable before any agent side effects.
	if err := writeState(e.config, s); err != nil {
		return err
	}
	r, err := e.publishAttempt(ctx, s)
	if err == nil && (r.Status != "released" || r.Commit == "" || r.Tag == "") {
		err = fmt.Errorf("agent has not released: %s", r.Detail)
	}
	if err == nil {
		j.Candidate = &r
		if err := writeState(e.config, s); err != nil {
			return err
		}
		err = e.verify(ctx, j.Target, r)
		if err == nil {
			return e.finish(s, r)
		}
	}
	detail := err.Error()
	if len(detail) > 64<<10 {
		detail = detail[len(detail)-(64<<10):]
	}
	j.Failure = detail
	j.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
	if saveErr := writeState(e.config, s); saveErr != nil {
		return saveErr
	}
	return fmt.Errorf("release attempt %d/%d: %w", j.Tries, e.config.Attempts, err)
}
func (e *engine) verify(ctx context.Context, target string, r Result) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.config.VerificationTimeout))
	defer cancel()
	if err := e.git.verify(ctx, target, r); err != nil {
		return err
	}
	if e.config.GitHubRepo() != "" {
		verifier := e.github
		verifier.config.GitHub.Workflows = append([]string{}, e.config.GitHub.Workflows...)
		if r.Plan != nil {
			verifier.config.GitHub.Workflows = append(verifier.config.GitHub.Workflows, r.Plan.GitHubWorkflows...)
		}
		if err := verifier.verify(ctx, r); err != nil {
			return err
		}
	}
	if r.Plan == nil {
		return errors.New("publication has no validated destination plan")
	}
	if err := r.Plan.validate(); err != nil {
		return err
	}
	if r.Plan.Commit != r.Commit || r.Plan.Tag != r.Tag {
		return errors.New("receipt does not match its validated publication plan")
	}
	planJSON, _ := json.Marshal(r.Plan)
	for _, destination := range r.Plan.Destinations {
		_, err := osrun.Run(ctx, e.config.Directory, map[string]string{"RELEASE_COMMIT": r.Commit, "RELEASE_TAG": r.Tag, "RELEASE_TARGET": target, "RELEASE_DESTINATION": destination.Name, "RELEASE_URL": r.URL, "RELEASE_PLAN_JSON": string(planJSON)}, destination.Verify...)
		if err != nil {
			return fmt.Errorf("published destination %s: %w", destination.Name, err)
		}
	}
	if len(e.config.Verify) > 0 {
		_, err := osrun.Run(ctx, e.config.Directory, map[string]string{"RELEASE_COMMIT": r.Commit, "RELEASE_TAG": r.Tag, "RELEASE_URL": r.URL, "RELEASE_TARGET": target, "RELEASE_REMOTE": e.config.Remote, "RELEASE_BRANCH": e.config.Branch}, e.config.Verify...)
		if err != nil {
			return fmt.Errorf("custom release verification: %w", err)
		}
	}
	return nil
}
func (e *engine) finish(s *State, r Result) error {
	s.Released = r.Commit
	s.ReleasedAt = e.now().UTC()
	s.LastResult = &r
	s.Job = nil
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.log.Info("release verified", "tag", r.Tag, "commit", r.Commit, "url", r.URL)
	return nil
}

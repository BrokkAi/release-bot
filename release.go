package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/BrokkAi/release-bot/internal/osrun"
)

type engine struct {
	config   Config
	git      checkout
	github   github
	agent    Agent
	log      *slog.Logger
	now      func() time.Time
	starting bool
}

var errAttemptsExhausted = errors.New("release retry budget exhausted; fix the reported failure and run release-bot retry")

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
	e := engine{config: cfg, git: checkout{cfg}, github: github{config: cfg}, agent: agentProcess{cfg, log}, log: log, now: time.Now, starting: true}
	// Check immediately, including on restart; polling only delays later checks.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cycleCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Timeout)+2*time.Duration(cfg.VerificationTimeout)+5*time.Minute)
		err := e.cycle(cycleCtx, force)
		e.starting = false
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var setup *agentSetupError
		if once || errors.As(err, &setup) || errors.Is(err, errAttemptsExhausted) {
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
	if s.ReleasedAt.IsZero() {
		return "no previous release timestamp"
	}
	age := now.Sub(s.ReleasedAt)
	// The deadline is measured from publication, never from startup or the
	// latest observed commit. Quiet periods apply only to early burst releases.
	if age >= time.Duration(cfg.Daily) {
		return "daily deadline"
	}
	if age < time.Duration(cfg.MinimumGap) {
		return ""
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
	head, err := e.git.releaseHead(ctx)
	if err != nil {
		return err
	}
	remoteHead, err := e.git.resolve(ctx, e.git.branchRef())
	if err != nil {
		return err
	}
	total, recent, err := e.git.changes(ctx, s.Released, head, now.Add(-time.Duration(e.config.BurstWindow)))
	if err != nil {
		return err
	}
	if s.Observed != head || s.ObservedRemote != remoteHead {
		s.Observed = head
		s.ObservedRemote = remoteHead
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
	workBranch, err := e.git.startBranch(ctx)
	if err != nil {
		return err
	}
	// Keep the remote baseline as the ancestry requirement: preparation may
	// merge the local work through a PR using squash or rebase.
	s.Job = &Job{Target: remoteHead, WorkBranch: workBranch, Started: now}
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.log.Info("release due", "reason", reason, "target", head, "work_branch", workBranch)
	return e.resume(ctx, s)
}
func (e *engine) resume(ctx context.Context, s *State) error {
	j := s.Job
	if e.starting && legacySelectionFailure(j.Failure) {
		// Older versions counted selection errors as release attempts. Refund
		// only the last known setup failure, preserving earlier real failures.
		j.Tries = max(0, j.Tries-1)
		j.SetupFailure, j.Failure = j.Failure, ""
		j.RetryAt = time.Time{}
		if err := writeState(e.config, s); err != nil {
			return err
		}
		e.log.Info("Recovered attempt consumed by an invalid agent setting", "model", e.config.Agent.Model, "effort", e.config.Agent.Effort)
	}
	if e.now().Before(j.RetryAt) && !e.starting {
		e.log.Info("retry scheduled", "at", j.RetryAt, "previous_error", j.Failure)
		return nil
	}
	if e.starting {
		e.log.Info("Resuming pending release on startup", "phase", j.Phase, "previous_error", j.Failure, "interruption", j.Interruption)
	}
	candidate := j.Candidate
	if candidate == nil && e.validatePlan(j.Plan) == nil {
		// Publication can finish before the agent delivers its receipt. Verify
		// the saved plan before launching another agent or checking availability,
		// even if an earlier retry already moved back into validation/preparation.
		candidate = &Result{Status: "released", Commit: j.Plan.Commit, Tag: j.Plan.Tag, Plan: j.Plan}
		if e.config.GitHubRepo() != "" {
			candidate.URL = "https://" + e.config.GitHub.Host + "/" + e.config.GitHubRepo() + "/releases/tag/" + url.PathEscape(j.Plan.Tag)
		}
	}
	if candidate != nil {
		e.log.Info("Reconciling previous publication", "tag", candidate.Tag)
		if err := e.verify(ctx, j.Target, *candidate); err == nil {
			return e.finish(s, *candidate)
		} else {
			if errors.Is(ctx.Err(), context.Canceled) {
				return e.interrupted(ctx, s, false)
			}
			// A speculative receipt may simply have no remote tag yet. Keep
			// the publisher's actionable failure for the preparation agent.
			if j.Candidate != nil || j.Failure == "" {
				j.Failure = err.Error()
			}
		}
	}
	if j.Tries >= e.config.Attempts {
		j.RetryAt = time.Time{}
		if err := writeState(e.config, s); err != nil {
			return err
		}
		return fmt.Errorf("%w; last failure: %s", errAttemptsExhausted, j.Failure)
	}
	j.Tries++
	j.Interruption = ""
	j.SetupFailure = ""
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
	if errors.Is(ctx.Err(), context.Canceled) {
		return e.interrupted(ctx, s, true)
	}
	var setup *agentSetupError
	if errors.As(err, &setup) {
		j.Tries--
		j.SetupFailure = err.Error()
		j.RetryAt = time.Time{}
		if saveErr := writeState(e.config, s); saveErr != nil {
			return saveErr
		}
		return err
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
	attemptErr := fmt.Errorf("release attempt %d/%d: %w", j.Tries, e.config.Attempts, err)
	if j.Tries >= e.config.Attempts {
		return errors.Join(errAttemptsExhausted, attemptErr)
	}
	return attemptErr
}

// Compatibility with the precise selector errors emitted before setup failures
// had their own persisted field. Never infer a refund from arbitrary tool errors.
func legacySelectionFailure(failure string) bool {
	failure = strings.TrimPrefix(failure, "preflight agent: ")
	for _, name := range []string{"Model", "Reasoning effort"} {
		if strings.HasPrefix(failure, "unknown "+name+" \"") && strings.Contains(failure, "\"; available values: ") {
			return true
		}
	}
	return false
}

func (e *engine) interrupted(ctx context.Context, s *State, refund bool) error {
	if refund {
		s.Job.Tries--
	}
	s.Job.Interruption = context.Cause(ctx).Error()
	s.Job.RetryAt = time.Time{}
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.log.Info("Release interrupted; saved for immediate resume", "reason", s.Job.Interruption, "phase", s.Job.Phase)
	return ctx.Err()
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

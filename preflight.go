package releasebot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BrokkAi/release-bot/internal/osrun"
)

type PublicationPlan struct {
	Commit          string        `json:"commit"`
	Tag             string        `json:"tag"`
	Destinations    []Destination `json:"destinations"`
	GitHubWorkflows []string      `json:"github_workflows,omitempty"`
}
type Destination struct {
	Name        string                `json:"name"`
	Kind        string                `json:"kind"`
	Version     string                `json:"version"`
	Environment string                `json:"environment"`
	Checks      []PublishabilityCheck `json:"checks"`
	Verify      []string              `json:"verify"`
}
type PublishabilityCheck struct {
	Kind     string   `json:"kind"`
	Command  []string `json:"command"`
	Evidence string   `json:"evidence"`
}

func (p *PublicationPlan) validate() error {
	if p == nil || !fullHash.MatchString(p.Commit) || p.Tag == "" || len(p.Destinations) == 0 {
		return errors.New("preflight must identify a full prepared commit, tag and every publication destination")
	}
	names := make(map[string]bool)
	for _, d := range p.Destinations {
		if d.Name == "" || d.Kind == "" || d.Version == "" || d.Environment == "" || names[d.Name] {
			return errors.New("each destination requires a unique name, kind, version and actual publishing environment")
		}
		if len(d.Verify) == 0 || d.Verify[0] == "" {
			return fmt.Errorf("destination %s needs a post-publication verify command", d.Name)
		}
		names[d.Name] = true
		seen := make(map[string]bool)
		for _, check := range d.Checks {
			if len(check.Command) == 0 || check.Command[0] == "" || check.Evidence == "" {
				return fmt.Errorf("destination %s has an incomplete publishability check", d.Name)
			}
			seen[check.Kind] = true
		}
		for _, kind := range []string{"build", "authorization", "version"} {
			if !seen[kind] {
				return fmt.Errorf("destination %s lacks a %s check", d.Name, kind)
			}
		}
	}
	return nil
}

func (e *engine) publishAttempt(ctx context.Context, s *State) (Result, error) {
	// Preparation may repair source/CI; publication uses a separate fresh session
	// and may only publish the exact plan that passed the gate.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.config.Timeout))
	defer cancel()
	job := s.Job
	usable := (job.Phase == "validating" || job.Phase == "publish") && !job.NeedsPreparation && e.validatePlan(job.Plan) == nil
	if usable {
		if err := e.checkPreparedTree(ctx, job.Plan.Commit); err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			usable = false
			job.BuildChecks = nil
			e.log.Info("Preparation checkpoint invalidated", "reason", err)
		}
	}
	if !usable {
		if job.Plan != nil && e.checkPreparedTree(ctx, job.Plan.Commit) != nil {
			job.BuildChecks = nil
		}
		job.Phase = "preflight"
		job.NeedsPreparation = true
		job.ValidatedAt = time.Time{}
		// Preserve the prior plan and failure as focused repair context.
		if err := writeState(e.config, s); err != nil {
			return Result{}, err
		}
		prepared, err := e.agent.Execute(ctx, releasePrompt(e.config, s))
		if err != nil {
			return Result{}, fmt.Errorf("preflight agent: %w", err)
		}
		if prepared.Status != "ready" {
			return Result{}, errors.New("preflight requires status ready; publication is not authorized in this phase")
		}
		if err := e.validatePlan(prepared.Plan); err != nil {
			return Result{}, err
		}
		job.Plan = prepared.Plan
		job.NeedsPreparation = false
	} else {
		e.log.Info("Resuming prepared release", "commit", job.Plan.Commit, "tag", job.Plan.Tag)
	}
	s.Job.Phase = "validating"
	job.ValidatedAt = time.Time{}
	if err := writeState(e.config, s); err != nil {
		return Result{}, err
	}
	if err := e.validatePublicationCheckpoint(ctx, s); err != nil {
		if ctx.Err() == nil {
			job.NeedsPreparation = true
			if saveErr := writeState(e.config, s); saveErr != nil {
				return Result{}, saveErr
			}
		}
		return Result{}, fmt.Errorf("publishability gate: %w", err)
	}
	s.Job.Phase = "publish"
	job.ValidatedAt = e.now().UTC()
	if err := writeState(e.config, s); err != nil {
		return Result{}, err
	}
	e.log.Info("publishability verified", "commit", job.Plan.Commit, "tag", job.Plan.Tag, "destinations", len(job.Plan.Destinations))
	result, err := e.agent.Execute(ctx, releasePrompt(e.config, s))
	var blocked *blockedResultError
	if errors.As(err, &blocked) || (err == nil && result.Status != "released") {
		job.NeedsPreparation = true
		if saveErr := writeState(e.config, s); saveErr != nil {
			return Result{}, saveErr
		}
	}
	if err != nil {
		return Result{}, err
	}
	if result.Commit != job.Plan.Commit || result.Tag != job.Plan.Tag {
		job.NeedsPreparation = true
		if err := writeState(e.config, s); err != nil {
			return Result{}, err
		}
		return Result{}, errors.New("publication differs from the validated commit/tag; a new preflight is required")
	}
	result.Plan = job.Plan
	return result, nil
}
func (e *engine) validatePlan(plan *PublicationPlan) error {
	if err := plan.validate(); err != nil {
		return err
	}
	if e.config.GitHubRepo() != "" && len(e.config.GitHub.Workflows) == 0 && len(plan.GitHubWorkflows) == 0 {
		return errors.New("preflight must discover the GitHub workflows required for this release and include github_workflows in the plan")
	}
	for _, workflow := range plan.GitHubWorkflows {
		if strings.TrimSpace(workflow) == "" {
			return errors.New("discovered workflow names must not be empty")
		}
	}
	return nil
}

// Direct validation has no persisted cache; the daemon uses the checkpoint path.
func (e *engine) validatePublication(ctx context.Context, target string, plan *PublicationPlan) error {
	return e.validatePublicationChecks(ctx, target, plan, nil)
}
func (e *engine) validatePublicationCheckpoint(ctx context.Context, s *State) error {
	s.Job.ValidatedAt = time.Time{}
	return e.validatePublicationChecks(ctx, s.Job.Target, s.Job.Plan, s)
}
func (e *engine) validatePublicationChecks(ctx context.Context, target string, plan *PublicationPlan, state *State) error {
	if err := plan.validate(); err != nil {
		return err
	}
	if _, err := e.git.git(ctx, "check-ref-format", "refs/tags/"+plan.Tag); err != nil {
		return err
	}
	if err := e.git.contains(ctx, plan.Commit, target); err != nil {
		return err
	}
	if err := e.checkPreparedTree(ctx, plan.Commit); err != nil {
		return err
	}
	encoded, _ := json.Marshal(plan)
	env := map[string]string{"RELEASE_COMMIT": plan.Commit, "RELEASE_TAG": plan.Tag, "RELEASE_TARGET": target, "RELEASE_PLAN_JSON": string(encoded)}
	for destinationIndex, destination := range plan.Destinations {
		env["RELEASE_DESTINATION"] = destination.Name
		for index, check := range destination.Checks {
			// Bind cached build success to the full plan, target and check position.
			// Authorization, version and unknown check kinds are always live.
			key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%d\n%d", encoded, target, destinationIndex, index))))
			if state != nil && check.Kind == "build" && state.Job.BuildChecks[key] {
				e.log.Info("Reusing successful build check", "destination", destination.Name, "commit", plan.Commit)
				continue
			}
			e.log.Info("checking publishability", "destination", destination.Name, "check", check.Kind, "environment", destination.Environment)
			if _, err := osrun.Run(ctx, e.config.Directory, env, check.Command...); err != nil {
				return fmt.Errorf("%s %s: %w", destination.Name, check.Kind, err)
			}
			if err := e.checkPreparedTree(ctx, plan.Commit); err != nil {
				if state != nil {
					state.Job.BuildChecks = nil
				}
				return err
			}
			if state != nil && check.Kind == "build" {
				if state.Job.BuildChecks == nil {
					state.Job.BuildChecks = make(map[string]bool)
				}
				state.Job.BuildChecks[key] = true
				if err := writeState(e.config, state); err != nil {
					return err
				}
			}
		}
	}
	delete(env, "RELEASE_DESTINATION")
	if len(e.config.Preflight) > 0 {
		if _, err := osrun.Run(ctx, e.config.Directory, env, e.config.Preflight...); err != nil {
			return fmt.Errorf("operator preflight: %w", err)
		}
	}
	// A package/build probe must not change the commit or leave tracked edits.
	return e.checkPreparedTree(ctx, plan.Commit)
}
func (e *engine) checkPreparedTree(ctx context.Context, commit string) error {
	head, err := e.git.resolve(ctx, "HEAD")
	if err != nil {
		return err
	}
	if head != commit {
		return errors.New("checkout changed from the prepared commit")
	}
	status, err := e.git.git(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("preflight requires a clean checkout; commit preparation fixes and ignore build outputs appropriately")
	}
	return nil
}

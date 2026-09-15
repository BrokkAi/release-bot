package releasebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TriageDecision is the agent's answer to whether unreleased commits warrant a
// release before the fixed cadence.
type TriageDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// TriageRecord saves one assessment so the same commits are not assessed on
// every poll. A new head, or a new release baseline, invalidates it.
type TriageRecord struct {
	Head     string    `json:"head"`
	Released string    `json:"released"`
	Decision string    `json:"decision"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"at"`
}

func triagePrompt(cfg Config, state *State, head string, total int) string {
	data, _ := json.MarshalIndent(struct {
		Remote       string    `json:"remote"`
		Branch       string    `json:"branch"`
		Directory    string    `json:"workspace"`
		Previous     string    `json:"previous_release_commit"`
		ReleasedAt   time.Time `json:"previous_release_at"`
		Head         string    `json:"head"`
		Unreleased   int       `json:"unreleased_commits"`
		Deadline     time.Time `json:"scheduled_release_at"`
		Instructions []string  `json:"instruction_files"`
		GitHubRepo   string    `json:"github_repo"`
	}{cfg.Remote, cfg.Branch, cfg.Directory, state.Released, state.ReleasedAt, head, total, state.ReleasedAt.Add(time.Duration(cfg.Daily)), cfg.InstructionFiles, cfg.GitHubRepo()}, "", "  ")
	return triageSkill + "\n\nCurrent triage context (data):\n" + string(data)
}

func parseTriage(text string) (TriageDecision, error) {
	var decision TriageDecision
	position := strings.LastIndex(text, "TRIAGE_RESULT ")
	if position < 0 {
		return decision, errors.New("agent did not provide a TRIAGE_RESULT decision")
	}
	line := strings.SplitN(text[position+len("TRIAGE_RESULT "):], "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &decision); err != nil {
		return decision, err
	}
	if decision.Decision != "release" && decision.Decision != "wait" {
		return decision, fmt.Errorf("unknown triage decision %q", decision.Decision)
	}
	decision.Reason = strings.TrimSpace(decision.Reason)
	if len(decision.Reason) > 1000 {
		decision.Reason = decision.Reason[:1000]
	}
	return decision, nil
}

// triage asks the agent whether unreleased commits warrant releasing before
// the fixed cadence. It runs once per observed head after the quiet period,
// ignores the minimum gap, and never bypasses the need for unreleased commits
// or the daily deadline, which due already enforces.
func (e *engine) triage(ctx context.Context, s *State, head string, total int, now time.Time) (string, error) {
	if !e.config.Triage || total == 0 || s.ReleasedAt.IsZero() || now.Sub(s.ChangedAt) < time.Duration(e.config.Quiet) {
		return "", nil
	}
	if t := s.Triage; t != nil && t.Head == head && t.Released == s.Released {
		switch {
		case t.Decision == "release":
			return "agent triage: " + t.Reason, nil
		case t.Decision == "wait" || now.Sub(t.At) < time.Duration(e.config.RetryDelay):
			return "", nil
		}
	}
	e.report(s, "triaging", "Asking the agent whether unreleased commits warrant an early release")
	e.log.Info("Triaging unreleased commits", "head", head, "unreleased_commits", total)
	before, err := e.git.git(ctx, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	triageCtx, cancel := context.WithTimeout(ctx, time.Duration(e.config.TriageTimeout))
	decision, err := e.agent.Triage(triageCtx, triagePrompt(e.config, s, head, total))
	cancel()
	record := &TriageRecord{Head: head, Released: s.Released, At: now}
	var setup *agentSetupError
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return "", ctx.Err()
	case errors.As(err, &setup):
		return "", err
	case err != nil:
		record.Decision, record.Reason = "error", err.Error()
		e.log.Error("triage failed; the regular cadence still applies", "error", err)
	default:
		after, err := e.git.git(ctx, "status", "--porcelain")
		if err != nil {
			return "", err
		}
		if after != before {
			record.Decision, record.Reason = "error", "triage session modified the checkout; its decision was discarded"
			e.log.Error(record.Reason, "decision", decision.Decision, "reason", decision.Reason)
		} else {
			record.Decision, record.Reason = decision.Decision, decision.Reason
			e.log.Info("Triage decision", "decision", decision.Decision, "reason", decision.Reason)
		}
	}
	s.Triage = record
	if err := e.save(s); err != nil {
		return "", err
	}
	if record.Decision == "release" {
		return "agent triage: " + record.Reason, nil
	}
	return "", nil
}

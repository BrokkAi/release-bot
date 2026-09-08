package releasebot

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Progress is an owned snapshot for a live display. Releases contains the last
// verified receipt and the pending job, when present. An empty Phase updates
// saved results without changing the current activity.
type Progress struct {
	Phase, Task, Commit  string
	Attempt, MaxAttempts int
	WakeAt               time.Time
	Failure              string
	LastTag              string
	Unreleased, Recent   int
	ChangesKnown         bool
	Releases             []ReleaseProgress
}

type ReleaseProgress struct {
	ID, Title, Status, URL, Body string
}

type progressKey struct{}

// WithProgress observes progress synchronously. The callback should return
// promptly and must not perform actions on the repository.
func WithProgress(ctx context.Context, observe func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, observe)
}

func (e *engine) save(s *State) error {
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.report(s, "", "")
	return nil
}

func (e *engine) report(s *State, phase, task string) {
	if e.observe == nil {
		return
	}
	p := Progress{Phase: phase, Task: task, MaxAttempts: e.config.Attempts,
		Unreleased: e.unreleased, Recent: e.recent, ChangesKnown: e.changesKnown}
	if phase == "waiting" || phase == "paused" {
		p.WakeAt = e.now().Add(time.Duration(e.config.Poll))
	}
	if s != nil {
		p.Commit = s.Observed
		if phase == "complete" {
			p.Commit = s.Released
		}
		if r := s.LastResult; r != nil {
			p.LastTag = r.Tag
			p.Releases = append(p.Releases, ReleaseProgress{ID: "release:" + r.Tag, Title: r.Tag, Status: "released", URL: r.URL, Body: releaseDetails(r.Commit, r.Detail, r.Plan)})
		}
		if j := s.Job; j != nil {
			p.Commit, p.Attempt = j.Target, j.Tries
			p.Failure = strings.Join(nonempty(j.Failure, j.SetupFailure, j.Interruption), "\n")
			status := j.Phase
			if status == "" {
				status = "pending"
			}
			if j.Tries >= e.config.Attempts && j.Failure != "" || j.SetupFailure != "" {
				status = "blocked"
			}
			title := "Pending release"
			if j.Plan != nil {
				title = j.Plan.Tag
			}
			body := releaseDetails(j.Target, p.Failure, j.Plan)
			body += fmt.Sprintf("\n\nWork branch\n%s\n\nAttempts\n%d/%d", j.WorkBranch, j.Tries, e.config.Attempts)
			if !j.RetryAt.IsZero() {
				body += "\n\nRetry eligible after\n" + j.RetryAt.Local().Format(time.RFC3339)
			}
			var receiptURL string
			if j.Candidate != nil {
				receiptURL = j.Candidate.URL
				body += "\n\nPublication receipt (awaiting verification)\n" + j.Candidate.Detail
			}
			p.Releases = append(p.Releases, ReleaseProgress{ID: "job:" + j.WorkBranch + ":" + j.Target, Title: title, Status: status, URL: receiptURL, Body: body})
		}
	}
	e.observe(p)
}

func nonempty(values ...string) []string {
	var result []string
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func releaseDetails(commit, detail string, plan *PublicationPlan) string {
	body := "Commit\n" + commit
	if detail != "" {
		body += "\n\nDetails\n" + detail
	}
	if plan != nil {
		body += "\n\nPrepared commit\n" + plan.Commit + "\n\nTag\n" + plan.Tag + "\n\nDestinations"
		for _, d := range plan.Destinations {
			body += fmt.Sprintf("\n%s · %s · %s\nEnvironment: %s", d.Name, d.Kind, d.Version, d.Environment)
			for _, check := range d.Checks {
				body += "\n" + check.Kind + ": " + check.Evidence
			}
		}
		body += "\n\nWorkflows\n" + strings.Join(plan.GitHubWorkflows, "\n")
	}
	return body
}

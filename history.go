package releasebot

import (
	"slices"
	"sort"
	"time"
)

// HistoryLimit bounds the private, local verified receipt collection.
const HistoryLimit = 100

// VerifiedReceipt records a past daemon verification, not current publication
// health. Result.Detail and the plan's check evidence are agent-supplied text.
type VerifiedReceipt struct {
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	Result     Result     `json:"result"`
}

// VerifiedHistory returns owned receipts oldest first (unknown times first),
// with tag and commit breaking timestamp ties. Reading never migrates the file.
func (s *State) VerifiedHistory() []VerifiedReceipt {
	receipts := []VerifiedReceipt{}
	if s == nil {
		return receipts
	}
	receipts = append(receipts, s.History...)
	if len(receipts) == 0 && s.LastResult != nil && s.LastResult.Status == "released" {
		var at *time.Time
		if !s.ReleasedAt.IsZero() {
			t := s.ReleasedAt
			at = &t
		}
		receipts = append(receipts, VerifiedReceipt{VerifiedAt: at, Result: *s.LastResult})
	}
	// Copy nested plans as well, so consumers cannot change saved evidence.
	for i := range receipts {
		r := &receipts[i]
		if r.VerifiedAt != nil {
			at := *r.VerifiedAt
			r.VerifiedAt = &at
		}
		if r.Result.Plan != nil {
			plan := *r.Result.Plan
			plan.GitHubWorkflows = slices.Clone(plan.GitHubWorkflows)
			plan.Destinations = slices.Clone(plan.Destinations)
			for j := range plan.Destinations {
				d := &plan.Destinations[j]
				d.Verify = slices.Clone(d.Verify)
				d.Checks = slices.Clone(d.Checks)
				for k := range d.Checks {
					d.Checks[k].Command = slices.Clone(d.Checks[k].Command)
				}
			}
			r.Result.Plan = &plan
		}
	}
	sort.SliceStable(receipts, func(i, j int) bool {
		a, b := receipts[i], receipts[j]
		var at, bt time.Time
		if a.VerifiedAt != nil {
			at = *a.VerifiedAt
		}
		if b.VerifiedAt != nil {
			bt = *b.VerifiedAt
		}
		if !at.Equal(bt) {
			return at.Before(bt)
		}
		if a.Result.Tag != b.Result.Tag {
			return a.Result.Tag < b.Result.Tag
		}
		return a.Result.Commit < b.Result.Commit
	})
	seen := map[[2]string]bool{}
	unique := receipts[:0]
	for _, r := range receipts {
		key := [2]string{r.Result.Tag, r.Result.Commit}
		if !seen[key] {
			unique = append(unique, r)
			seen[key] = true
		}
	}
	if len(unique) > HistoryLimit {
		unique = unique[len(unique)-HistoryLimit:]
	}
	return unique
}

func (s *State) recordVerified(r Result, at time.Time) {
	s.History = s.VerifiedHistory() // Preserve the available legacy receipt first.
	for _, receipt := range s.History {
		if receipt.Result.Tag == r.Tag && receipt.Result.Commit == r.Commit {
			return
		}
	}
	s.History = append(s.History, VerifiedReceipt{VerifiedAt: &at, Result: r})
	s.History = s.VerifiedHistory()
}
